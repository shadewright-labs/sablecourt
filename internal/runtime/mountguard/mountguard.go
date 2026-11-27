// Package mountguard decides which host directories the daemon may bind mount
// into a harness container. It is the only place that rule lives.
//
// The rule does not compare path spellings. A checkout the daemon made (a task
// worktree, a council ref, review or writer checkout, and a launch's own Git
// metadata) is accepted only when it is reached through the worktree root,
// opened once at startup as an os.Root, with no symlink component; Docker gets
// the root's canonical path plus those verified components. The one checkout
// an operator names, the live repository, is compared by device and inode
// with each protected path and every ancestor of it, reached with "..".
//
// Stated limits: someone who already has the daemon user's rights can rename
// directories it owns between the check and container create, can plant a
// hard link to a protected file inside the live repository, and an operator
// can place these paths in a directory another user can write.
package mountguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
)

const sep = string(filepath.Separator)

// maxDepth bounds the walk up a directory tree with "..".
const maxDepth = 256

// ErrUnsafe is returned for a bind source the rule refuses.
var ErrUnsafe = errors.New("unsafe checkout directory")

// homeCredentialPaths are directories and files under the daemon user's home
// that hold credentials. A live checkout may not contain or lie under them.
var homeCredentialPaths = [...]string{
	".ssh",
	".gnupg",
	".aws",
	".kube",
	".docker",
	".config",
	".azure",
	".gcloud",
	".gcp",
	".claude",
	".codex",
	".gemini",
	".agy",
	".netrc",
	".npmrc",
	".pypirc",
	".git-credentials",
	filepath.Join(".local", "share", "sablecourt"),
	filepath.Join("Library", "Keychains"),
	filepath.Join("Library", "Application Support"),
}

// Root is the daemon's worktree root, held open as an os.Root.
type Root struct {
	path string
	root *os.Root
}

// OpenRoot creates the worktree root if needed and opens it. Path returns the
// canonical spelling found at open time; every use checks that the spelling
// still names the opened directory.
func OpenRoot(dir string) (*Root, error) {
	if dir == "" {
		return nil, errors.New("worktree root is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create worktree root: %w", err)
	}
	path, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	h, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open worktree root: %w", err)
	}
	r := &Root{path: path, root: h}
	if err := r.verify(); err != nil {
		_ = h.Close()
		return nil, err
	}
	return r, nil
}

// Path is the canonical path of the worktree root.
func (r *Root) Path() string { return r.path }

// Close releases the handle.
func (r *Root) Close() error { return r.root.Close() }

// verify checks that the canonical path still names the opened directory.
func (r *Root) verify() error {
	held, err := r.root.Stat(".")
	if err != nil {
		return err
	}
	named, err := os.Lstat(r.path)
	if err != nil || !held.IsDir() || !os.SameFile(held, named) {
		return errors.New("worktree root changed after it was opened")
	}
	return nil
}

// Source returns the bind source for a directory the daemon made under the
// root: the root's canonical path plus components that are each a directory
// and not a symlink, checked through the handle. A path that does not start
// with the canonical path, or has an empty, "." or ".." component, is refused.
func (r *Root) Source(path string) (string, error) {
	comps, err := r.components(path)
	if err != nil {
		return "", err
	}
	if err := r.verify(); err != nil {
		return "", err
	}
	for i := range comps {
		info, err := r.root.Lstat(strings.Join(comps[:i+1], sep))
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrUnsafe, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%w: a component is a symlink or not a directory", ErrUnsafe)
		}
	}
	return r.path + sep + strings.Join(comps, sep), nil
}

// Lstat reports on a path under the root through the handle.
func (r *Root) Lstat(path string) (fs.FileInfo, error) {
	comps, err := r.components(path)
	if err != nil {
		return nil, err
	}
	return r.root.Lstat(strings.Join(comps, sep))
}

// Remove removes one file under the root through the handle. A missing file
// is not an error.
func (r *Root) Remove(path string) error {
	comps, err := r.components(path)
	if err != nil {
		return err
	}
	if err := r.root.Remove(strings.Join(comps, sep)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveAll removes a path under the root and everything below it through the
// handle, without following symlinks.
func (r *Root) RemoveAll(path string) error {
	comps, err := r.components(path)
	if err != nil {
		return err
	}
	return r.root.RemoveAll(strings.Join(comps, sep))
}

func (r *Root) components(path string) ([]string, error) {
	rest, ok := strings.CutPrefix(path, r.path+sep)
	if !ok {
		return nil, fmt.Errorf("%w: not under the worktree root", ErrUnsafe)
	}
	comps := strings.Split(rest, sep)
	for _, c := range comps {
		if c == "" || c == "." || c == ".." {
			return nil, fmt.Errorf("%w: path has an empty or dot component", ErrUnsafe)
		}
	}
	return comps, nil
}

// RefuseInside returns an error if any non-empty path is the worktree root,
// lies under it, or shares identity with it. The daemon calls it at startup
// for the database, the secret file and the state directory.
func (r *Root) RefuseInside(paths ...string) error {
	held, err := r.root.Stat(".")
	if err != nil {
		return err
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		anc, err := ancestry(p)
		if err != nil {
			return fmt.Errorf("check %q against the worktree root: %w", p, err)
		}
		for _, a := range anc {
			if os.SameFile(a, held) {
				return fmt.Errorf("%q lies under the worktree root %q", p, r.path)
			}
		}
	}
	return nil
}

// Guard holds what the rule needs at launch time: the worktree root and the
// daemon's own files and directories (state directory, database, database
// directory, secret file). The state directory, the home directory and the
// home credential paths are added at each check.
type Guard struct {
	Root      *Root
	Protected []string
}

// MountSource returns the path to give the container runtime for a bind
// mount. A live checkout is checked by identity (CheckLive); any other
// checkout must be reached through the worktree root (Root.Source) and is
// also checked against the protected paths and sockets by identity.
func (g *Guard) MountSource(dir string, live bool, sockets []string) (string, error) {
	if live {
		return dir, g.CheckLive(dir, sockets)
	}
	if g == nil || g.Root == nil {
		return "", fmt.Errorf("%w: no worktree root is configured", ErrUnsafe)
	}
	src, err := g.Root.Source(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsafe, err)
	}
	protected, _, err := g.paths(sockets)
	if err != nil {
		return "", err
	}
	if err := refuseContaining(info, protected); err != nil {
		return "", err
	}
	return src, nil
}

// CheckLive applies the live checkout rule: dir must be an absolute path to a
// directory that is not a symlink and not the file system root; it must not
// be, contain or share identity with any protected path, socket, the state
// directory, the home directory or a home credential path; and it must not
// lie under the state directory, the worktree root or a home credential path.
// A nil Guard checks only what needs no daemon configuration.
func (g *Guard) CheckLive(dir string, sockets []string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: expected an absolute checkout directory", ErrUnsafe)
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: checkout must be a directory", ErrUnsafe)
	}
	protected, enclosing, err := g.paths(sockets)
	if err != nil {
		return err
	}
	if err := refuseContaining(info, protected); err != nil {
		return err
	}
	anc, err := ancestry(dir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafe, err)
	}
	if len(anc) < 2 {
		return fmt.Errorf("%w: checkout is the file system root", ErrUnsafe)
	}
	for _, p := range enclosing {
		parent, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnsafe, err)
		}
		for _, a := range anc {
			if os.SameFile(a, parent) {
				return fmt.Errorf("%w: checkout lies under a daemon or credential directory", ErrUnsafe)
			}
		}
	}
	return nil
}

