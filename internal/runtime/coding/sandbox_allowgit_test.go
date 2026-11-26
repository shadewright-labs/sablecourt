package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitSandboxLayout is a synthetic Git registration on disk: a common
// directory with objects and refs, a linked worktree registration under
// common/worktrees, a checkout directory outside the common directory, and a
// separate bare-style directory for live mode.
type gitSandboxLayout struct {
	root, work, common, git, live string
}

func newGitSandboxLayout(t *testing.T, rootName string) gitSandboxLayout {
	t.Helper()
	base, err := resolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, rootName)
	l := gitSandboxLayout{
		root:   root,
		work:   filepath.Join(root, "work"),
		common: filepath.Join(root, "repo", ".git"),
		live:   filepath.Join(root, "live.git"),
	}
	l.git = filepath.Join(l.common, "worktrees", "task")
	for _, dir := range []string{
		l.work, filepath.Join(l.common, "objects"), filepath.Join(l.common, "refs"), l.git,
		filepath.Join(l.live, "objects"), filepath.Join(l.live, "refs"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func (l gitSandboxLayout) state(mode string) *worktreeState {
	return &worktreeState{
		TaskID:    "task-1",
		Mode:      mode,
		CommonDir: l.common,
		GitDir:    l.git,
		Branch:    "sablecourt/task-1",
		Base:      strings.Repeat("ab", 20),
	}
}

func replaceWithSymlink(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
}

func TestAllowGitRegistrationRefusals(t *testing.T) {
	type tc struct {
		name     string
		mode     string
		readOnly bool
		setup    func(t *testing.T, l gitSandboxLayout, s *worktreeState)
		want     string // empty means allowed
	}
	reviewer := func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
		isolated := filepath.Join(l.work, ".git")
		for _, dir := range []string{filepath.Join(isolated, "objects"), filepath.Join(isolated, "refs")} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		s.ReadOnly, s.Branch, s.CommonDir, s.GitDir = true, "", isolated, isolated
	}
	live := func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
		if err := os.MkdirAll(filepath.Join(l.work, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		s.Branch, s.CommonDir, s.GitDir = "", l.live, l.live
	}
	writer := func(_ *testing.T, _ gitSandboxLayout, s *worktreeState) {
		s.CouncilKey = "c1"
		s.Branch = councilWriterBranch("c1")
	}
	cases := []tc{
		// Default task worktree.
		{name: "task writer allowed", mode: ""},
		{name: "task reader allowed", mode: "", readOnly: true},
		{name: "missing common dir", setup: func(_ *testing.T, l gitSandboxLayout, s *worktreeState) {
			s.CommonDir = filepath.Join(l.root, "absent")
		}, want: "no such file"},
		{name: "missing git dir", setup: func(_ *testing.T, l gitSandboxLayout, s *worktreeState) {
			s.GitDir = filepath.Join(l.common, "worktrees", "absent")
		}, want: "no such file"},
		{name: "task branch mismatch", setup: func(_ *testing.T, _ gitSandboxLayout, s *worktreeState) {
			s.Branch = "main"
		}, want: "invalid coding sandbox Git registration"},
		{name: "git dir outside worktrees", setup: func(_ *testing.T, l gitSandboxLayout, s *worktreeState) {
			s.GitDir = filepath.Join(l.common, "refs")
		}, want: "invalid coding sandbox Git registration"},
		{name: "common dir inside checkout", setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			inner := filepath.Join(l.work, "nested", ".git")
			if err := os.MkdirAll(filepath.Join(inner, "worktrees", "task"), 0o700); err != nil {
				t.Fatal(err)
			}
			s.CommonDir, s.GitDir = inner, filepath.Join(inner, "worktrees", "task")
		}, want: "invalid coding sandbox Git registration"},
		{name: "objects redirected", setup: func(t *testing.T, l gitSandboxLayout, _ *worktreeState) {
			replaceWithSymlink(t, filepath.Join(l.common, "objects"))
		}, want: "unavailable or redirected"},
		{name: "refs missing", setup: func(t *testing.T, l gitSandboxLayout, _ *worktreeState) {
			if err := os.RemoveAll(filepath.Join(l.common, "refs")); err != nil {
				t.Fatal(err)
			}
		}, want: "unavailable or redirected"},
		{name: "config redirected", readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, _ *worktreeState) {
			replaceWithSymlink(t, filepath.Join(l.common, "config"))
		}, want: "redirected"},
		{name: "exclude config redirected", readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, _ *worktreeState) {
			replaceWithSymlink(t, l.work+excludeConfigSuffix)
		}, want: "redirected"},
		{name: "worktree logs redirected", setup: func(t *testing.T, l gitSandboxLayout, _ *worktreeState) {
			replaceWithSymlink(t, filepath.Join(l.git, "logs"))
		}, want: "redirected"},
		{name: "branch ref redirected", setup: func(t *testing.T, l gitSandboxLayout, _ *worktreeState) {
			if err := os.MkdirAll(filepath.Join(l.common, "refs", "heads", "sablecourt"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(l.root, "elsewhere"), filepath.Join(l.common, "refs", "heads", "sablecourt", "task-1")); err != nil {
				t.Fatal(err)
			}
		}, want: "redirected"},
		{name: "writer without base commit", setup: func(_ *testing.T, _ gitSandboxLayout, s *worktreeState) {
			s.Base = "HEAD"
		}, want: "no valid base commit ID"},
		{name: "reader ignores base commit", readOnly: true, setup: func(_ *testing.T, _ gitSandboxLayout, s *worktreeState) {
			s.Base = ""
		}},
		{name: "unknown mode", mode: "shared", want: "unsupported coding sandbox checkout mode"},

		// Ref checkouts are detached and read-only.
		{name: "ref reader allowed", mode: checkoutModeRef, readOnly: true, setup: func(_ *testing.T, _ gitSandboxLayout, s *worktreeState) {
			s.Branch = ""
		}},
		{name: "ref writer refused", mode: checkoutModeRef, setup: func(_ *testing.T, _ gitSandboxLayout, s *worktreeState) {
			s.Branch = ""
		}, want: "invalid coding sandbox ref Git registration"},
		{name: "ref with branch refused", mode: checkoutModeRef, readOnly: true, want: "invalid coding sandbox ref Git registration"},

		// Council writer on its own branch.
		{name: "council writer allowed", mode: checkoutModeWriter, setup: writer},
		{name: "council writer read-only refused", mode: checkoutModeWriter, readOnly: true, setup: writer,
			want: "invalid coding sandbox writer Git registration"},
		{name: "council writer without key refused", mode: checkoutModeWriter, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			writer(t, l, s)
			s.CouncilKey = ""
		}, want: "invalid coding sandbox writer Git registration"},
		{name: "council writer foreign branch refused", mode: checkoutModeWriter, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			writer(t, l, s)
			s.Branch = "sablecourt/task-1"
		}, want: "invalid coding sandbox writer Git registration"},

		// Council reviewer in an isolated checkout.
		{name: "reviewer allowed", mode: checkoutModeWriter, readOnly: true, setup: reviewer},
		{name: "reviewer writable refused", mode: checkoutModeWriter, setup: reviewer,
			want: "invalid coding sandbox reviewer Git registration"},
		{name: "reviewer with branch refused", mode: checkoutModeWriter, readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			reviewer(t, l, s)
			s.Branch = councilWriterBranch("c1")
		}, want: "invalid coding sandbox reviewer Git registration"},
		{name: "reviewer on shared registration refused", mode: checkoutModeWriter, readOnly: true, setup: func(_ *testing.T, _ gitSandboxLayout, s *worktreeState) {
			s.ReadOnly, s.Branch = true, ""
		}, want: "invalid coding sandbox reviewer isolated Git registration"},
		{name: "reviewer objects redirected", mode: checkoutModeWriter, readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			reviewer(t, l, s)
			replaceWithSymlink(t, filepath.Join(l.work, ".git", "refs"))
		}, want: "unavailable or redirected"},

		// Live checkouts read the primary repository through a separate Git dir.
		{name: "live reader allowed", mode: checkoutModeLive, readOnly: true, setup: live},
		{name: "live writer refused", mode: checkoutModeLive, setup: live, want: "requires read-only confinement"},
		{name: "live without repository git dir", mode: checkoutModeLive, readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			live(t, l, s)
			if err := os.RemoveAll(filepath.Join(l.work, ".git")); err != nil {
				t.Fatal(err)
			}
		}, want: "unavailable or redirected"},
		{name: "live repository git dir redirected", mode: checkoutModeLive, readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			live(t, l, s)
			replaceWithSymlink(t, filepath.Join(l.work, ".git"))
		}, want: "unavailable or redirected"},
		{name: "live common differs from git dir", mode: checkoutModeLive, readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			live(t, l, s)
			s.CommonDir = l.common
		}, want: "invalid coding sandbox live Git registration"},
		{name: "live with branch", mode: checkoutModeLive, readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			live(t, l, s)
			s.Branch = "main"
		}, want: "invalid coding sandbox live Git registration"},
		{name: "live git dir inside checkout", mode: checkoutModeLive, readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			live(t, l, s)
			s.CommonDir, s.GitDir = filepath.Join(l.work, ".git"), filepath.Join(l.work, ".git")
		}, want: "invalid coding sandbox live Git registration"},
		{name: "live objects redirected", mode: checkoutModeLive, readOnly: true, setup: func(t *testing.T, l gitSandboxLayout, s *worktreeState) {
			live(t, l, s)
			replaceWithSymlink(t, filepath.Join(l.live, "objects"))
		}, want: "unavailable or redirected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newGitSandboxLayout(t, "root")
			s := l.state(c.mode)
			if c.setup != nil {
				c.setup(t, l, s)
			}
			var profile strings.Builder
			err := allowGit(&profile, l.work, s, c.readOnly)
			if c.want == "" {
				if err != nil {
					t.Fatalf("allowGit refused a valid registration: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("allowGit error = %v, want %q", err, c.want)
			}
		})
	}
}

