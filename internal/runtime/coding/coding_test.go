package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func initTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	run("init")
	run("config", "user.email", "sablecourt-test")
	run("config", "user.name", "Sablecourt Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o600); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}

func TestCodingAdapterCreatesWorktreeAndStripsEnv(t *testing.T) {
	ctx := context.Background()
	repoDir := initTempGitRepo(t)
	wtRoot := filepath.Join(t.TempDir(), "worktrees")

	adapter := NewAdapter(wtRoot)
	adapter.DryRun = true

	spec := scruntime.LaunchSpec{
		TaskID: "task-42",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "go-coder", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{
					Harness:  "claude",
					RepoPath: repoDir,
					Sandbox:  "seatbelt",
				},
			},
		},
		ModelGateURL:   "model-gate",
		ModelGateToken: "sc_task_abcdef",
	}

	handle, err := adapter.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle.Branch != "sablecourt/task-42" {
		t.Fatalf("unexpected branch %q", handle.Branch)
	}
	if _, err := os.Stat(filepath.Join(handle.WorkDir, "README.md")); err != nil {
		t.Fatalf("expected README.md in worktree: %v", err)
	}

	env := BuildStrippedEnv(spec)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "SABLECOURT_MANAGED=1") || !strings.Contains(joined, "SABLECOURT_TASK_ID=task-42") {
		t.Fatalf("missing Nightloom starter metadata in env:\n%s", joined)
	}
	for _, forbidden := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GITHUB_TOKEN"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unexpected raw secret env %s in stripped env", forbidden)
		}
	}

}

func codingFixture(t *testing.T) (*Adapter, scruntime.LaunchSpec) {
	t.Helper()
	repo := initTempGitRepo(t)
	adapter := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	adapter.allowUnconfined = true
	return adapter, scruntime.LaunchSpec{
		TaskID: "task-test",
		Workload: domain.Workload{Spec: domain.WorkloadSpec{
			Coding: &domain.CodingConfig{RepoPath: repo, Harness: "claude", Sandbox: "none"},
		}},
	}
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return string(out)
}

func testWrite(t *testing.T, dir, name, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func fakeHarness(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	testWrite(t, dir, "claude", body, 0o700)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shellHarness(t *testing.T, body string) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	fakeHarness(t, "#!"+shell+"\n"+body)
}

func startTestTask(t *testing.T, adapter *Adapter, spec scruntime.LaunchSpec) scruntime.Handle {
	t.Helper()
	handle, err := adapter.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func assertWorktreeRemoved(t *testing.T, repo, worktree string) {
	t.Helper()
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree directory remains: %v", err)
	}
	if list := testGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Errorf("worktree list still has a task entry:\n%s", list)
	}
}

func TestCancelPreservesWorktreeEdits(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		staged     bool
	}{
		{name: "unstaged tracked edit", file: "README.md"},
		{name: "unstaged new file", file: "notes.txt"},
		{name: "staged new file", file: "notes.txt", staged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			handle := startTestTask(t, adapter, spec)
			testWrite(t, handle.WorkDir, tc.file, "task edit\n", 0o600)
			if tc.staged {
				testGit(t, handle.WorkDir, "add", "--", tc.file)
			}
			for range 2 {
				if err := adapter.Stop(context.Background(), handle); err != nil {
					t.Fatal(err)
				}
			}
			repo := spec.Workload.Spec.Coding.RepoPath
			assertWorktreeRemoved(t, repo, handle.WorkDir)
			if got := testGit(t, repo, "show", handle.Branch+":"+tc.file); got != "task edit\n" {
				t.Errorf("task branch content = %q", got)
			}
		})
	}
}

