package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func interceptGit(t *testing.T, body string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	t.Setenv("REAL_TEST_GIT", realGit)
	testWrite(t, bin, "git", "#!/bin/sh\n"+body+"\nexec \"$REAL_TEST_GIT\" \"$@\"\n", 0o700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func captureGit(t *testing.T) string {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "calls")
	t.Setenv("TEST_GIT_TRACE", trace)
	interceptGit(t, "{ printf '%s\\t' \"$@\"; printf '\\n'; } >> \"$TEST_GIT_TRACE\"")
	return trace
}

func gitCalls(t *testing.T, trace string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		calls = append(calls, strings.Split(strings.TrimSuffix(line, "\t"), "\t"))
	}
	return calls
}

func TestGitCallsUseSafeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish bool
	}{
		{name: "start"},
		{name: "after workload", finish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			var handle scruntime.Handle
			var state *worktreeState
			if tc.finish {
				handle = startTestTask(t, adapter, spec)
				var err error
				state, err = adapter.worktreeState(handle)
				if err != nil {
					t.Fatal(err)
				}
				testWrite(t, handle.WorkDir, "notes.txt", "task edit\n", 0o600)
			}
			trace := captureGit(t)
			if tc.finish {
				if err := adapter.Stop(context.Background(), handle); err != nil {
					t.Fatal(err)
				}
			} else {
				startTestTask(t, adapter, spec)
			}
			for _, args := range gitCalls(t, trace) {
				for _, config := range []string{
					"core.fsmonitor=false", "core.hooksPath=" + os.DevNull,
					"core.sshCommand=false", "protocol.file.allow=never", "core.excludesFile=",
				} {
					i := slices.Index(args, config)
					if i < 1 || args[i-1] != "-c" {
						t.Errorf("git %q lacks -c %s", args, config)
					}
				}
				if !tc.finish {
					continue
				}
				worktreeScoped := slices.Contains(args, "symbolic-ref") ||
					slices.Contains(args, "status") || slices.Contains(args, "add") ||
					slices.Contains(args, "commit") || slices.Contains(args, "ls-files") ||
					slices.Contains(args, "HEAD^{commit}")
				wantGitDir := state.CommonDir
				if slices.Contains(args, "--work-tree="+handle.WorkDir) {
					wantGitDir = state.GitDir
				}
				if len(args) < 2 || args[0] != "-C" || args[1] != state.CommonDir || !slices.Contains(args, "--git-dir="+wantGitDir) {
					t.Errorf("git %q lacks the recorded Git directory", args)
				}
				if worktreeScoped && wantGitDir != state.GitDir {
					t.Errorf("git %q lacks the recorded worktree", args)
				}
			}
		})
	}
}

func TestCleanupRefusesChangedGitEntry(t *testing.T) {
	for _, kind := range []string{"symlink", "replacement", "directory", "missing", "different target", "checkout symlink"} {
		t.Run(kind, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			handle := startTestTask(t, adapter, spec)
			testWrite(t, handle.WorkDir, "notes.txt", "task edit\n", 0o600)
			entry := filepath.Join(handle.WorkDir, ".git")
			raw, err := os.ReadFile(entry)
			if err != nil {
				t.Fatal(err)
			}
			saved := filepath.Join(t.TempDir(), "original")
			switch kind {
			case "different target":
				err = os.WriteFile(entry, []byte("gitdir: ../missing\n"), 0o600)
			case "checkout symlink":
				err = os.Rename(handle.WorkDir, saved)
				if err == nil {
					err = os.Symlink(saved, handle.WorkDir)
				}
			default:
				if err := os.Rename(entry, saved); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "symlink":
					err = os.Symlink(saved, entry)
				case "replacement":
					err = os.WriteFile(entry, raw, 0o600)
				case "directory":
					err = os.Mkdir(entry, 0o700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			trace := captureGit(t)
			err = adapter.Stop(context.Background(), handle)
			if err == nil || !strings.Contains(err.Error(), "coding worktree .git") {
				t.Errorf("Stop = %v, want a refusal about the coding worktree .git entry", err)
			}
			if _, err := os.Stat(trace); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Stop ran Git before refusing: %v", err)
			}
			if _, err := os.Stat(filepath.Join(handle.WorkDir, "notes.txt")); err != nil {
				t.Errorf("task edit was removed: %v", err)
			}
		})
	}
}

