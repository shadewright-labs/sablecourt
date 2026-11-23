package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	run("config", "user.email", "test@sablecourt.local")
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
		ModelGateURL:   "http://127.0.0.1:7432/gate/model",
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

	profile := SeatbeltProfile(handle.WorkDir)
	if !strings.Contains(profile, "(deny file-write*)") || !strings.Contains(profile, handle.WorkDir) {
		t.Fatalf("unexpected Seatbelt profile:\n%s", profile)
	}
}
