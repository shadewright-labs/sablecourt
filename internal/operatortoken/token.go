// Package operatortoken manages the owner-only credential shared by the daemon and CLI.
package operatortoken

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	Filename = "operator.token"
	prefix   = "op_"
	size     = len(prefix) + 64
)

// StateDir returns the state directory shared by the daemon and CLI.
func StateDir() (string, error) {
	if dir := os.Getenv("SABLECOURT_STATE_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", errors.New("SABLECOURT_STATE_DIR must be an absolute path")
		}
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "sablecourt"), nil
}

// Read opens an existing credential without creating or repairing files.
func Read(dir string) (string, error) {
	root, err := openState(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	return readToken(root)
}

// LoadOrCreate publishes a new credential only after the complete file is ready.
func LoadOrCreate(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	root, err := openState(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	token, err := readToken(root)
	if !errors.Is(err, os.ErrNotExist) {
		return token, err
	}

	// The directory lock serializes local first-start writes across daemon processes.
	lock, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("lock operator state: %w", err)
	}
	token, err = readToken(root)
	if !errors.Is(err, os.ErrNotExist) {
		return token, err
	}

	const pending = Filename + ".pending"
	if err := root.Remove(pending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var raw [32]byte
	rand.Read(raw[:])
	token = prefix + hex.EncodeToString(raw[:])
	f, err := root.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer root.Remove(pending)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return "", err
	}
	if _, err := io.WriteString(f, token+"\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	syncErr := f.Sync()
	if err := errors.Join(syncErr, f.Close()); err != nil {
		return "", err
	}
	if err := root.Rename(pending, Filename); err != nil {
		return "", err
	}
	if err := lock.Sync(); err != nil {
		return "", err
	}
	return token, nil
}

func openState(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode() != os.ModeDir|0o700 || !ownedByCaller(info) {
		return nil, errors.New("operator state directory must be owned by the current user with mode 0700")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) || actual.Mode() != info.Mode() || !ownedByCaller(actual) {
		_ = root.Close()
		return nil, errors.New("operator state directory changed while opening it")
	}
	return root, nil
}

func readToken(root *os.Root) (string, error) {
	info, err := root.Lstat(Filename)
	if err != nil {
		return "", err
	}
	if info.Mode() != 0o600 || !ownedByCaller(info) {
		return "", errors.New("operator token must be a regular file owned by the current user with mode 0600")
	}
	f, err := root.OpenFile(Filename, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, actual) || actual.Mode() != info.Mode() || !ownedByCaller(actual) {
		return "", errors.New("operator token changed while opening it")
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(size+2)))
	if err != nil {
		return "", err
	}
	token := strings.TrimSuffix(string(raw), "\n")
	if len(token) != size || !strings.HasPrefix(token, prefix) {
		return "", errors.New("invalid operator token file")
	}
	if _, err := hex.DecodeString(token[len(prefix):]); err != nil {
		return "", errors.New("invalid operator token file")
	}
	return token, nil
}

func ownedByCaller(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
