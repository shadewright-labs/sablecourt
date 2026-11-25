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
	"sync"
	"syscall"
)

const (
	Filename     = "operator.token"
	LockFilename = "daemon.lock"
	prefix       = "op_"
	size         = len(prefix) + 64
)

type stateLock struct {
	once sync.Once
	file *os.File
	root *os.Root
	err  error
}

func (l *stateLock) Close() error {
	l.once.Do(func() {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		l.err = errors.Join(l.file.Close(), l.root.Close())
	})
	return l.err
}

// LockStateDir acquires an exclusive non-blocking lock on the state directory for a running daemon.
func LockStateDir(dir string) (io.Closer, error) {
	safeDir, err := ensureStateDir(dir)
	if err != nil {
		return nil, err
	}
	root, err := openState(safeDir)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(LockFilename, os.O_CREATE|os.O_RDWR|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCaller(info) {
		_ = f.Close()
		_ = root.Close()
		return nil, errors.New("operator state lock must be a regular file owned by the current user")
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = root.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = root.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.New("operator state directory is already locked by another daemon")
		}
		return nil, fmt.Errorf("lock operator state directory: %w", err)
	}
	return &stateLock{file: f, root: root}, nil
}

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
	safeDir, err := ensureStateDir(dir)
	if err != nil {
		return "", err
	}
	root, err := openState(safeDir)
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
	safeDir, err := resolveSafeStateDir(dir, false)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(safeDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode() != os.ModeDir|0o700 || !ownedByCaller(info) {
		return nil, errors.New("operator state directory must be owned by the current user with mode 0700")
	}
	root, err := os.OpenRoot(safeDir)
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

// ensureStateDir creates any missing part of the state directory through a handle on the
// deepest verified ancestor. A symlink raced into a shared parent such as a sticky 1777
// directory cannot redirect creation, because the handle refuses to leave the ancestor.
func ensureStateDir(dir string) (string, error) {
	ancestor, rel, err := resolveStateDir(dir, true)
	if err != nil {
		return "", err
	}
	if rel == "" {
		return ancestor, nil
	}
	if err := mkdirBelow(ancestor, rel); err != nil {
		return "", err
	}
	return filepath.Join(ancestor, rel), nil
}

func mkdirBelow(ancestor, rel string) error {
	info, err := os.Lstat(ancestor)
	if err != nil {
		return err
	}
	if !safeAncestor(info) {
		return errors.New("operator state parent directory has unsafe ownership or permissions")
	}
	root, err := os.OpenRoot(ancestor)
	if err != nil {
		return err
	}
	defer root.Close()
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) || !safeAncestor(actual) {
		return errors.New("operator state parent directory changed while opening it")
	}
	return root.MkdirAll(rel, 0o700)
}

func resolveSafeStateDir(dir string, allowMissing bool) (string, error) {
	ancestor, rel, err := resolveStateDir(dir, allowMissing)
	if err != nil {
		return "", err
	}
	return filepath.Join(ancestor, rel), nil
}

// resolveStateDir returns the deepest existing verified directory and the missing suffix below it.
func resolveStateDir(dir string, allowMissing bool) (string, string, error) {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return "", "", errors.New("operator state directory must be an absolute path")
	}
	rootInfo, err := os.Lstat(string(filepath.Separator))
	if err != nil {
		return "", "", err
	}
	if !safeAncestor(rootInfo) {
		return "", "", errors.New("operator state root directory has unsafe ownership or permissions")
	}
	rel := strings.TrimPrefix(clean, string(filepath.Separator))
	if rel == "" {
		return "", "", errors.New("operator state directory must be owned by the current user with mode 0700")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := string(filepath.Separator)
	symlinks := 0
	for i := 0; i < len(parts); i++ {
		isLeaf := i == len(parts)-1
		next := filepath.Join(cur, parts[i])
		info, err := os.Lstat(next)
		if err != nil {
			if allowMissing && errors.Is(err, os.ErrNotExist) {
				return cur, filepath.Join(parts[i:]...), nil
			}
			return "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if isLeaf {
				return "", "", errors.New("operator state directory must not be a symlink")
			}
			// Allow only root-owned OS directory symlinks (such as /var -> private/var on macOS).
			if !ownedByRoot(info) || (os.Geteuid() == 0 && cur != string(filepath.Separator)) {
				return "", "", errors.New("operator state path must not traverse a symlink")
			}
			symlinks++
			if symlinks > 8 {
				return "", "", errors.New("operator state path traverses too many symlinks")
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(cur, target)
			}
			target = filepath.Clean(target)
			var nextParts []string
			if trimmed := strings.TrimPrefix(target, string(filepath.Separator)); trimmed != "" {
				nextParts = strings.Split(trimmed, string(filepath.Separator))
			}
			parts = append(nextParts, parts[i+1:]...)
			cur = string(filepath.Separator)
			i = -1
			continue
		}
		if !info.IsDir() {
			return "", "", errors.New("operator state path component is not a directory")
		}
		if isLeaf {
			if info.Mode() != os.ModeDir|0o700 || !ownedByCaller(info) {
				return "", "", errors.New("operator state directory must be owned by the current user with mode 0700")
			}
		} else if !safeAncestor(info) {
			return "", "", errors.New("operator state parent directory has unsafe ownership or permissions")
		}
		cur = next
	}
	return cur, "", nil
}

func safeAncestor(info os.FileInfo) bool {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if !ownedByCaller(info) && !ownedByRoot(info) {
		return false
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return false
	}
	return true
}

func ownedByRoot(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
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
	return ok && stat.Uid == uint32(os.Geteuid()) // #nosec G115 -- os.Geteuid returns the kernel uid_t. Converting back to its 32-bit representation preserves every UID bit.
}
