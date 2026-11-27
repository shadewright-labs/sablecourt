package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nestedParent returns a resolved parent repository with a submodule "child"
// checked out, ready for inspectNested.
func nestedParent(t *testing.T, withModule bool) (string, gitRepo, string) {
	t.Helper()
	repo, err := resolvedPath(initTempGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	if withModule {
		testGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--", initTempGitRepo(t), "child")
		testGit(t, repo, "commit", "-m", "track child")
	}
	head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	return repo, gitRepo{dir: repo, gitDir: filepath.Join(repo, ".git"), workTree: repo}, head
}

func TestInspectNestedRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		module  bool
		setup   func(t *testing.T, repo string)
		want    string // empty means accepted
		exclude []string
	}{
		{name: "clean submodule", module: true, exclude: []string{":(exclude,literal)child"}},
		{name: "deinitialized submodule with leftover storage", module: true, setup: func(t *testing.T, repo string) {
			testGit(t, repo, "submodule", "deinit", "-f", "--", "child")
		}, want: "unverified submodule storage"},
		{name: "deinitialized submodule without storage", module: true, setup: func(t *testing.T, repo string) {
			testGit(t, repo, "submodule", "deinit", "-f", "--", "child")
			if err := os.RemoveAll(filepath.Join(repo, ".git", "modules")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "submodule files without a checkout", module: true, setup: func(t *testing.T, repo string) {
			testGit(t, repo, "submodule", "deinit", "-f", "--", "child")
			if err := os.RemoveAll(filepath.Join(repo, ".git", "modules")); err != nil {
				t.Fatal(err)
			}
			testWrite(t, filepath.Join(repo, "child"), "notes", "keep", 0o600)
		}, want: "has files without a checkout"},
		{name: "submodule staged for removal", module: true, setup: func(t *testing.T, repo string) {
			testGit(t, repo, "rm", "--cached", "--quiet", "child")
		}, want: "staged for removal"},
		{name: "submodule git entry is a symlink", module: true, setup: func(t *testing.T, repo string) {
			dot := filepath.Join(repo, "child", ".git")
			if err := os.Remove(dot); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(repo, ".git", "modules", "child"), dot); err != nil {
				t.Fatal(err)
			}
		}, want: "unsupported .git entry"},
		{name: "submodule gitdir pointer is malformed", module: true, setup: func(t *testing.T, repo string) {
			testWrite(t, filepath.Join(repo, "child"), ".git", "not a pointer\n", 0o600)
		}, want: "invalid gitdir pointer"},
		{name: "submodule gitdir outside the module store", module: true, setup: func(t *testing.T, repo string) {
			outside, err := resolvedPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			testWrite(t, filepath.Join(repo, "child"), ".git", "gitdir: "+outside+"\n", 0o600)
		}, want: "outside the registered submodule store"},
		{name: "submodule HEAD is a symlink", module: true, setup: func(t *testing.T, repo string) {
			head := filepath.Join(repo, ".git", "modules", "child", "HEAD")
			target := filepath.Join(t.TempDir(), "HEAD")
			data, err := os.ReadFile(head)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(head); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, head); err != nil {
				t.Fatal(err)
			}
		}, want: "unsupported module HEAD"},
		{name: "submodule index missing", module: true, setup: func(t *testing.T, repo string) {
			if err := os.Remove(filepath.Join(repo, ".git", "modules", "child", "index")); err != nil {
				t.Fatal(err)
			}
		}, want: "index"},
		{name: "empty nested repository", setup: func(t *testing.T, repo string) {
			testGit(t, filepath.Join(repo, "vendor"), "init", "--quiet", "nested")
		}, exclude: []string{":(exclude,literal)vendor/nested"}},
		{name: "nested repository with an index", setup: func(t *testing.T, repo string) {
			testGit(t, repo, "init", "--quiet", "nested")
			testWrite(t, filepath.Join(repo, "nested", ".git"), "index", "", 0o600)
		}, want: "index is outside the saved snapshot"},
		{name: "nested repository with a loose object", setup: func(t *testing.T, repo string) {
			testGit(t, repo, "init", "--quiet", "nested")
			if err := os.MkdirAll(filepath.Join(repo, "nested", ".git", "objects", "ab"), 0o700); err != nil {
				t.Fatal(err)
			}
			testWrite(t, filepath.Join(repo, "nested", ".git", "objects", "ab"), "cdef", "x", 0o600)
		}, want: "objects contains data outside the saved snapshot"},
		{name: "nested repository with an external gitdir", setup: func(t *testing.T, repo string) {
			if err := os.Mkdir(filepath.Join(repo, "linked"), 0o700); err != nil {
				t.Fatal(err)
			}
			testWrite(t, filepath.Join(repo, "linked"), ".git", "gitdir: "+t.TempDir()+"\n", 0o600)
		}, want: "cannot verify external gitdir"},
		{name: "directory with HEAD and objects but no refs", setup: func(t *testing.T, repo string) {
			if err := os.MkdirAll(filepath.Join(repo, "data", "objects"), 0o700); err != nil {
				t.Fatal(err)
			}
			testWrite(t, filepath.Join(repo, "data"), "HEAD", "ref: refs/heads/main\n", 0o600)
		}},
		{name: "directory with HEAD, objects and a refs file", setup: func(t *testing.T, repo string) {
			if err := os.MkdirAll(filepath.Join(repo, "data", "objects"), 0o700); err != nil {
				t.Fatal(err)
			}
			testWrite(t, filepath.Join(repo, "data"), "HEAD", "ref: refs/heads/main\n", 0o600)
			testWrite(t, filepath.Join(repo, "data"), "refs", "", 0o600)
		}},
		{name: "bare repository shape", setup: func(t *testing.T, repo string) {
			for _, dir := range []string{"objects", "refs"} {
				if err := os.MkdirAll(filepath.Join(repo, "data", dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			testWrite(t, filepath.Join(repo, "data"), "HEAD", "ref: refs/heads/main\n", 0o600)
		}, want: "bare Git repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, checkout, head := nestedParent(t, tc.module)
			if tc.setup != nil {
				if err := os.MkdirAll(filepath.Join(repo, "vendor"), 0o700); err != nil {
					t.Fatal(err)
				}
				tc.setup(t, repo)
			}
			exclude, err := inspectNested(context.Background(), checkout, head, head, filepath.Join(repo, ".git", "modules"))
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("inspectNested error = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("inspectNested refused: %v", err)
			}
			if strings.Join(exclude, "\n") != strings.Join(tc.exclude, "\n") {
				t.Errorf("exclude = %q, want %q", exclude, tc.exclude)
			}
		})
	}
}
