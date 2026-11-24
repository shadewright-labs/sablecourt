package coding

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestLaunchCommandUsesAbsoluteHarness(t *testing.T) {
	for _, tc := range []struct{ harness, sandbox string }{
		{"claude", "seatbelt"}, {"codex", "seatbelt"}, {"agy", "seatbelt"},
		{"claude", "none"}, {"codex", "none"}, {"agy", "none"},
	} {
		t.Run(tc.harness+"/"+tc.sandbox, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			spec.Workload.Spec.Coding.Harness = tc.harness
			spec.Workload.Spec.Coding.Sandbox = tc.sandbox
			h := startTestTask(t, a, spec)
			state, err := a.worktreeState(h)
			if err != nil {
				t.Fatal(err)
			}
			dir, scratch := t.TempDir(), t.TempDir()
			testWrite(t, dir, tc.harness, "#!/bin/sh\nexit 0\n", 0o700)
			t.Setenv("PATH", dir)
			cmd, _, err := a.buildCommand(spec, launchInput{prompt: "fixture"}, h.WorkDir, scratch, state, false)
			if err != nil {
				t.Fatal(err)
			}
			bin := cmd.Args[0]
			if tc.sandbox == "seatbelt" {
				bin = cmd.Args[3]
			}
			want, err := resolvedPath(filepath.Join(dir, tc.harness))
			if err != nil || bin != want {
				t.Fatalf("harness executable = %q, want %q (%v)", bin, want, err)
			}

			a.HarnessPaths = map[string]string{tc.harness: filepath.Join(dir, "missing")}
			_, _, err = a.buildCommand(spec, launchInput{prompt: "fixture"}, h.WorkDir, scratch, state, false)
			if err == nil || !strings.Contains(err.Error(), a.HarnessPaths[tc.harness]) {
				t.Fatalf("missing configured executable error = %v", err)
			}
		})
	}
}

func TestSeatbeltCanonicalLaunchPaths(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("readOnly=%t", readOnly), func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			spec.Workload.Spec.Coding.Sandbox = "seatbelt"
			h := startTestTask(t, a, spec)
			state, err := a.worktreeState(h)
			if err != nil {
				t.Fatal(err)
			}
			root, err := resolvedPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			scratch := filepath.Join(root, "scratch")
			if err := os.Mkdir(scratch, 0o700); err != nil {
				t.Fatal(err)
			}
			workAlias := filepath.Join(root, "checkout")
			if err := os.Symlink(h.WorkDir, workAlias); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, "harness")
			testWrite(t, root, "harness", "#!/bin/sh\nexit 0\n", 0o700)
			binAlias := filepath.Join(alias, "harness")
			a.HarnessPaths = map[string]string{"claude": binAlias}
			scratchAlias := filepath.Join(alias, "scratch")
			cmd, profile, err := a.buildCommand(spec, launchInput{prompt: "fixture"}, workAlias, scratchAlias, state, readOnly)
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Dir != h.WorkDir || cmd.Args[3] != bin ||
				!strings.Contains(profile, fmt.Sprintf("(allow file-write* (subpath %q))", scratch)) ||
				strings.Contains(profile, alias) || strings.Contains(profile, workAlias) {
				t.Errorf("noncanonical Seatbelt launch paths: directory %q, executable %q", cmd.Dir, cmd.Args[3])
			}
			for _, key := range []string{"HOME", "TMPDIR"} {
				if !strings.Contains("\n"+strings.Join(cmd.Env, "\n")+"\n", "\n"+key+"="+scratch+"\n") {
					t.Errorf("%s does not point to the canonical scratch directory", key)
				}
			}
			headLock := fmt.Sprintf("(literal %q)", filepath.Join(state.GitDir, "HEAD.lock"))
			if strings.Contains(profile, headLock) == readOnly {
				t.Error("only writers may lock the worktree HEAD during a Git commit")
			}
		})
	}
}