// paths lists what no source may contain and what a live checkout may not
// lie under.
func (g *Guard) paths(sockets []string) (protected, enclosing []string, err error) {
	stateDir, err := operatortoken.StateDir()
	if err != nil {
		return nil, nil, errors.New("cannot resolve daemon state directory")
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, nil, errors.New("cannot resolve daemon home directory")
	}
	protected = append(protected, stateDir, home)
	enclosing = append(enclosing, stateDir)
	for _, rel := range homeCredentialPaths {
		p := filepath.Join(home, rel)
		protected = append(protected, p)
		enclosing = append(enclosing, p)
	}
	if g != nil {
		if g.Root != nil {
			protected = append(protected, g.Root.Path())
			enclosing = append(enclosing, g.Root.Path())
		}
		protected = append(protected, g.Protected...)
	}
	return append(protected, sockets...), enclosing, nil
}

// Overlap reports whether two existing directories are the same, or one lies
// under the other, by identity.
func Overlap(a, b string) (bool, error) {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		top, err := os.Stat(pair[1])
		if err != nil {
			return false, err
		}
		anc, err := ancestry(pair[0])
		if err != nil {
			return false, err
		}
		for _, x := range anc {
			if os.SameFile(x, top) {
				return true, nil
			}
		}
	}
	return false, nil
}

// refuseContaining refuses a source that is, or is an ancestor of, any of the
// paths, by identity. A path that cannot be checked counts as contained.
func refuseContaining(src fs.FileInfo, paths []string) error {
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		anc, err := ancestry(p)
		if err != nil {
			return fmt.Errorf("%w: cannot check a protected path: %v", ErrUnsafe, err)
		}
		for _, a := range anc {
			if os.SameFile(a, src) {
				return fmt.Errorf("%w: checkout holds a protected path", ErrUnsafe)
			}
		}
	}
	return nil
}

// ancestry returns the identity of what p names and of every directory above
// it, up to the file system root, reached with ".." so the kernel, not a
// spelling, decides each step. A p that does not exist yet starts from its
// deepest existing ancestor; a "." or ".." past that point, a dangling
// symlink or any other error is refused, since nothing on disk says where it
// leads. For a file, the walk starts from the directory that holds the file
// itself (the target, if p is a symlink).
func ancestry(p string) ([]fs.FileInfo, error) {
	if !filepath.IsAbs(p) {
		return nil, fmt.Errorf("%q is not absolute", p)
	}
	cur := p
	var linfo fs.FileInfo
	for {
		var err error
		linfo, err = os.Lstat(cur)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent, base := split(cur)
		if base == "." || base == ".." {
			return nil, fmt.Errorf("%q has a dot component past what exists", p)
		}
		if parent == cur {
			return nil, fmt.Errorf("%q has no existing ancestor", p)
		}
		cur = parent
	}
	info, err := os.Stat(cur)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", p, err)
	}
	var out []fs.FileInfo
	if !info.IsDir() {
		out = append(out, info)
		if linfo.Mode()&fs.ModeSymlink != 0 {
			if cur, err = filepath.EvalSymlinks(cur); err != nil {
				return nil, err
			}
		}
		cur, _ = split(cur)
		if info, err = os.Stat(cur); err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%q: parent is not a directory", p)
		}
	}
	for range maxDepth {
		out = append(out, info)
		up := cur + sep + ".."
		parent, err := os.Stat(up)
		if err != nil {
			return nil, err
		}
		if os.SameFile(parent, info) {
			return out, nil
		}
		cur, info = up, parent
	}
	return nil, fmt.Errorf("%q is nested too deeply", p)
}

// split cuts path at its last separator without cleaning it.
func split(path string) (parent, base string) {
	i := strings.LastIndex(path, sep)
	if i < 0 {
		return path, ""
	}
	parent, base = path[:i], path[i+1:]
	if parent == "" {
		parent = sep
	}
	return parent, base
}
