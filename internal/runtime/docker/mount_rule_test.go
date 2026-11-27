package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"

	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// caseInsensitive reports whether dir sits on a file system that ignores case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	mkdirAll(t, probe)
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

// mountRuleFixture is one launch through harnessContainerSpec with a fresh
// home, state directory and worktree root.
type mountRuleFixture struct {
	a           *Adapter
	f           *councilEngine
	s           rt.CouncilLaunch
	home, state string
	root, live  string
	task        string
}

func newMountRuleFixture(t *testing.T, kind rt.CouncilCheckoutKind) *mountRuleFixture {
	t.Helper()
	home, state := isolateHome(t)
	f := newCouncilEngine()
	a := NewAdapter(f, "")
	a.discoverSocket = func() string { return "" }
	a.CouncilHarnesses = map[string]CouncilHarness{"claude": {Image: "fixture", LoginVolume: f.vol.Name, ProxyURL: "http://relay:8888", DataGateURL: "http://relay:8888/gate/mcp"}}
	m := &mountRuleFixture{a: a, f: f, home: home, state: state}
	m.root = withWorktreeRoot(t, a)
	m.task = mkdirAll(t, filepath.Join(m.root, "task-guard"))
	m.live = liveDir(t)
	path := m.task
	if kind == rt.CouncilLive {
		path = m.live
	}
	m.s = rt.CouncilLaunch{TaskID: "task-guard", Workload: "guard", Harness: "claude", Checkout: rt.CouncilCheckout{Path: path, Kind: kind},
		Limits: rt.CouncilLimits{NanoCPUs: 1, MemoryBytes: 1 << 20, PIDs: 1, WallTime: 1e9}, Prompt: "p", TaskToken: "lease"}
	return m
}

