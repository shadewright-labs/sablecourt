package mountguard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resolvedTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openRoot(t *testing.T, dir string) *Root {
	t.Helper()
	r, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestRootSource checks the daemon-made source rule on its own: only the
// root's canonical path plus plain, non-symlink directory components.
func TestRootSource(t *testing.T) {
	base := resolvedTemp(t)
	r := openRoot(t, filepath.Join(base, "worktrees"))
	sep := string(filepath.Separator)
	mkdir(t, filepath.Join(r.Path(), "task", "sub"))
	mkdir(t, filepath.Join(base, "outside"))
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(r.Path(), "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.Path(), "task"), filepath.Join(r.Path(), "in")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "task", path: r.Path() + sep + "task", want: r.Path() + sep + "task"},
		{name: "nested", path: r.Path() + sep + "task" + sep + "sub", want: r.Path() + sep + "task" + sep + "sub"},
		{name: "root itself", path: r.Path()},
		{name: "trailing separator", path: r.Path() + sep + "task" + sep},
		{name: "double separator", path: r.Path() + sep + sep + "task"},
		{name: "dot", path: r.Path() + sep + "." + sep + "task"},
		{name: "dot-dot", path: r.Path() + sep + "task" + sep + ".." + sep + "task"},
		{name: "symlink out", path: r.Path() + sep + "out"},
		{name: "symlink in", path: r.Path() + sep + "in" + sep + "sub"},
		{name: "missing", path: r.Path() + sep + "missing"},
		{name: "sibling with the root as prefix", path: r.Path() + "-other" + sep + "task"},
		{name: "outside", path: filepath.Join(base, "outside")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Source(tc.path)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("Source(%q) = %q, want a refusal", tc.path, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Source(%q) = %q, %v, want %q", tc.path, got, err, tc.want)
			}
		})
	}
}

// TestRootRefusesWhenSwapped checks that the canonical path must still name
// the opened directory.
func TestRootRefusesWhenSwapped(t *testing.T) {
	base := resolvedTemp(t)
	r := openRoot(t, filepath.Join(base, "worktrees"))
	mkdir(t, filepath.Join(r.Path(), "task"))
	if err := os.Rename(r.Path(), filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(r.Path(), "task"))
	if _, err := r.Source(filepath.Join(r.Path(), "task")); err == nil {
		t.Fatal("source accepted after the root was replaced")
	}
}

// TestRootRemoveStaysInside checks deletions through the handle.
func TestRootRemoveStaysInside(t *testing.T) {
	base := resolvedTemp(t)
	r := openRoot(t, filepath.Join(base, "worktrees"))
	victim := mkdir(t, filepath.Join(base, "victim.git"))
	if err := os.Symlink(victim, filepath.Join(r.Path(), "task.git")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{victim, r.Path() + string(filepath.Separator) + ".." + string(filepath.Separator) + "victim.git"} {
		if err := r.RemoveAll(path); err == nil {
			t.Errorf("RemoveAll(%q) accepted a path outside the root", path)
		}
	}
	if err := r.RemoveAll(filepath.Join(r.Path(), "task.git")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("RemoveAll followed a symlink out of the root: %v", err)
	}
	if err := r.Remove(filepath.Join(r.Path(), "missing.json")); err != nil {
		t.Fatalf("Remove of a missing file: %v", err)
	}
}

// TestRefuseInside covers the startup row: the database, secret file and
// state directory may not lie under the worktree root or share its identity.
func TestRefuseInside(t *testing.T) {
	base := resolvedTemp(t)
	r := openRoot(t, filepath.Join(base, "worktrees"))
	if err := os.Symlink(r.Path(), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		name, path string
		refuse     bool
	}{
		{name: "beside the root", path: filepath.Join(base, "sablecourt.db")},
		{name: "empty", path: ""},
		{name: "under the root", path: filepath.Join(r.Path(), "sablecourt.db"), refuse: true},
		{name: "the root itself", path: r.Path(), refuse: true},
		{name: "under the root through a symlink", path: filepath.Join(base, "alias", "state"), refuse: true},
		{name: "under the root through dot-dot", path: r.Path() + sep + "x" + sep + ".." + sep + "secrets.json", refuse: true},
		{name: "deep under the root, not yet created", path: filepath.Join(r.Path(), "a", "b", "c"), refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r.RefuseInside(tc.path)
			if (err != nil) != tc.refuse {
				t.Fatalf("RefuseInside(%q) = %v, want refusal %t", tc.path, err, tc.refuse)
			}
		})
	}
}