func TestGitCallsDropGitEnvironment(t *testing.T) {
	for _, name := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_CONFIG", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_FIXTURE",
	} {
		t.Run(name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			t.Setenv(name, "fixture")
			t.Setenv("TEST_GIT_KEEP", "fixture")
			check := fmt.Sprintf(`[ "${%s+x}" != x ]`, name)
			if name == "GIT_CONFIG_GLOBAL" || name == "GIT_CONFIG_NOSYSTEM" {
				check = ":"
			}
			interceptGit(t, check+` && [ "$GIT_CONFIG_GLOBAL" = /dev/null ] && [ "$GIT_CONFIG_NOSYSTEM" = 1 ] && [ "$TEST_GIT_KEEP" = fixture ] || exit 97`)
			handle := startTestTask(t, adapter, spec)
			if err := adapter.Stop(context.Background(), handle); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecordedCommitIDs(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("B", 64)
	for _, tc := range []struct {
		name, base, tip string
		finished, valid bool
	}{
		{name: "unfinished", base: sha1, valid: true},
		{name: "sha1", base: sha1, tip: sha1, finished: true, valid: true},
		{name: "sha256", base: sha256, tip: sha256, finished: true, valid: true},
		{name: "missing base"},
		{name: "abbreviated base", base: sha1[:12]},
		{name: "nonhex base", base: "g" + sha1[1:]},
		{name: "revision expression", base: "HEAD^{commit}"},
		{name: "option as base", base: "--output=fixture"},
		{name: "option as tip", base: sha1, tip: "--output=fixture"},
		{name: "space in tip", base: sha1, tip: sha1 + " "},
		{name: "missing finished tip", base: sha1, finished: true},
		{name: "mixed formats", base: sha1, tip: sha256, finished: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "worktree")
			state := &worktreeState{Base: tc.base, Tip: tc.tip}
			if tc.finished {
				state.FinishedAt = time.Now()
			}
			if err := writeWorktreeState(dir, state); err != nil {
				t.Fatal(err)
			}
			_, err := readWorktreeState(dir + stateSuffix)
			if (err == nil) != tc.valid {
				t.Errorf("readWorktreeState = %v, want accepted = %v", err, tc.valid)
			}
		})
	}
}

func TestGitRevisionsFollowEndOfOptions(t *testing.T) {
	for _, finish := range []bool{false, true} {
		t.Run(fmt.Sprintf("finish=%v", finish), func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			adapter.BranchRetention = time.Nanosecond
			handle := startTestTask(t, adapter, spec)
			trace := captureGit(t)
			if err := adapter.Stop(context.Background(), handle); err != nil {
				t.Fatal(err)
			}
			if !finish {
				spec.TaskID = "next-task"
				startTestTask(t, adapter, spec)
			}
			var checked int
			for _, args := range gitCalls(t, trace) {
				revParse := slices.Contains(args, "rev-parse") && slices.Contains(args, "--verify")
				artifactDiff := slices.Contains(args, "diff") && slices.Contains(args, "--no-color")
				ancestor := slices.Contains(args, "merge-base")
				branch := slices.Contains(args, "branch") && slices.Contains(args, "--no-track")
				if !revParse && !artifactDiff && !ancestor && !branch {
					continue
				}
				checked++
				i := slices.Index(args, "--end-of-options")
				if i < 0 || i >= len(args)-1 || args[i+1] == "--" {
					t.Errorf("git %q lacks a revision after --end-of-options", args)
				}
			}
			if checked == 0 {
				t.Error("no revision commands observed")
			}
		})
	}
}

func TestSessionOutputRedactsAcrossWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{name: "split password", chunks: []string{"start\npass", "word=fix", "ture-value\n"}, want: "start\npassword=[REDACTED:secret]\n"},
		{name: "split key block", chunks: []string{"start\n-----BE", "GIN PRIVATE KEY-----\n", "fixture body\n-----END PRIVATE", " KEY-----\nend\n"}, want: "start\n[REDACTED:private_key]\nend\n"},
		{name: "one line key", chunks: []string{"before -----BEGIN PRIVATE KEY-----fixture", " body-----END PRIVATE KEY----- after\n"}, want: "before [REDACTED:private_key] after\n"},
		{name: "oversized line", chunks: []string{strings.Repeat("x", maxSessionLine), "-----BEGIN PRIVATE KEY-----\n", "fixture body\n"}, want: "[REDACTED:output limit]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, err := os.CreateTemp(t.TempDir(), "session")
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			hub := attach.NewHub()
			hub.Register("task", nil)
			output := &sessionOutput{file: log, hub: hub, taskID: "task"}
			for _, chunk := range tc.chunks {
				if n, err := output.Write([]byte(chunk)); n != len(chunk) || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			got, err := os.ReadFile(log.Name())
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("log = %q, want %q", got, tc.want)
			}
			history, _, _, unsubscribe, err := hub.Subscribe("task")
			if err != nil {
				t.Fatal(err)
			}
			defer unsubscribe()
			if history != tc.want {
				t.Errorf("attach history = %q, want %q", history, tc.want)
			}
		})
	}
}