func (m *mountRuleFixture) protect(paths ...string) {
	m.a.Mounts.Protected = append(m.a.Mounts.Protected, paths...)
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestMountRuleDaemonMadeSources covers the first row of the decision of
// 2026-11-27 on issue 181 for every kind the daemon makes (task, ref, review,
// which includes the pinned verifier, and the assigned writer): the source is
// reached through the worktree root handle with no symlink component, and
// Docker gets the root's canonical path plus those components.
func TestMountRuleDaemonMadeSources(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name   string
		accept bool
		setup  func(t *testing.T, m *mountRuleFixture)
	}{
		{name: "task directory under the root", accept: true},
		{name: "own Git metadata beside it", accept: true, setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.GitDir = mkdirAll(t, m.task+".git")
		}},
		{name: "directory outside the root", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.Path = liveDir(t)
		}},
		{name: "the root itself", setup: func(t *testing.T, m *mountRuleFixture) { m.s.Checkout.Path = m.root }},
		{name: "symlink component", setup: func(t *testing.T, m *mountRuleFixture) {
			mkdirAll(t, filepath.Join(m.root, "real", "task"))
			symlink(t, filepath.Join(m.root, "real"), filepath.Join(m.root, "alias"))
			m.s.Checkout.Path = filepath.Join(m.root, "alias", "task")
		}},
		{name: "leaf symlink to a task directory", setup: func(t *testing.T, m *mountRuleFixture) {
			symlink(t, m.task, filepath.Join(m.root, "link"))
			m.s.Checkout.Path = filepath.Join(m.root, "link")
		}},
		{name: "leaf symlink out of the root", setup: func(t *testing.T, m *mountRuleFixture) {
			symlink(t, m.home, filepath.Join(m.root, "home"))
			m.s.Checkout.Path = filepath.Join(m.root, "home")
		}},
		{name: "dangling symlink", setup: func(t *testing.T, m *mountRuleFixture) {
			symlink(t, filepath.Join(m.home, "missing"), filepath.Join(m.root, "dangling"))
			m.s.Checkout.Path = filepath.Join(m.root, "dangling")
		}},
		{name: "root spelled through a symlink", setup: func(t *testing.T, m *mountRuleFixture) {
			alias := filepath.Join(t.TempDir(), "alias")
			symlink(t, m.root, alias)
			m.s.Checkout.Path = filepath.Join(alias, "task-guard")
		}},
		{name: "dot-dot component", setup: func(t *testing.T, m *mountRuleFixture) {
			mkdirAll(t, filepath.Join(m.root, "x"))
			m.s.Checkout.Path = m.root + sep + "x" + sep + ".." + sep + "task-guard"
		}},
		{name: "dot-dot out of the root", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.Path = m.root + sep + ".." + sep + filepath.Base(m.root)
		}},
		{name: "case-different spelling of the root", setup: func(t *testing.T, m *mountRuleFixture) {
			if !caseInsensitive(t, filepath.Dir(m.root)) {
				t.Skip("file system is case-sensitive")
			}
			m.s.Checkout.Path = filepath.Join(strings.ToUpper(m.root), "task-guard")
		}},
		{name: "not a directory", setup: func(t *testing.T, m *mountRuleFixture) {
			file := filepath.Join(m.root, "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			m.s.Checkout.Path = file
		}},
		{name: "no worktree root configured", setup: func(t *testing.T, m *mountRuleFixture) { m.a.Mounts = nil }},
		{name: "checkout holds the runtime socket", setup: func(t *testing.T, m *mountRuleFixture) {
			sock := filepath.Join(m.task, "docker.sock")
			m.a.discoverSocket = func() string { return sock }
		}},
		{name: "git dir outside the root", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.GitDir = liveDir(t)
		}},
		{name: "git dir is a symlink", setup: func(t *testing.T, m *mountRuleFixture) {
			real := mkdirAll(t, filepath.Join(m.root, "meta"))
			symlink(t, real, m.task+".git")
			m.s.Checkout.GitDir = m.task + ".git"
		}},
		{name: "git dir inside the checkout", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.GitDir = mkdirAll(t, filepath.Join(m.task, "meta"))
		}},
		{name: "git dir holds the checkout", setup: func(t *testing.T, m *mountRuleFixture) {
			outer := mkdirAll(t, filepath.Join(m.root, "outer"))
			m.s.Checkout.Path = mkdirAll(t, filepath.Join(outer, "task"))
			m.s.Checkout.GitDir = outer
		}},
		{name: "git dir is a worktree registration", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.GitDir = mkdirAll(t, m.task+".git")
			if err := os.WriteFile(filepath.Join(m.task+".git", "commondir"), []byte("../..\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "git dir has registered worktrees", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.GitDir = mkdirAll(t, m.task+".git")
			mkdirAll(t, filepath.Join(m.task+".git", "worktrees"))
		}},
		{name: "git dir has alternates", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.GitDir = mkdirAll(t, m.task+".git")
			mkdirAll(t, filepath.Join(m.task+".git", "objects", "info"))
			if err := os.WriteFile(filepath.Join(m.task+".git", "objects", "info", "alternates"), []byte("/elsewhere\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, kind := range []rt.CouncilCheckoutKind{rt.CouncilTask, rt.CouncilRef, rt.CouncilReview, rt.CouncilWriter} {
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				m := newMountRuleFixture(t, kind)
				if tc.setup != nil {
					tc.setup(t, m)
				}
				o, err := m.a.harnessContainerSpec(t.Context(), m.f, m.s, false)
				if !tc.accept {
					if err == nil {
						t.Fatalf("source %q accepted", m.s.Checkout.Path)
					}
					return
				}
				if err != nil {
					t.Fatalf("source refused: %v", err)
				}
				got := o.HostConfig.Mounts
				if got[0].Type != mount.TypeBind || got[0].Source != m.root+sep+"task-guard" {
					t.Fatalf("bind source = %+v, want the root's canonical path plus the task component", got[0])
				}
				if m.s.Checkout.GitDir != "" && got[1].Source != m.root+sep+"task-guard.git" {
					t.Fatalf("git dir source = %+v", got[1])
				}
			})
		}
	}
}

