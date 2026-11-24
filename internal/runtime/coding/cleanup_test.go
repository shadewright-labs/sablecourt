package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

func TestStopChecksGitdirBeforeGit(t *testing.T) {
	for _, change := range []string{"config.worktree", "replaced exclude", "edited exclude", "exclude selector", "commondir", "gitdir"} {
		t.Run(change, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			if change == "config.worktree" {
				testGit(t, spec.Workload.Spec.Coding.RepoPath, "config", "extensions.worktreeConfig", "true")
			}
			handle := startTestTask(t, a, spec)
			s, err := a.worktreeState(handle)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "hook-ran")
			switch change {
			case "config.worktree":
				testWrite(t, s.GitDir, change, "[core]\n\thooksPath = "+filepath.Dir(marker)+"\n", 0o600)
				testWrite(t, filepath.Dir(marker), "pre-commit", "#!/bin/sh\ntouch \"$TEST_HOOK_MARKER\"\n", 0o700)
				t.Setenv("TEST_HOOK_MARKER", marker)
			case "replaced exclude":
				file := filepath.Join(s.GitDir, "info", "exclude")
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(file, file+".old"); err != nil {
					t.Fatal(err)
				}
				testWrite(t, filepath.Dir(file), "exclude", string(data), 0o600)
			case "exclude selector":
				testWrite(t, filepath.Dir(handle.WorkDir), filepath.Base(handle.WorkDir)+excludeConfigSuffix, "[invalid\n", 0o600)
			case "edited exclude":
				testWrite(t, s.GitDir, "info/exclude", "*\n", 0o600)
			default:
				testWrite(t, s.GitDir, change, "../invalid\n", 0o600)
			}
			testWrite(t, handle.WorkDir, "notes", "keep", 0o600)
			trace := captureGit(t)
			if err := a.Stop(context.Background(), handle); err == nil {
				t.Error("Stop accepted changed gitdir")
			}
			for _, file := range []string{trace, marker} {
				if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s exists or inaccessible: %v", filepath.Base(file), err)
				}
			}
		})
	}
}

func TestSnapshotIgnoresExternalExcludes(t *testing.T) {
	for _, scope := range []string{"local", "global"} {
		t.Run(scope, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			repo := spec.Workload.Spec.Coding.RepoPath
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			testWrite(t, dir, "ignore", "notes\n", 0o600)
			testGit(t, repo, "config", "--"+scope, "core.excludesFile", filepath.Join(dir, "ignore"))
			handle := startTestTask(t, a, spec)
			if config := testGit(t, repo, "config", "--local", "--list"); strings.Contains(config, "extensions.worktreeconfig=true") {
				t.Error("Start enabled worktree config")
			}
			testWrite(t, handle.WorkDir, "notes", "keep", 0o600)
			if err := a.Stop(context.Background(), handle); err != nil {
				t.Fatal(err)
			}
			if got := testGit(t, repo, "show", handle.Branch+":notes"); got != "keep" {
				t.Errorf("snapshot = %q", got)
			}
		})
	}
}

func TestCleanupKeepsUnsavedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		finished   bool
	}{
		{name: "ignored before snapshot", path: "drafts/notes"},
		{name: "ignored after snapshot", path: "drafts/notes", finished: true},
		{name: "untracked after snapshot", path: "notes", finished: true},
		{name: "tracked after snapshot", path: "README.md", finished: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			repo := spec.Workload.Spec.Coding.RepoPath
			testWrite(t, repo, ".gitignore", "drafts/\n", 0o600)
			testGit(t, repo, "add", ".gitignore")
			testGit(t, repo, "commit", "-m", "ignore drafts")
			h := startTestTask(t, a, spec)
			if tc.finished {
				state, err := a.worktreeState(h)
				if err != nil {
					t.Fatal(err)
				}
				if err := removeSessionFiles(t.Context(), state.checkoutGit(h.WorkDir), h.WorkDir); err != nil {
					t.Fatal(err)
				}
				state.Tip = state.Base
				state.FinishedAt = time.Now().UTC()
				if err := writeWorktreeState(h.WorkDir, state); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(h.WorkDir, tc.path)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			testWrite(t, h.WorkDir, tc.path, "only copy\n", 0o600)
			if err := a.Stop(t.Context(), h); err == nil || !strings.Contains(err.Error(), "unsaved") {
				t.Errorf("cleanup = %v, want refusal for unsaved files", err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "only copy\n" {
				t.Errorf("unsaved file = %q, %v", got, err)
			}
			if state, err := a.worktreeState(h); err != nil || state.CleanupError == "" {
				t.Errorf("missing recorded cleanup error: %v", err)
			}
			if tc.path == "README.md" {
				testGit(t, h.WorkDir, "restore", "--", tc.path)
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := a.Stop(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			assertWorktreeRemoved(t, repo, h.WorkDir)
		})
	}
}

func TestSessionOutputHoldsCredentialPrefix(t *testing.T) {
	for _, tc := range []struct{ name, prefix, next string }{
		{"password", "log: password=\n", "fixture-value\n"},
		{"bearer", "Authorization: Bearer\r\n", "fixture-token\n"},
		{"split bearer prefix", "Authorization:\n", "Bearer\nfixture-token\n"},
		{"blank line", "client_api-key :\n", "\n  fixture-value"},
		{"eof", "access_token=\n", ""},
		{"bound", "password=\n", strings.Repeat("x", maxSessionLine)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, err := os.CreateTemp(t.TempDir(), "output")
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			out := sessionOutput{file: log}
			if _, err := out.Write([]byte(tc.prefix)); err != nil {
				t.Fatal(err)
			}
			if st, err := log.Stat(); err != nil || st.Size() != 0 {
				t.Error("credential prefix was emitted before its value")
			}
			if _, err := out.Write([]byte(tc.next)); err != nil {
				t.Fatal(err)
			}
			if err := out.Flush(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log.Name())
			if err != nil {
				t.Fatal(err)
			}
			want := redact.String(tc.prefix + tc.next)
			if tc.name == "bound" {
				want = "[REDACTED:output limit]\n"
			}
			if string(data) != want {
				t.Errorf("redacted output differs, length %d, want %d", len(data), len(want))
			}
		})
	}
}

func TestCleanupPreservesNestedWork(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		module       bool
	}{
		{"modified", "uncommitted", true},
		{"untracked", "uncommitted", true},
		{"staged edit", "staged", true},
		{"new commit", "checked out commit", true},
		{"side commit", "unreachable", true},
		{"locally committed gitlink", "launch snapshot", true},
		{"bare repository", "bare Git repository", false},
		{"staged repository", "staged as a gitlink", false},
		{"ignored repository", "nested Git repository", false},
		{"unborn with edits", "nested Git repository", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			repo := spec.Workload.Spec.Coding.RepoPath
			if tc.module {
				testGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--", initTempGitRepo(t), "child")
				testGit(t, repo, "commit", "-m", "track child")
			}
			h := startTestTask(t, a, spec)
			child := filepath.Join(h.WorkDir, "child")
			if tc.module {
				testGit(t, h.WorkDir, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
			} else {
				if err := os.Mkdir(child, 0o700); err != nil {
					t.Fatal(err)
				}
				if tc.name == "bare repository" {
					testGit(t, child, "clone", "--bare", repo, ".")
				} else {
					testGit(t, child, "init")
				}
			}
			switch tc.name {
			case "modified", "staged edit":
				testWrite(t, child, "README.md", "keep", 0o600)
				if tc.name == "staged edit" {
					testGit(t, child, "add", "README.md")
				}
			case "bare repository":
				testWrite(t, h.WorkDir, ".gitignore", "child/\n", 0o600)
			case "untracked", "unborn with edits":
				testWrite(t, child, "notes", "keep", 0o600)
			default:
				testGit(t, child, "-c", "user.name=Fixture", "-c", "user.email=fixture", "commit", "--allow-empty", "-m", "preserve")
				switch tc.name {
				case "locally committed gitlink":
					testGit(t, h.WorkDir, "add", "child")
					testGit(t, h.WorkDir, "commit", "-m", "update module")
				case "side commit":
					testGit(t, child, "branch", "side")
					testGit(t, child, "checkout", "--detach", "HEAD^")
				case "staged repository":
					testGit(t, h.WorkDir, "add", "child")
				case "ignored repository":
					testWrite(t, h.WorkDir, ".gitignore", "child/\n", 0o600)
				}
			}
			err := a.Stop(context.Background(), h)
			if err == nil || !strings.Contains(err.Error(), "child") || !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("Stop = %v, want refusal naming child and %s", err, tc.reason)
			}
			if _, err := os.Stat(child); err != nil {
				t.Errorf("child was removed: %v", err)
			}
			state, err := a.worktreeState(h)
			if err != nil || !strings.Contains(state.CleanupError, tc.reason) || !state.FinishedAt.IsZero() {
				t.Errorf("missing incomplete cleanup state for %q: %v", tc.reason, err)
			}
		})
	}
}

func TestCleanupCleanSubmodule(t *testing.T) {
	for _, editParent := range []bool{false, true} {
		name := "unchanged parent"
		if editParent {
			name = "modified parent"
		}
		t.Run(name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			repo := spec.Workload.Spec.Coding.RepoPath
			testGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--", initTempGitRepo(t), "child")
			testGit(t, repo, "commit", "-m", "track child")
			h := startTestTask(t, a, spec)
			testGit(t, h.WorkDir, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
			gitdir := strings.TrimSpace(testGit(t, filepath.Join(h.WorkDir, "child"), "rev-parse", "--absolute-git-dir"))
			testWrite(t, gitdir, "config", "[invalid\n", 0o600)
			if editParent {
				testWrite(t, h.WorkDir, "notes", "keep", 0o600)
			}
			if err := a.Stop(context.Background(), h); err != nil {
				t.Fatal(err)
			}
			assertWorktreeRemoved(t, repo, h.WorkDir)
			if editParent && testGit(t, repo, "show", h.Branch+":notes") != "keep" {
				t.Error("missing parent edit")
			}
		})
	}
}

func TestStopRechecksRegistrationBetweenCalls(t *testing.T) {
	for _, name := range []string{"info/exclude", "config.worktree"} {
		t.Run(name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			h := startTestTask(t, a, spec)
			s, err := a.worktreeState(h)
			if err != nil {
				t.Fatal(err)
			}
			trace := filepath.Join(t.TempDir(), "calls")
			t.Setenv("TEST_TRACE", trace)
			t.Setenv("TEST_CHANGE", filepath.Join(s.GitDir, name))
			interceptGit(t, `echo git >> "$TEST_TRACE"
"$REAL_TEST_GIT" "$@" || exit $?
echo '# changed' >> "$TEST_CHANGE"
exit $?`)
			if err := a.Stop(context.Background(), h); err == nil {
				t.Error("Stop accepted a changed registration")
			}
			data, err := os.ReadFile(trace)
			if err != nil || string(data) != "git\n" {
				t.Errorf("expected one Git call, got %q, %v", data, err)
			}
		})
	}
}
