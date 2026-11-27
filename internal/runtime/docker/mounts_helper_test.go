package docker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/runtime/mountguard"
)

// withWorktreeRoot opens a worktree root in a temporary directory, gives the
// adapter the mount guard for it, and returns the root's canonical path.
func withWorktreeRoot(t *testing.T, a *Adapter, protected ...string) string {
	t.Helper()
	r, err := mountguard.OpenRoot(filepath.Join(t.TempDir(), "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	a.Mounts = &mountguard.Guard{Root: r, Protected: protected}
	return r.Path()
}

// mkdirAll creates a directory and returns its path.
func mkdirAll(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// isolateHome points HOME and the state directory at temporary directories,
// so a test never depends on the files of the user running it.
func isolateHome(t *testing.T) (home, state string) {
	t.Helper()
	home = t.TempDir()
	state = filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("SABLECOURT_STATE_DIR", state)
	t.Setenv("DOCKER_HOST", "")
	return home, state
}

// liveDir is a directory outside the worktree root for a live checkout.
func liveDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// openRootAt opens dir as the worktree root and gives the adapter its guard.
func openRootAt(t *testing.T, a *Adapter, dir string, protected ...string) {
	t.Helper()
	r, err := mountguard.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	a.Mounts = &mountguard.Guard{Root: r, Protected: protected}
}
