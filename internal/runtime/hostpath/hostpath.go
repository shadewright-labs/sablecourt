// Package hostpath resolves the host paths the daemon opens and the paths the
// checkout guard compares, with one rule, so both name the same file.
//
// Paths are never cleaned lexically before they are resolved: in link/../name
// the kernel follows link before "..", so a cleaned spelling can name a
// different file from the one opened.
package hostpath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ResolveFile turns a configured daemon file path (the database or the secret
// file) into the one path the daemon opens. A relative path is joined to the
// working directory without cleaning. The parent directory must exist and is
// resolved with its symlinks. The final component must name a file, not "."
// or "..", and must not be a symlink, dangling or not: a symlink could be
// pointed elsewhere after the guard has compared it. An existing final
// component must be a regular file. An empty path stays empty.
func ResolveFile(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	path := raw
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd + string(filepath.Separator) + path
	}
	parent, base, err := split(path)
	if err != nil {
		return "", fmt.Errorf("%q: %w", raw, err)
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		return "", fmt.Errorf("%q is a symlink; name the file itself", raw)
	case err == nil && !info.Mode().IsRegular():
		return "", fmt.Errorf("%q is not a regular file", raw)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	dir, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve the directory of %q: %w", raw, err)
	}
	return filepath.Join(dir, base), nil
}

// Resolve returns an absolute path with every symlink resolved, for the
// checkout guard. An existing path is resolved whole. A path that does not
// exist yet resolves to its resolved parent directory plus its final
// component; the parent must exist, and a dangling symlink is refused because
// its target is not known until it is created. Anything that cannot be fully
// resolved is an error, so the guard fails closed.
func Resolve(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q is not absolute", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%q is a dangling symlink", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	parent, base, err := split(path)
	if err != nil {
		return "", fmt.Errorf("%q: %w", path, err)
	}
	dir, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve the directory of %q: %w", path, err)
	}
	return filepath.Join(dir, base), nil
}

// CreateFile creates a resolved daemon file that does not exist yet, with
// mode 0600 and the given content. O_EXCL refuses any existing entry,
// including a symlink planted at the path, so the file is created exactly at
// path. An existing regular file is left alone; anything else is refused.
func CreateFile(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path comes from ResolveFile.
	if errors.Is(err, fs.ErrExist) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%q is not a regular file", path)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// split cuts path at its last separator without cleaning it.
func split(path string) (parent, base string, err error) {
	i := strings.LastIndex(path, string(filepath.Separator))
	parent, base = path[:i], path[i+1:]
	if parent == "" {
		parent = string(filepath.Separator)
	}
	if base == "" || base == "." || base == ".." {
		return "", "", errors.New("does not name a file")
	}
	return parent, base, nil
}