// TestAncestry checks the identity walk on the cases the review passes found:
// dot-dot after a symlink, dangling symlinks, dot components past what
// exists, case-different spellings and "?" in a name.
func TestAncestry(t *testing.T) {
	base := resolvedTemp(t)
	checkout := mkdir(t, filepath.Join(base, "checkout"))
	mkdir(t, filepath.Join(checkout, "sub"))
	mkdir(t, filepath.Join(checkout, "a?b"))
	settings := mkdir(t, filepath.Join(base, "settings"))
	if err := os.Symlink(filepath.Join(checkout, "sub"), filepath.Join(settings, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "missing"), filepath.Join(settings, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "file"), filepath.Join(settings, "file-link")); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	held, err := os.Stat(checkout)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		under      bool // the walk reaches the checkout
		err        bool
	}{
		{name: "file in the checkout", path: filepath.Join(checkout, "file"), under: true},
		{name: "missing file in the checkout", path: filepath.Join(checkout, "x", "y.db"), under: true},
		{name: "symlink then dot-dot", path: settings + sep + "link" + sep + ".." + sep + "new.db", under: true},
		{name: "symlink to a file in the checkout", path: filepath.Join(settings, "file-link"), under: true},
		{name: "question mark in a name", path: filepath.Join(checkout, "a?b", "sablecourt.db"), under: true},
		{name: "outside", path: filepath.Join(settings, "new.db")},
		{name: "dangling symlink", path: filepath.Join(settings, "dangling"), err: true},
		{name: "dot-dot past what exists", path: settings + sep + "missing" + sep + ".." + sep + "new.db", err: true},
		{name: "relative", path: "settings" + sep + "new.db", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anc, err := ancestry(tc.path)
			if tc.err {
				if err == nil {
					t.Fatalf("ancestry(%q) succeeded, want an error", tc.path)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range anc {
				found = found || os.SameFile(a, held)
			}
			if found != tc.under {
				t.Fatalf("ancestry(%q) reaches the checkout: %t, want %t", tc.path, found, tc.under)
			}
		})
	}
	t.Run("case-different spelling", func(t *testing.T) {
		upper := filepath.Join(base, "CHECKOUT")
		if _, err := os.Stat(upper); err != nil {
			t.Skip("file system is case-sensitive")
		}
		anc, err := ancestry(filepath.Join(upper, "secrets.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(anc[0], held) {
			t.Fatal("case-different spelling did not reach the checkout")
		}
	})
}

// TestCheckLiveWithoutGuard checks that a nil Guard still applies the rules
// that need no daemon configuration.
func TestCheckLiveWithoutGuard(t *testing.T) {
	home := resolvedTemp(t)
	t.Setenv("HOME", home)
	t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(resolvedTemp(t), "state"))
	var g *Guard
	if err := g.CheckLive(resolvedTemp(t), nil); err != nil {
		t.Fatalf("plain repository refused: %v", err)
	}
	if err := g.CheckLive(home, nil); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("home accepted: %v", err)
	}
	if _, err := g.MountSource(filepath.Join(home, "task"), false, nil); err == nil || !strings.Contains(err.Error(), "no worktree root") {
		t.Fatalf("daemon-made source without a root: %v", err)
	}
}

// TestOverlap checks the identity overlap test used for a launch's Git
// metadata directory.
func TestOverlap(t *testing.T) {
	base := resolvedTemp(t)
	a := mkdir(t, filepath.Join(base, "task"))
	inner := mkdir(t, filepath.Join(a, "meta"))
	sibling := mkdir(t, filepath.Join(base, "task.git"))
	for _, tc := range []struct {
		x, y string
		want bool
	}{
		{a, inner, true}, {inner, a, true}, {a, a, true}, {a, sibling, false},
	} {
		got, err := Overlap(tc.x, tc.y)
		if err != nil || got != tc.want {
			t.Errorf("Overlap(%q, %q) = %t, %v, want %t", tc.x, tc.y, got, err, tc.want)
		}
	}
	if _, err := Overlap(a, filepath.Join(base, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing directory: %v", err)
	}
}