func TestSeatbeltRuntimePaths(t *testing.T) {
	a, spec := codingFixture(t)
	a.DryRun = true
	h := startTestTask(t, a, spec)
	state, err := a.worktreeState(h)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := seatbeltProfile(h.WorkDir, t.TempDir(), "/bin/sh", state, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(allow file-read* file-test-existence (literal "/"))`) {
		t.Error("profile denies dyld opening the root directory")
	}
	for _, path := range []string{"/usr/lib", "/System/Cryptexes/OS"} {
		if real, err := resolvedPath(path); err == nil {
			rule := fmt.Sprintf("(allow file-read* file-test-existence file-map-executable (subpath %q))", real)
			if !strings.Contains(profile, rule) {
				t.Errorf("missing dyld rule for %s", real)
			}
		}
	}
	if strings.Contains(profile, "(allow file-read*)") {
		t.Error("profile permits blanket file-read*")
	}
	for _, rule := range []string{
		`(allow file-read-metadata file-test-existence (literal "/etc"))`,
		`(allow file-read* file-test-existence (literal "/private/var/select/sh"))`,
		`(allow file-read* file-test-existence (literal "/private/etc/gitconfig"))`,
		fmt.Sprintf("(allow file-read* file-test-existence (literal %q))", filepath.Join(state.CommonDir, "info", "exclude")),
	} {
		if !strings.Contains(profile, rule) {
			t.Errorf("missing expected literal rule %s", rule)
		}
	}
}

func TestSeatbeltRefusesRedirectedGitPaths(t *testing.T) {
	for _, name := range []string{"packed-refs", "logs/refs", "info/exclude"} {
		t.Run(name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			h := startTestTask(t, a, spec)
			state, err := a.worktreeState(h)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state.CommonDir, name)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Fatal(err)
			}
			if _, err := seatbeltProfile(h.WorkDir, t.TempDir(), "/bin/sh", state, false); err == nil {
				t.Fatal("accepted redirected Git path")
			}
		})
	}
}

func TestHarnessEnvironmentAndStdin(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "agy"} {
		t.Run(harness, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.AttachHub = attach.NewHub()
			spec.Workload.Spec.Coding.Harness = harness
			t.Setenv("INHERITED_VALUE", "sentinel")
			t.Setenv("OPENAI_API_KEY", "fixture-secret")
			dir := t.TempDir()
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			testWrite(t, dir, harness, `#!/bin/sh
test -n "$HOME" || exit 10
test -z "${INHERITED_VALUE+x}${OPENAI_API_KEY+x}" || exit 11
if read -r input; then exit 12; fi
printf 'stdin reached EOF\n'
`, 0o700)
			h := startTestTask(t, a, spec)
			t.Cleanup(func() { _ = syscall.Kill(-h.PID, syscall.SIGKILL) })
			var log []byte
			waitUntil(t, func() bool {
				log, _ = os.ReadFile(h.WorkDir + ".log")
				return strings.Contains(string(log), "stdin reached EOF")
			})
			if _, err := a.Observe(context.Background(), h); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHarnessExitObservation(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		code         int
		message      string
	}{
		{"success", "exit 0", 0, ""},
		{"nonzero with redacted diagnostics", "printf 'denied: password=' >&2; printf 'fixture-value\n' >&2; exit 23", 23, "denied: password=[REDACTED:secret]"},
		{"exit 127", "exit 127", 127, ""},
		{"bounded diagnostics", "i=0; while test $i -lt 300; do printf '%080d\n' 0 >&2; i=$((i+1)); done; printf 'last diagnostic' >&2; exit 9", 9, "last diagnostic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shellHarness(t, tc.script)
			a, spec := codingFixture(t)
			h := startTestTask(t, a, spec)
			waitUntil(t, func() bool { return outputClosed(a.process(h.WorkDir).done) })
			want := scruntime.StateCompleted
			if tc.code != 0 {
				want = scruntime.StateFailed
			}
			// A fresh adapter must reach the same conclusion from durable evidence.
			for _, adapter := range []*Adapter{NewAdapter(a.WorktreeRoot), a} {
				obs, err := adapter.Observe(context.Background(), h)
				if err != nil || obs.State != want || obs.ExitCode != tc.code {
					t.Fatalf("Observe = %+v, %v; want %s with exit %d", obs, err, want, tc.code)
				}
				if len(obs.Message) > 8192 || !strings.Contains(obs.Message, tc.message) || strings.Contains(obs.Message, "fixture-value") {
					t.Fatalf("diagnostics length = %d, expected redacted tail %q", len(obs.Message), tc.message)
				}
			}
		})
	}
}

func TestHarnessLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         scruntime.ObservedState
	}{
		{"wall time", "trap '' TERM; while :; do :; done", scruntime.StateFailed},
		{"descendant retains stdout", "sleep 30 & exit 0", scruntime.StateCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shellHarness(t, tc.script)
			a, spec := codingFixture(t)
			if tc.want == scruntime.StateFailed {
				a.WallTime = 150 * time.Millisecond
			}
			h := startTestTask(t, a, spec)
			t.Cleanup(func() { _ = syscall.Kill(-h.PID, syscall.SIGKILL) })
			deadline := time.Now().Add(4 * time.Second)
			var last scruntime.Observation
			for time.Now().Before(deadline) {
				obs, err := a.Observe(context.Background(), h)
				if err != nil {
					t.Fatal(err)
				}
				if obs.State == tc.want {
					if tc.want == scruntime.StateFailed && !strings.Contains(obs.Message, "wall time") {
						t.Fatalf("missing wall time reason: %+v", obs)
					}
					return
				}
				last = obs
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatalf("task did not reach %s (last observation: %+v)", tc.want, last)
		})
	}
}

func TestLaunchRefusesInvalidSandbox(t *testing.T) {
	for _, sandbox := range []string{"", "unknown", "docker", "bubblewrap", "none"} {
		t.Run("value="+sandbox, func(t *testing.T) {
			shellHarness(t, "exit 0")
			a, spec := codingFixture(t)
			spec.Workload.Spec.Coding.Sandbox = sandbox
			a.allowUnconfined = false
			h, err := a.Start(context.Background(), spec)
			if err == nil {
				_ = a.Stop(context.Background(), h)
				t.Fatal("started with an unavailable sandbox")
			}
		})
	}
}

func TestLaunchRefusesMalformedInput(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"wrong root type", "[]"},
		{"null root", "null"},
		{"wrong field type", `{"prompt":17}`},
		{"null field", `{"model":null}`},
		{"duplicate field", `{"prompt":"one","prompt":"two"}`},
		{"empty model", `{"model":" "}`},
		{"flag as model", `{"model":"--settings"}`},
		{"flag as prompt", `{"prompt":"--settings"}`},
		{"oversized", `{"prompt":"` + strings.Repeat("x", 70*1024) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			spec.InputJSON = tc.input
			if _, err := a.Start(context.Background(), spec); err == nil {
				t.Fatal("accepted malformed launch input")
			}
		})
	}
}