// TestMountRuleLiveCheckout covers the live checkout row: the operator-named
// repository is compared by device and inode with each protected path and
// every ancestor, and refused under the state directory or the worktree root.
func TestMountRuleLiveCheckout(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name   string
		accept bool
		setup  func(t *testing.T, m *mountRuleFixture)
	}{
		{name: "plain repository", accept: true},
		{name: "contains home", setup: func(t *testing.T, m *mountRuleFixture) {
			t.Setenv("HOME", mkdirAll(t, filepath.Join(m.live, "home")))
		}},
		{name: "is home", setup: func(t *testing.T, m *mountRuleFixture) { t.Setenv("HOME", m.live) }},
		{name: "contains the runtime socket", setup: func(t *testing.T, m *mountRuleFixture) {
			m.a.discoverSocket = func() string { return filepath.Join(m.live, "docker.sock") }
		}},
		{name: "contains the DOCKER_HOST socket", setup: func(t *testing.T, m *mountRuleFixture) {
			t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(m.live, "run", "engine.sock"))
		}},
		{name: "is the state directory", setup: func(t *testing.T, m *mountRuleFixture) { t.Setenv("SABLECOURT_STATE_DIR", m.live) }},
		{name: "contains the state directory", setup: func(t *testing.T, m *mountRuleFixture) {
			t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(m.live, "state"))
		}},
		{name: "lies under the state directory", setup: func(t *testing.T, m *mountRuleFixture) {
			mkdirAll(t, m.state)
			m.s.Checkout.Path = mkdirAll(t, filepath.Join(m.state, "repo"))
		}},
		{name: "is the default state directory", setup: func(t *testing.T, m *mountRuleFixture) {
			t.Setenv("SABLECOURT_STATE_DIR", "")
			m.s.Checkout.Path = mkdirAll(t, filepath.Join(m.home, ".local", "share", "sablecourt"))
		}},
		{name: "lies under a credential directory", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.Path = mkdirAll(t, filepath.Join(m.home, ".ssh", "repo"))
		}},
		{name: "lies under the worktree root", setup: func(t *testing.T, m *mountRuleFixture) { m.s.Checkout.Path = m.task }},
		{name: "contains the worktree root", setup: func(t *testing.T, m *mountRuleFixture) {
			openRootAt(t, m.a, filepath.Join(m.live, "worktrees"))
		}},
		{name: "contains a protected file that does not exist yet", setup: func(t *testing.T, m *mountRuleFixture) {
			m.protect(filepath.Join(mkdirAll(t, filepath.Join(m.live, "db")), "sablecourt.db"))
		}},
		{name: "is a protected directory", setup: func(t *testing.T, m *mountRuleFixture) { m.protect(m.live) }},
		{name: "protected path through a symlink and dot-dot", setup: func(t *testing.T, m *mountRuleFixture) {
			outside := t.TempDir()
			symlink(t, mkdirAll(t, filepath.Join(m.live, "sub")), filepath.Join(outside, "link"))
			m.protect(filepath.Join(outside, "link") + sep + ".." + sep + "sablecourt.db")
		}},
		{name: "protected file through a symlinked directory", setup: func(t *testing.T, m *mountRuleFixture) {
			if err := os.WriteFile(filepath.Join(m.live, "secrets.json"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			settings := t.TempDir()
			symlink(t, m.live, filepath.Join(settings, "inside"))
			m.protect(filepath.Join(settings, "inside", "secrets.json"))
		}},
		{name: "protected file is a symlink into the checkout", setup: func(t *testing.T, m *mountRuleFixture) {
			if err := os.WriteFile(filepath.Join(m.live, "sablecourt.db"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(t.TempDir(), "linked.db")
			symlink(t, filepath.Join(m.live, "sablecourt.db"), link)
			m.protect(link)
		}},
		{name: "protected path is a dangling symlink into the checkout", setup: func(t *testing.T, m *mountRuleFixture) {
			link := filepath.Join(t.TempDir(), "dangling.json")
			symlink(t, filepath.Join(m.live, "missing.json"), link)
			m.protect(link)
		}},
		{name: "credential directory is a dangling symlink into the checkout", setup: func(t *testing.T, m *mountRuleFixture) {
			symlink(t, filepath.Join(m.live, "missing"), filepath.Join(m.home, ".ssh"))
		}},
		{name: "protected file named with a case-different spelling", setup: func(t *testing.T, m *mountRuleFixture) {
			if !caseInsensitive(t, m.live) {
				t.Skip("file system is case-sensitive")
			}
			parent := filepath.Dir(m.live)
			upper := filepath.Join(parent, strings.ToUpper(filepath.Base(m.live)))
			if upper == m.live {
				upper = filepath.Join(parent, strings.ToLower(filepath.Base(m.live)))
			}
			m.protect(filepath.Join(upper, "secrets.json"))
		}},
		{name: "home spelled with a different case", setup: func(t *testing.T, m *mountRuleFixture) {
			if !caseInsensitive(t, m.live) {
				t.Skip("file system is case-sensitive")
			}
			home := mkdirAll(t, filepath.Join(m.live, "home"))
			t.Setenv("HOME", strings.ToUpper(home))
		}},
		{name: "question mark in a protected path into the checkout", setup: func(t *testing.T, m *mountRuleFixture) {
			m.protect(filepath.Join(mkdirAll(t, filepath.Join(m.live, "a?b")), "sablecourt.db"))
		}},
		{name: "dot-dot past what exists in a protected path", setup: func(t *testing.T, m *mountRuleFixture) {
			m.protect(m.live + sep + "missing" + sep + ".." + sep + "sablecourt.db")
		}},
		{name: "leaf symlink", setup: func(t *testing.T, m *mountRuleFixture) {
			link := filepath.Join(t.TempDir(), "link")
			symlink(t, m.live, link)
			m.s.Checkout.Path = link
		}},
		{name: "relative path", setup: func(t *testing.T, m *mountRuleFixture) { m.s.Checkout.Path = "repo" }},
		{name: "file system root", setup: func(t *testing.T, m *mountRuleFixture) { m.s.Checkout.Path = sep }},
		{name: "live checkout with separate Git metadata", setup: func(t *testing.T, m *mountRuleFixture) {
			m.s.Checkout.GitDir = mkdirAll(t, m.task+".git")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMountRuleFixture(t, rt.CouncilLive)
			if tc.setup != nil {
				tc.setup(t, m)
			}
			o, err := m.a.harnessContainerSpec(t.Context(), m.f, m.s, false)
			if !tc.accept {
				if err == nil {
					t.Fatalf("live checkout %q accepted", m.s.Checkout.Path)
				}
				return
			}
			if err != nil {
				t.Fatalf("live checkout refused: %v", err)
			}
			if got := o.HostConfig.Mounts[0]; got.Source != m.live || !got.ReadOnly {
				t.Fatalf("bind = %+v, want the repository read-only", got)
			}
		})
	}
}

// TestMountRuleRefusesRemoteEngine covers the remote engine row: host file
// identity says nothing about a remote engine's files, so no harness launches.
func TestMountRuleRefusesRemoteEngine(t *testing.T) {
	for _, tc := range []struct {
		name, env, client string
		accept            bool
	}{
		{name: "local socket", env: "unix:///run/docker.sock", accept: true},
		{name: "tcp DOCKER_HOST", env: "tcp://engine.invalid:2376"},
		{name: "ssh DOCKER_HOST", env: "ssh://engine.invalid"},
		{name: "client resolved a tcp host", client: "tcp://engine.invalid:2375"},
		{name: "unparsable DOCKER_HOST", env: "://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMountRuleFixture(t, rt.CouncilTask)
			t.Setenv("DOCKER_HOST", tc.env)
			engine := hostedEngine{councilEngine: m.f, host: tc.client}
			m.a.Client = engine
			_, err := m.a.harnessContainerSpec(t.Context(), engine, m.s, false)
			if tc.accept && err != nil {
				t.Fatalf("local engine refused: %v", err)
			}
			if !tc.accept && (err == nil || !strings.Contains(err.Error(), "remote engine")) {
				t.Fatalf("remote engine: %v", err)
			}
		})
	}
}
