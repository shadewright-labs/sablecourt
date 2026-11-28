package coding

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// harnessGitDirFixture makes a repository with a linked worktree and returns
// the worktree, its checkout state and a task base inside a private
// directory, as the worktree root is.
func harnessGitDirFixture(t *testing.T) (string, *worktreeState, string) {
	t.Helper()
	repo := initTempGitRepo(t)
	work := filepath.Join(t.TempDir(), "w")
	testGit(t, repo, "worktree", "add", "-q", work)
	gitDir := strings.TrimSpace(testGit(t, work, "rev-parse", "--absolute-git-dir"))
	state := &worktreeState{
		TaskID:    "task-gitdir",
		Mode:      checkoutModeRef,
		CommonDir: filepath.Join(repo, ".git"),
		GitDir:    gitDir,
	}
	root := filepath.Join(t.TempDir(), "worktrees")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return work, state, filepath.Join(root, "task-gitdir")
}

func harnessGitStatus(t *testing.T, gitDir, work string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+gitDir, "--work-tree="+work, "status", "--porcelain")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestHarnessGitDirCopiesSharedIndex checks that a checkout with a split
// index gets the shared index file its index names, so Git in the container
// can read the index.
func TestHarnessGitDirCopiesSharedIndex(t *testing.T) {
	work, state, taskBase := harnessGitDirFixture(t)
	testGit(t, work, "config", "core.splitIndex", "true")
	testGit(t, work, "update-index", "--split-index")
	testWrite(t, work, "added.txt", "x\n", 0o600)
	testGit(t, work, "add", "added.txt")

	gitDir, err := prepareHarnessGitDir(context.Background(), work, taskBase, state, "")
	if err != nil {
		t.Fatalf("prepareHarnessGitDir: %v", err)
	}
	out, err := harnessGitStatus(t, gitDir, work)
	if err != nil {
		t.Fatalf("git status in the launch's Git directory: %v (%s)", err, out)
	}
	if !strings.Contains(out, "A  added.txt") {
		t.Fatalf("staged file missing from status: %q", out)
	}
}

// TestHarnessGitDirReadableByHarnessUser checks that the launch's Git
// directory can be read by a container user other than the daemon user
// (uid 10001 on native Linux Docker), while its parent stays private, so
// no other host user can reach it.
func TestHarnessGitDirReadableByHarnessUser(t *testing.T) {
	work, state, taskBase := harnessGitDirFixture(t)
	gitDir, err := prepareHarnessGitDir(context.Background(), work, taskBase, state, "")
	if err != nil {
		t.Fatalf("prepareHarnessGitDir: %v", err)
	}
	err = filepath.WalkDir(gitDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o044)
		if d.IsDir() {
			want = 0o055
		}
		if info.Mode().Perm()&want != want {
			t.Errorf("%s has mode %v, not readable by the harness user", path, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.Stat(filepath.Dir(gitDir))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm()&0o077 != 0 {
		t.Fatalf("parent of the launch's Git directory has mode %v", parent.Mode().Perm())
	}
}

// TestHarnessGitDirRefusesSharedParent checks that the daemon does not open
// the launch's Git directory to the harness user when another host user
// could reach it through its parent, and leaves no directory behind.
func TestHarnessGitDirRefusesSharedParent(t *testing.T) {
	for _, mode := range []fs.FileMode{0o750, 0o705, 0o711} {
		t.Run(mode.String(), func(t *testing.T) {
			work, state, taskBase := harnessGitDirFixture(t)
			if err := os.Chmod(filepath.Dir(taskBase), mode); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareHarnessGitDir(context.Background(), work, taskBase, state, ""); err == nil {
				t.Fatal("launch Git directory prepared under a parent other users can reach")
			}
			if _, err := os.Lstat(harnessGitDirPath(taskBase)); !os.IsNotExist(err) {
				t.Fatalf("launch Git directory left behind: %v", err)
			}
		})
	}
}

// TestHarnessGitDirRefusesSharedIndexSymlink checks that the shared index
// gets the same regular-file check as the index: a symlink is refused.
func TestHarnessGitDirRefusesSharedIndexSymlink(t *testing.T) {
	work, state, taskBase := harnessGitDirFixture(t)
	testGit(t, work, "config", "core.splitIndex", "true")
	testGit(t, work, "update-index", "--split-index")
	out := strings.TrimSpace(testGit(t, work, "rev-parse", "--shared-index-path"))
	shared := filepath.Join(state.GitDir, filepath.Base(out))
	moved := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(shared, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareHarnessGitDir(context.Background(), work, taskBase, state, ""); err == nil {
		t.Fatal("shared index symlink accepted")
	}
	if _, err := os.Lstat(harnessGitDirPath(taskBase)); !os.IsNotExist(err) {
		t.Fatalf("launch Git directory left behind: %v", err)
	}
}