func TestCompletionRemovesWorktree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty bool
	}{
		{name: "clean"},
		{name: "dirty", dirty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			handle := startTestTask(t, adapter, spec)
			if tc.dirty {
				testWrite(t, handle.WorkDir, "notes.txt", "task edit\n", 0o600)
			}
			for range 2 {
				obs, err := adapter.Observe(context.Background(), handle)
				if err != nil || obs.State != scruntime.StateCompleted {
					t.Fatalf("Observe = %v, %v", obs, err)
				}
			}
			repo := spec.Workload.Spec.Coding.RepoPath
			assertWorktreeRemoved(t, repo, handle.WorkDir)
			testGit(t, repo, "rev-parse", "--verify", handle.Branch)
			if tc.dirty {
				testGit(t, repo, "show", handle.Branch+":notes.txt")
			}
		})
	}
}

func TestTaskSessionFilesAreExcludedLocally(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{name: "session marker"},
		{name: "session marker and context", input: `{"context_packet":"task context"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shellHarness(t, "exit 0\n")
			adapter, spec := codingFixture(t)
			spec.InputJSON = tc.input
			handle := startTestTask(t, adapter, spec)
			for _, file := range []string{".sablecourt-session.json", ".sablecourt-context.md"} {
				if file == ".sablecourt-context.md" && tc.input == "" {
					continue
				}
				if _, err := os.Stat(filepath.Join(handle.WorkDir, file)); err != nil {
					t.Fatal(err)
				}
				testWrite(t, spec.Workload.Spec.Coding.RepoPath, file, "owner file\n", 0o600)
			}
			if status := testGit(t, handle.WorkDir, "status", "--short"); status != "" {
				t.Errorf("worktree status = %q", status)
			}
			testGit(t, handle.WorkDir, "add", "-A")
			if staged := testGit(t, handle.WorkDir, "diff", "--cached", "--name-only"); staged != "" {
				t.Errorf("staged session files: %q", staged)
			}
			ownerStatus := testGit(t, spec.Workload.Spec.Coding.RepoPath, "status", "--short")
			if !strings.Contains(ownerStatus, "?? .sablecourt-session.json") {
				t.Errorf("owner status = %q", ownerStatus)
			}
		})
	}
}

func TestArtifactDiffUsesLaunchBase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		commitNew bool
		observe   bool
	}{
		{name: "tracked commit and new worktree file"},
		{name: "tracked commit and committed new file after observation", commitNew: true, observe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			handle := startTestTask(t, adapter, spec)
			repo := spec.Workload.Spec.Coding.RepoPath
			testWrite(t, repo, "base-only.txt", "later owner commit\n", 0o600)
			testGit(t, repo, "add", "--", "base-only.txt")
			testGit(t, repo, "commit", "-m", "advance base")
			testWrite(t, handle.WorkDir, "README.md", "committed edit\n", 0o600)
			testGit(t, handle.WorkDir, "add", "--", "README.md")
			testGit(t, handle.WorkDir, "commit", "-m", "edit readme")
			testWrite(t, handle.WorkDir, "notes.txt", "new file text\n", 0o600)
			if tc.commitNew {
				testGit(t, handle.WorkDir, "add", "--", "notes.txt")
				testGit(t, handle.WorkDir, "commit", "-m", "add notes")
			}
			if tc.observe {
				if _, err := adapter.Observe(context.Background(), handle); err != nil {
					t.Fatal(err)
				}
			}
			artifacts, err := adapter.CollectArtifacts(context.Background(), handle)
			if err != nil {
				t.Fatal(err)
			}
			var diff string
			for _, artifact := range artifacts {
				if artifact.Name == "git-diff" {
					diff = artifact.Text
				}
			}
			if !strings.Contains(diff, "+committed edit") || !strings.Contains(diff, "+new file text") ||
				strings.Contains(diff, "base-only.txt") || strings.Contains(diff, ".sablecourt-") {
				t.Errorf("git-diff = %q", diff)
			}
		})
	}
}

func TestFailedStartRemovesWorktreeAndBranch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		attach bool
	}{
		{name: "detached"},
		{name: "attached", attach: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHarness(t, "#!"+filepath.Join(t.TempDir(), "missing-interpreter")+"\n")
			adapter, spec := codingFixture(t)
			if tc.attach {
				adapter.AttachHub = attach.NewHub()
			}
			_, err := adapter.Start(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), "start coding harness") {
				t.Fatalf("expected process start error, got %v", err)
			}
			repo := spec.Workload.Spec.Coding.RepoPath
			worktree := filepath.Join(adapter.WorktreeRoot, spec.TaskID)
			assertWorktreeRemoved(t, repo, worktree)
			if branches := testGit(t, repo, "branch", "--list", "sablecourt/"+spec.TaskID); branches != "" {
				t.Errorf("failed start left a branch: %q", branches)
			}
			if _, err := os.Stat(worktree + ".log"); !os.IsNotExist(err) {
				t.Errorf("failed start left a session log: %v", err)
			}
		})
	}
}

func TestStopWaitsForProcessGroup(t *testing.T) {
	for _, tc := range []struct {
		name, trap string
		savesEdit  bool
	}{
		{name: "waits for termination handler", trap: `trap 'printf final > final.txt; exit 0' TERM`, savesEdit: true},
		{name: "kills process that ignores termination", trap: `trap '' TERM`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shellHarness(t, tc.trap+"\nprintf ready > ready.txt\nwhile :; do :; done\n")
			adapter, spec := codingFixture(t)
			adapter.AttachHub = attach.NewHub()
			handle := startTestTask(t, adapter, spec)
			t.Cleanup(func() { _ = syscall.Kill(-handle.PID, syscall.SIGKILL) })
			waitUntil(t, func() bool {
				_, err := os.Stat(filepath.Join(handle.WorkDir, "ready.txt"))
				return err == nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := adapter.Stop(ctx, handle); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(-handle.PID, 0); err != syscall.ESRCH {
				t.Errorf("process group still exists after Stop: %v", err)
			}
			if tc.savesEdit {
				testGit(t, spec.Workload.Spec.Coding.RepoPath, "show", handle.Branch+":final.txt")
			}
		})
	}
}

func waitUntil(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for workload")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSessionLogRedaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		attach bool
	}{
		{name: "detached"},
		{name: "attached", attach: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := "password=fixture-value"
			shellHarness(t, "printf '%s\\n' '"+payload+"'\n")
			adapter, spec := codingFixture(t)
			if tc.attach {
				adapter.AttachHub = attach.NewHub()
			}
			handle := startTestTask(t, adapter, spec)
			var log []byte
			waitUntil(t, func() bool {
				log, _ = os.ReadFile(handle.WorkDir + ".log")
				return len(log) > 0
			})
			if strings.Contains(string(log), payload) || !strings.Contains(string(log), "[REDACTED:secret]") {
				t.Errorf("session log = %q", log)
			}
		})
	}
}

func TestCleanupRefusesWrongBranch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		switchArgs []string
	}{
		{name: "other branch", switchArgs: []string{"switch", "-c", "other"}},
		{name: "detached head", switchArgs: []string{"switch", "--detach"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			handle := startTestTask(t, adapter, spec)
			testGit(t, handle.WorkDir, tc.switchArgs...)
			testWrite(t, handle.WorkDir, "notes.txt", "task edit", 0o600)
			if err := adapter.Stop(context.Background(), handle); err == nil {
				t.Error("cleanup accepted a worktree without its task branch checked out")
			}
			if _, err := os.Stat(filepath.Join(handle.WorkDir, "notes.txt")); err != nil {
				t.Errorf("task edit was removed: %v", err)
			}
		})
	}
}

func TestFinishedBranchRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention time.Duration
		merge     bool
		keep      bool
	}{
		{name: "expired and merged", retention: time.Nanosecond, merge: true},
		{name: "expired and unmerged", retention: time.Nanosecond, keep: true},
		{name: "within retention", retention: time.Hour, merge: true, keep: true},
		{name: "disabled", merge: true, keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			adapter.BranchRetention = tc.retention
			handle := startTestTask(t, adapter, spec)
			testWrite(t, handle.WorkDir, "notes.txt", "task edit", 0o600)
			testGit(t, handle.WorkDir, "add", "--", "notes.txt")
			testGit(t, handle.WorkDir, "commit", "-m", "add notes")
			if _, err := adapter.Observe(context.Background(), handle); err != nil {
				t.Fatal(err)
			}
			repo := spec.Workload.Spec.Coding.RepoPath
			if tc.merge {
				testGit(t, repo, "merge", "--ff-only", handle.Branch)
			}
			spec.TaskID = "next-task"
			startTestTask(t, adapter, spec)
			branchExists := testGit(t, repo, "branch", "--list", handle.Branch) != ""
			if branchExists != tc.keep {
				t.Errorf("previous branch exists = %v, want %v", branchExists, tc.keep)
			}
		})
	}
}

func TestStartRefusesSessionFileConflicts(t *testing.T) {
	for _, tc := range []struct {
		name, file, content string
	}{
		{name: "tracked marker", file: ".sablecourt-session.json", content: "owner data\n"},
		{name: "tracked context", file: ".sablecourt-context.md", content: "owner context\n"},
		{name: "ignore negation", file: ".gitignore", content: "!/.sablecourt-session.json\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			repo := spec.Workload.Spec.Coding.RepoPath
			testWrite(t, repo, tc.file, tc.content, 0o600)
			testGit(t, repo, "add", "--", tc.file)
			testGit(t, repo, "commit", "-m", "add owner file")
			_, err := adapter.Start(context.Background(), spec)
			if err == nil {
				t.Error("start accepted conflicting session file")
			}
			assertWorktreeRemoved(t, repo, filepath.Join(adapter.WorktreeRoot, spec.TaskID))
			if branches := testGit(t, repo, "branch", "--list", "sablecourt/"+spec.TaskID); branches != "" {
				t.Errorf("failed start left a branch: %q", branches)
			}
			if got := testGit(t, repo, "show", "HEAD:"+tc.file); got != tc.content {
				t.Errorf("owner file content = %q", got)
			}
		})
	}
}

func TestCancelPreservesIndexAndWorktreeVersions(t *testing.T) {
	for _, tc := range []struct {
		name, file string
	}{
		{name: "tracked edit", file: "README.md"},
		{name: "new file", file: "notes.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, spec := codingFixture(t)
			adapter.DryRun = true
			handle := startTestTask(t, adapter, spec)
			testWrite(t, handle.WorkDir, tc.file, "staged version\n", 0o600)
			testGit(t, handle.WorkDir, "add", "--", tc.file)
			testWrite(t, handle.WorkDir, tc.file, "worktree version\n", 0o600)
			if err := adapter.Stop(context.Background(), handle); err != nil {
				t.Fatal(err)
			}
			repo := spec.Workload.Spec.Coding.RepoPath
			if got := testGit(t, repo, "show", handle.Branch+":"+tc.file); got != "worktree version\n" {
				t.Errorf("worktree version = %q", got)
			}
			if got := testGit(t, repo, "show", handle.Branch+"^:"+tc.file); got != "staged version\n" {
				t.Errorf("staged version = %q", got)
			}
		})
	}
}

func TestHarnessFinalResponse(t *testing.T) {
	for _, tc := range []struct{ name, command, want string }{
		{"answer", "printf 'review complete\\nVERDICT: PASS\\n'; printf 'diagnostic\\n' >&2", "review complete\nVERDICT: PASS"},
		{"redacted", "printf pass; printf 'word=fixture-value\\n'", "password=[REDACTED:secret]"},
		{"unfinished key", "printf '%s\\n' '-----BEGIN PRIVATE KEY-----' 'fixture payload'", ""},
		{"stderr only", "printf 'VERDICT: PASS\\n' >&2", ""},
		{"failed", "printf 'VERDICT: PASS\\n'; exit 1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shellHarness(t, tc.command)
			a, spec := codingFixture(t)
			h := startTestTask(t, a, spec)
			waitUntil(t, func() bool { return outputClosed(a.process(h.WorkDir).done) })
			if _, err := a.Observe(context.Background(), h); err != nil {
				t.Fatal(err)
			}
			arts, err := NewAdapter(a.WorktreeRoot).CollectArtifacts(context.Background(), h)
			if err != nil {
				t.Fatal(err)
			}
			var final string
			for _, art := range arts {
				if art.Name == "final-message" {
					final = strings.TrimRight(art.Text, "\n")
				}
			}
			if final != tc.want {
				t.Fatalf("final response = %q", final)
			}
		})
	}
}

func TestPinnedWorktrees(t *testing.T) {
	ctx := context.Background()
	a, spec := codingFixture(t)
	a.DryRun = true
	h := startTestTask(t, a, spec)
	testWrite(t, h.WorkDir, "change", "author content", 0o600)
	if err := a.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	tip := strings.TrimSpace(testGit(t, spec.Workload.Spec.Coding.RepoPath, "rev-parse", h.Branch))
	arts, err := a.CollectArtifacts(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	var saved string
	for _, art := range arts {
		if art.Name == "git-commit" {
			saved = art.Text
		}
	}
	if saved != tip {
		t.Fatalf("recorded author commit = %q, want %q", saved, tip)
	}
	testGit(t, spec.Workload.Spec.Coding.RepoPath, "branch", "-f", h.Branch, "HEAD")
	for _, tc := range []struct {
		name, commit    string
		readOnly, valid bool
	}{
		{"verifier", tip, true, true}, {"remediation", tip, false, true}, {"missing", strings.Repeat("0", 40), true, false}, {"mutable ref", "HEAD", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec.TaskID = tc.name
			h, err := a.StartAtCommit(ctx, spec, tc.commit, tc.readOnly)
			if !tc.valid {
				if err == nil {
					t.Fatal("accepted invalid pin")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(h.WorkDir, "change"))
			if err != nil || string(got) != "author content" || strings.TrimSpace(testGit(t, h.WorkDir, "rev-parse", "HEAD")) != tip {
				t.Fatalf("author content=%q, err=%v", got, err)
			}
			if !tc.readOnly {
				testWrite(t, h.WorkDir, "change", "remediation", 0o600)
			}
			if err := a.Stop(ctx, h); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifierSandbox(t *testing.T) {
	a, spec := codingFixture(t)
	a.DryRun = true
	h := startTestTask(t, a, spec)
	state, err := a.worktreeState(h)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	bin, err := resolvedPath("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := seatbeltProfile(h.WorkDir, scratch, bin, state, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(profile, "\n") {
		if strings.Contains(line, "file-write") && (strings.Contains(line, h.WorkDir) || strings.Contains(line, state.CommonDir)) {
			t.Fatalf("verifier can write checkout or Git metadata: %s", line)
		}
	}
	for _, sandbox := range []string{"", "none", "unsupported"} {
		if err := a.validateSandbox(sandbox, true); err == nil {
			t.Fatalf("verifier accepted sandbox %q", sandbox)
		}
	}
}

func TestVerifierCannotWriteCheckout(t *testing.T) {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput(); err != nil {
			if strings.Contains(string(out), "Operation not permitted") {
				t.Skip("nested Seatbelt denied by sandbox")
			}
			t.Fatalf("Seatbelt probe: %v (%s)", err, out)
		}
	default:
		t.Skip("no native verifier sandbox")
	}
	shellHarness(t, `if test "$(cat change)" = "author content" && ! (echo modified >change) && ! touch new-file; then echo "verified read-only"; fi`)
	a, spec := codingFixture(t)
	a.DryRun = true
	h := startTestTask(t, a, spec)
	testWrite(t, h.WorkDir, "change", "author content", 0o600)
	if err := a.Stop(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	tip := strings.TrimSpace(testGit(t, spec.Workload.Spec.Coding.RepoPath, "rev-parse", h.Branch))
	spec.TaskID = "check"
	spec.Workload.Spec.Coding.Sandbox = "seatbelt"
	a.DryRun = false
	h, err := a.StartAtCommit(context.Background(), spec, tip, true)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return outputClosed(a.process(h.WorkDir).done) })
	arts, err := a.CollectArtifacts(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	for _, art := range arts {
		if art.Name == "final-message" && strings.TrimSpace(art.Text) == "verified read-only" {
			return
		}
	}
	t.Fatal("verifier did not confirm a read-only checkout")
}
