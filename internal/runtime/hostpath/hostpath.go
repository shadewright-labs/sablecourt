// Package hostpath resolves and creates the daemon's own files (the database
// and the secret file). Which directories a harness container may mount is
// decided by package mountguard, by handle and file identity, not here.
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

// CreateFile creates a resolved daemon file that does not exist yet, with
// mode 0600 and the given content. O_EXCL refuses any existing entry,
// including a symlink planted at the path, so the file is created exactly at
// path. An existing regular file is left alone; anything else is refused.
// It returns the identity of the file, for a later os.SameFile check that
// whatever opens the path opened this file.
func CreateFile(path string, content []byte) (fs.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path comes from ResolveFile.
	if errors.Is(err, fs.ErrExist) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%q is not a regular file", path)
		}
		return info, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		_, err = f.Write(content)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return info, f.Close()
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