func TestSessionOutputFlushesOnExit(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		{name: "unterminated line", payload: "password=fixture-value", want: "password=[REDACTED:secret]"},
		{name: "unterminated key block", payload: "-----BEGIN PRIVATE KEY-----\nfixture body", want: "[REDACTED:private_key]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shellHarness(t, "printf '%s' '"+tc.payload+"'\n")
			adapter, spec := codingFixture(t)
			handle := startTestTask(t, adapter, spec)
			waitUntil(t, func() bool { return outputClosed(adapter.running[handle.WorkDir]) })
			got, err := os.ReadFile(handle.WorkDir + ".log")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("log = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCancelHandlesNestedRepositories(t *testing.T) {
	for _, tc := range []struct {
		name       string
		initModule bool
		commit     bool
	}{
		{name: "nested empty repository"},
		{name: "initialized submodule", initModule: true},
		{name: "nested repository with a commit", commit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			repo := spec.Workload.Spec.Coding.RepoPath
			if tc.initModule {
				source := initTempGitRepo(t)
				testGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--", source, "module")
				testGit(t, repo, "commit", "-m", "track module")
			}
			handle := startTestTask(t, adapter, spec)
			if tc.initModule {
				testGit(t, handle.WorkDir, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
			} else {
				nested := filepath.Join(handle.WorkDir, "nested[fixture]", "empty")
				if err := os.MkdirAll(nested, 0o700); err != nil {
					t.Fatal(err)
				}
				testGit(t, nested, "init")
				testWrite(t, nested, "uncommitted.txt", "nested edit\n", 0o600)
				if tc.commit {
					testGit(t, nested, "-c", "user.name=Fixture", "-c", "user.email=fixture",
						"commit", "--allow-empty", "-m", "keep nested commit")
				}
			}
			testWrite(t, handle.WorkDir, "notes.txt", "task edit\n", 0o600)
			if !tc.initModule && !tc.commit {
				// The unborn repository still has an untracked file. Rescue it
				// before retrying cleanup of the now empty repository.
				if err := adapter.Stop(context.Background(), handle); err == nil || !strings.Contains(err.Error(), "nested Git repository") {
					t.Fatalf("Stop with nested edit = %v", err)
				}
				file := filepath.Join(handle.WorkDir, "nested[fixture]", "empty", "uncommitted.txt")
				if err := os.Rename(file, filepath.Join(handle.WorkDir, "rescued.txt")); err != nil {
					t.Fatal(err)
				}
			}
			err := adapter.Stop(context.Background(), handle)
			if tc.commit {
				if err == nil || !strings.Contains(err.Error(), "nested Git repository") {
					t.Errorf("Stop = %v, want a nested repository refusal", err)
				}
				if _, err := os.Stat(filepath.Join(handle.WorkDir, "notes.txt")); err != nil {
					t.Errorf("task edit was removed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertWorktreeRemoved(t, repo, handle.WorkDir)
			if !tc.initModule && !tc.commit {
				if got := testGit(t, repo, "show", handle.Branch+":rescued.txt"); got != "nested edit\n" {
					t.Errorf("rescued nested edit = %q", got)
				}
			}
			if got := testGit(t, repo, "show", handle.Branch+":notes.txt"); got != "task edit\n" {
				t.Errorf("task edit = %q", got)
			}
		})
	}
}

func TestStopKillsWithDoneContext(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "already canceled"},
		{name: "deadline during grace period", timeout: 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shellHarness(t, "trap '' TERM\nprintf ready > ready.txt\nwhile :; do :; done\n")
			adapter, spec := codingFixture(t)
			handle := startTestTask(t, adapter, spec)
			done := adapter.running[handle.WorkDir]
			t.Cleanup(func() {
				if !outputClosed(done) {
					_ = syscall.Kill(-handle.PID, syscall.SIGKILL)
				}
			})
			waitUntil(t, func() bool {
				_, err := os.Stat(filepath.Join(handle.WorkDir, "ready.txt"))
				return err == nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			_ = adapter.Stop(ctx, handle)
			if !outputClosed(done) {
				t.Error("workload did not exit before Stop returned")
			}
			if err := syscall.Kill(-handle.PID, 0); err != syscall.ESRCH {
				t.Errorf("process group still exists after Stop: %v", err)
			}
		})
	}
}

func TestRollbackComparesAndDeletesBranch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		moveBranch bool
	}{
		{name: "owner HEAD diverged"},
		{name: "task branch advanced", moveBranch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			repo := spec.Workload.Spec.Coding.RepoPath
			ctx := context.Background()
			dir, state, err := adapter.prepareWorktree(ctx, repo, spec.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			var want string
			if tc.moveBranch {
				testGit(t, dir, "commit", "--allow-empty", "-m", "advance task branch")
				want = testGit(t, dir, "rev-parse", "HEAD")
			}
			testGit(t, repo, "switch", "--orphan", "diverged")
			testGit(t, repo, "commit", "--allow-empty", "-m", "replace owner HEAD")
			if err := rollbackWorktree(ctx, dir, state); err != nil {
				t.Fatal(err)
			}
			assertWorktreeRemoved(t, repo, dir)
			if want == "" {
				if got := testGit(t, repo, "branch", "--list", state.Branch); got != "" {
					t.Errorf("rollback left branch %q", got)
				}
			} else if got := testGit(t, repo, "rev-parse", state.Branch); got != want {
				t.Errorf("advanced branch = %q, want %q", got, want)
			}
		})
	}
}

func TestSnapshotRefusesConfiguredFilters(t *testing.T) {
	for _, kind := range []string{"clean", "process"} {
		t.Run(kind, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			repo := spec.Workload.Spec.Coding.RepoPath
			marker := filepath.Join(t.TempDir(), "filter-ran")
			t.Setenv("TEST_FILTER_MARKER", marker)
			testGit(t, repo, "config", "filter.fixture."+kind, `touch "$TEST_FILTER_MARKER"; cat`)
			handle := startTestTask(t, adapter, spec)
			testWrite(t, handle.WorkDir, ".gitattributes", "notes.txt filter=fixture\n", 0o600)
			testWrite(t, handle.WorkDir, "notes.txt", "task edit\n", 0o600)
			if err := adapter.Stop(context.Background(), handle); err == nil {
				t.Error("Stop accepted an edit that requires a configured filter")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("configured filter ran: %v", err)
			}
		})
	}
}