func TestSeatbeltProfileIsolation(t *testing.T) {
	for _, name := range []string{"operator.token", "sablecourt.db"} {
		t.Run(name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			h := startTestTask(t, a, spec)
			state, err := a.worktreeState(h)
			if err != nil {
				t.Fatal(err)
			}
			root, work := filepath.Dir(h.WorkDir), h.WorkDir
			resolved, err := resolvedPath(work)
			if err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(work, alias); err != nil {
				t.Fatal(err)
			}
			profile, err := seatbeltProfile(alias, t.TempDir(), "/bin/sh", state, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(profile, "(deny default)") || !strings.Contains(profile, resolved) ||
				strings.Contains(profile, alias) || strings.Contains(profile, "(subpath "+`"/tmp")`) ||
				strings.Contains(profile, filepath.Join(root, name)) {
				t.Fatalf("profile does not confine reads and writes to resolved paths:\n%s", profile)
			}
			t.Run("native", func(t *testing.T) {
				if runtime.GOOS != "darwin" {
					t.Skip("Seatbelt requires macOS")
				}
				out, err := exec.CommandContext(t.Context(), seatbeltExecutable, "-p", profile, "/usr/bin/true").CombinedOutput()
				if err != nil {
					if strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
						requireNestedSeatbelt(t)
					}
					t.Fatalf("Seatbelt profile rejected: %v (%s)", err, out)
				}
			})
		})
	}
}