func TestAllowGitProfileRules(t *testing.T) {
	t.Run("task writer", func(t *testing.T) {
		l := newGitSandboxLayout(t, "root")
		s := l.state("")
		var b strings.Builder
		if err := allowGit(&b, l.work, s, false); err != nil {
			t.Fatal(err)
		}
		profile := b.String()
		ref := filepath.Join(l.common, "refs", "heads", "sablecourt", "task-1")
		for _, rule := range []string{
			fmt.Sprintf("(allow file-read* file-test-existence file-write* (literal %q))", filepath.Join(l.git, "index")),
			fmt.Sprintf("(allow file-read* file-test-existence file-write* (literal %q))", ref),
			fmt.Sprintf("(allow file-read* file-test-existence (literal %q))", l.work+excludeConfigSuffix),
			"(allow file-write-create (regex #",
		} {
			if !strings.Contains(profile, rule) {
				t.Errorf("writer profile lacks %s", rule)
			}
		}
		// Configuration stays under daemon control.
		if strings.Contains(profile, fmt.Sprintf("file-write* (literal %q)", filepath.Join(l.common, "config"))) {
			t.Error("writer profile grants writes to the Git configuration")
		}
	})
	t.Run("ref reader has no writes", func(t *testing.T) {
		l := newGitSandboxLayout(t, "root")
		s := l.state(checkoutModeRef)
		s.Branch = ""
		var b strings.Builder
		if err := allowGit(&b, l.work, s, true); err != nil {
			t.Fatal(err)
		}
		profile := b.String()
		if strings.Contains(profile, "file-write") {
			t.Errorf("read-only profile grants writes:\n%s", profile)
		}
		if strings.Contains(profile, l.work+excludeConfigSuffix) {
			t.Error("ref profile reads the task exclude configuration")
		}
	})
	t.Run("live reader denies the repository git dir", func(t *testing.T) {
		l := newGitSandboxLayout(t, "root")
		if err := os.MkdirAll(filepath.Join(l.work, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		s := l.state(checkoutModeLive)
		s.Branch, s.CommonDir, s.GitDir = "", l.live, l.live
		var b strings.Builder
		if err := allowGit(&b, l.work, s, true); err != nil {
			t.Fatal(err)
		}
		profile := b.String()
		for _, rule := range []string{
			fmt.Sprintf("(deny file-read* file-test-existence (subpath %q))", filepath.Join(l.work, ".git")),
			fmt.Sprintf("(allow file-read* file-test-existence (subpath %q))", l.live),
		} {
			if !strings.Contains(profile, rule) {
				t.Errorf("live profile lacks %s", rule)
			}
		}
		if strings.Contains(profile, "file-write") {
			t.Error("live profile grants writes")
		}
	})
	t.Run("objects path with a quote", func(t *testing.T) {
		l := newGitSandboxLayout(t, `ro"ot`)
		var b strings.Builder
		err := allowGit(&b, l.work, l.state(""), false)
		if err == nil || !strings.Contains(err.Error(), "unsupported characters") {
			t.Fatalf("allowGit error = %v, want unsupported characters refusal", err)
		}
	})
}
