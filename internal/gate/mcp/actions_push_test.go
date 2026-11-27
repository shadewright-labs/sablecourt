package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func initGitRepoWithBareRemote(t *testing.T, branch string) (repoDir, bareDir, commitSHA string) {
	t.Helper()
	repoDir = initGitRepo(t)
	bareDir = filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", bareDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v (%s)", err, string(out))
	}
	runGit(t, repoDir, "remote", "add", "origin", bareDir)
	runGit(t, repoDir, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repoDir, "change.txt"), []byte("task change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "change.txt")
	runGit(t, repoDir, "commit", "-m", "task work")
	commitSHA = runGit(t, repoDir, "rev-parse", "HEAD")
	return repoDir, bareDir, commitSHA
}

func setupGitPushGate(t *testing.T, repoDir string, wl domain.Workload, effect domain.PolicyEffect, nowFn func() time.Time) (*Gate, *sqlite.SQLiteStore, *approval.Queue, string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{
			"default": {
				Metadata: domain.ObjectMeta{Name: "default"},
				Spec: domain.PolicySpec{
					DefaultEffect: domain.EffectDeny,
					Rules: []domain.PolicyRule{
						{Workload: wl.Metadata.Name, Capability: "action:git.push", Effect: effect},
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{wl.Metadata.Name: wl},
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: wl.Metadata.Name, Zone: wl.Metadata.Zone, Class: wl.Spec.Class,
		ManifestDigest: "m1", AgentCardJSON: "{}",
	})
	_ = st.CreateTask(ctx, domain.Task{
		ID: "task-push-1", IdempotencyKey: "idem-push-1", WorkloadName: wl.Metadata.Name, State: domain.TaskStateWorking,
	})
	token, err := model.MintTaskToken(ctx, st, "task-push-1", wl.Metadata.Name, time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	eng := policy.NewEngine([]domain.Policy{bundle.Policies["default"]}, nil)
	queue := approval.NewQueue(st, autoApprover{})
	gate := New(Options{
		Store:     st,
		Bundle:    bundle,
		Policy:    eng,
		Approvals: queue,
		Now:       nowFn,
	})
	RegisterBuiltinGitOpsWithOptions(gate, GitOpsOptions{RepoRoot: repoDir})
	return gate, st, queue, token
}

func TestGitPushApprovedFlowAndCommitPinning(t *testing.T) {
	ctx := context.Background()
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)

	wl := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class:   domain.ClassCodingAgent,
			Runtime: domain.RuntimeConfig{Type: "worktree-cli"},
			Coding:  &domain.CodingConfig{Harness: "claude", RepoPath: repoDir},
			Grants:  domain.GrantSet{Actions: []string{"git.push"}},
		},
	}
	// Even with policy allow, git.push catalog requires out-of-band approval.
	gate, _, queue, token := setupGitPushGate(t, repoDir, wl, domain.EffectAllow, nil)
	h := gate.ActionHandler()

	payload := `{"branch":"` + branch + `","commit_sha":"` + commitSHA + `"}`
	// Missing approval must pause with 202.
	rec := postAction(t, h, http.MethodPost, "/git.push", token, `{"payload":`+payload+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 approval_required, got %d: %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Approve(ctx, pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Execute with approved ID pushes exact commit to bare remote.
	rec = postAction(t, h, http.MethodPost, "/git.push", token, `{"approvalId":"`+pending.ApprovalID+`","payload":`+payload+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 push, got %d: %s", rec.Code, rec.Body.String())
	}
	pushedSHA := runGit(t, bareDir, "rev-parse", "refs/heads/"+branch)
	if pushedSHA != commitSHA {
		t.Fatalf("remote branch SHA = %q, want %q", pushedSHA, commitSHA)
	}

	// Reusing the same approval must fail closed.
	rec = postAction(t, h, http.MethodPost, "/git.push", token, `{"approvalId":"`+pending.ApprovalID+`","payload":`+payload+`}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("replayed git.push approval must be refused with 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Concurrent requests that present one approval run the action once; the
// others are refused because the approval is already consumed.
func TestApprovalIsSingleUseUnderConcurrentReplay(t *testing.T) {
	ctx := context.Background()
	gate, st, queue, token := setupMCPGateTest(t)
	var calls atomic.Int32
	gate.RegisterAction("git.open_pr", func(_ context.Context, _ domain.Workload, _ string, _ json.RawMessage) (any, error) {
		calls.Add(1)
		return map[string]string{"status": "ok"}, nil
	})
	h := gate.ActionHandler()

	rec := postAction(t, h, http.MethodPost, "/git.open_pr", token, `{"payload":{"branch":"sablecourt/task-1"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Approve(ctx, pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	body := `{"approvalId":"` + pending.ApprovalID + `","payload":{"branch":"sablecourt/task-1"}}`
	var wg sync.WaitGroup
	var okCount, denyCount atomic.Int32
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := postAction(t, h, http.MethodPost, "/git.open_pr", token, body)
			switch r.Code {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusForbidden:
				denyCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if okCount.Load() != 1 || denyCount.Load() != 5 || calls.Load() != 1 {
		t.Fatalf("expected exactly 1 execution (ok=%d deny=%d calls=%d)", okCount.Load(), denyCount.Load(), calls.Load())
	}
	got, err := st.GetApproval(ctx, pending.ApprovalID)
	if err != nil || got.Status != domain.ApprovalConsumed {
		t.Fatalf("expected consumed approval, got %+v err=%v", got, err)
	}
}

func TestGitPushRefusesCommitChangeAfterApproval(t *testing.T) {
	ctx := context.Background()
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)

	wl := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class:   domain.ClassCodingAgent,
			Runtime: domain.RuntimeConfig{Type: "worktree-cli"},
			Coding:  &domain.CodingConfig{Harness: "claude", RepoPath: repoDir},
			Grants:  domain.GrantSet{Actions: []string{"git.push"}},
		},
	}
	gate, _, queue, token := setupGitPushGate(t, repoDir, wl, domain.EffectApprove, nil)
	h := gate.ActionHandler()

	payload := `{"branch":"` + branch + `","commit_sha":"` + commitSHA + `"}`
	rec := postAction(t, h, http.MethodPost, "/git.push", token, `{"payload":`+payload+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)
	if _, err := queue.Approve(ctx, pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatal(err)
	}

	// Mutate branch HEAD after approval: push must fail closed.
	if err := os.WriteFile(filepath.Join(repoDir, "extra.txt"), []byte("unreviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "extra.txt")
	runGit(t, repoDir, "commit", "-m", "unreviewed commit")

	rec = postAction(t, h, http.MethodPost, "/git.push", token, `{"approvalId":"`+pending.ApprovalID+`","payload":`+payload+`}`)
	if rec.Code != http.StatusConflict || !strings.Contains(errorText(t, rec), "branch HEAD does not match") {
		t.Fatalf("expected 409 commit mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
	cmd := exec.Command("git", "-C", bareDir, "rev-parse", "--verify", "refs/heads/"+branch)
	if err := cmd.Run(); err == nil {
		t.Fatal("remote branch must not exist when branch HEAD changed after approval")
	}
}

func TestGitPushRefusalPaths(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, _, commitSHA := initGitRepoWithBareRemote(t, branch)
	runGit(t, repoDir, "branch", "sablecourt/other-task")

	coderWL := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class:   domain.ClassCodingAgent,
			Runtime: domain.RuntimeConfig{Type: "worktree-cli"},
			Coding:  &domain.CodingConfig{Harness: "claude", RepoPath: repoDir},
			Grants:  domain.GrantSet{Actions: []string{"git.push"}},
		},
	}
	ungrantedRepoWL := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "ops", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class:  domain.ClassLongLivedAgent,
			Grants: domain.GrantSet{Actions: []string{"git.push"}},
		},
	}
	restrictedRepoWL := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "ops-repo", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class:  domain.ClassLongLivedAgent,
			Grants: domain.GrantSet{Actions: []string{"git.push"}, Repos: []string{"shadewright-labs/other-repo"}},
		},
	}

	cases := []struct {
		name     string
		wl       domain.Workload
		effect   domain.PolicyEffect
		body     string
		wantCode int
		wantErr  string
	}{
		{
			name:     "protected branch main",
			wl:       coderWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"main","commit_sha":"` + commitSHA + `"}}`,
			wantCode: http.StatusForbidden,
			wantErr:  "must start with sablecourt/",
		},
		{
			name:     "other task branch refused",
			wl:       coderWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"sablecourt/other-task","commit_sha":"` + commitSHA + `"}}`,
			wantCode: http.StatusForbidden,
			wantErr:  "not this task's branch",
		},
		{
			name:     "unknown remote target",
			wl:       coderWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"` + commitSHA + `","remote":"evil"}}`,
			wantCode: http.StatusForbidden,
			wantErr:  "unknown remote",
		},
		{
			name:     "remote URL injection refused",
			wl:       coderWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"` + commitSHA + `","remote":"https://evil.example/repo.git"}}`,
			wantCode: http.StatusForbidden,
			wantErr:  "illegal characters",
		},
		{
			name:     "unknown requested repo target",
			wl:       coderWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"` + commitSHA + `","repo":"other/repo"}}`,
			wantCode: http.StatusForbidden,
			wantErr:  "does not match granted repository",
		},
		{
			name:     "workload without granted repo",
			wl:       ungrantedRepoWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"` + commitSHA + `"}}`,
			wantCode: http.StatusForbidden,
			wantErr:  "no granted repository",
		},
		{
			name:     "workload granted different repo",
			wl:       restrictedRepoWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"` + commitSHA + `"}}`,
			wantCode: http.StatusForbidden,
			wantErr:  "not in workload grants.repos",
		},
		{
			name:     "non-hex short commit refused",
			wl:       coderWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"HEAD"}}`,
			wantCode: http.StatusBadRequest,
			wantErr:  "commit_sha must be a full",
		},
		{
			name:     "unknown field force refused",
			wl:       coderWL,
			effect:   domain.EffectApprove,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"` + commitSHA + `","force":true}}`,
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid action payload",
		},
		{
			name:     "policy deny blocks push",
			wl:       coderWL,
			effect:   domain.EffectDeny,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"` + commitSHA + `"}}`,
			wantCode: http.StatusForbidden,
			wantErr:  "policy denied",
		},
		{
			name:     "missing approval pauses with 202 even under policy allow",
			wl:       coderWL,
			effect:   domain.EffectAllow,
			body:     `{"payload":{"branch":"` + branch + `","commit_sha":"` + commitSHA + `"}}`,
			wantCode: http.StatusAccepted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate, _, _, token := setupGitPushGate(t, repoDir, tc.wl, tc.effect, nil)
			rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token, tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantErr != "" && !strings.Contains(errorText(t, rec), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", errorText(t, rec), tc.wantErr)
			}
		})
	}
}