func TestHarnessUnrecordedExitFailsClosed(t *testing.T) {
	for _, name := range []string{"observe", "collect"} {
		t.Run(name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			h := startTestTask(t, a, spec)
			h.PID = 1 << 30
			restored := NewAdapter(a.WorktreeRoot)
			if name == "observe" {
				obs, err := restored.Observe(context.Background(), h)
				if err == nil && obs.State != scruntime.StateFailed {
					t.Fatalf("recorded an unobserved exit as %s", obs.State)
				}
			} else if _, err := restored.CollectArtifacts(context.Background(), h); err == nil {
				t.Fatal("collected worktree without recorded exit or owned process")
			}
		})
	}
}

func requireNestedSeatbelt(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt requires macOS")
	}
	out, err := exec.CommandContext(t.Context(), seatbeltExecutable, "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput()
	if err == nil {
		return
	}
	if strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
		t.Skip("host sandbox denies nested Seatbelt profiles")
	}
	t.Fatalf("Seatbelt probe: %v (%s)", err, out)
}

func TestSeatbeltWorktreeAndSecretIsolation(t *testing.T) {
	requireNestedSeatbelt(t)
	for _, name := range []string{"operator.token", "sablecourt.db"} {
		t.Run(name, func(t *testing.T) {
			a, spec := codingFixture(t)
			stateDir, err := resolvedPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			testWrite(t, stateDir, name, "fixture", 0o600)
			worktrees := filepath.Join(stateDir, "worktrees")
			if err := os.Mkdir(worktrees, 0o700); err != nil {
				t.Fatal(err)
			}
			a.WorktreeRoot = filepath.Join(stateDir, "alias")
			if err := os.Symlink(worktrees, a.WorktreeRoot); err != nil {
				t.Fatal(err)
			}
			spec.Workload.Spec.Coding.Sandbox = "seatbelt"
			outside := filepath.Join(stateDir, "outside")
			fakeHarness(t, fmt.Sprintf(`#!/bin/sh
set -e
if cat %q; then exit 11; fi
if (printf escaped > %q); then exit 12; fi
if (printf forged > "$PWD.run/exit"); then exit 13; fi
printf 'scratch\n' > "$HOME/scratch"
printf 'temporary\n' > "$TMPDIR/temporary"
printf 'created\n' > example
printf 'edited\n' >> example
git add -- example
git commit --no-gpg-sign -m fixture
`, filepath.Join(stateDir, name), outside))
			h := startTestTask(t, a, spec)
			waitUntil(t, func() bool { return outputClosed(a.process(h.WorkDir).done) })
			obs, err := a.Observe(context.Background(), h)
			if err != nil || obs.State != scruntime.StateCompleted {
				t.Fatalf("sandboxed writer = %+v, %v", obs, err)
			}
			if got := testGit(t, spec.Workload.Spec.Coding.RepoPath, "show", h.Branch+":example"); got != "created\nedited\n" {
				t.Fatalf("sandboxed commit content = %q", got)
			}
			if _, err := os.Stat(outside); !os.IsNotExist(err) {
				t.Fatalf("sandboxed writer touched outside path: %v", err)
			}
		})
	}
}
