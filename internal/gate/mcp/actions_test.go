package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type stubPROpener struct {
	last OpenPRInput
}

func (s *stubPROpener) OpenPR(_ context.Context, in OpenPRInput) (string, error) {
	s.last = in
	return "https://github.com/" + in.Owner + "/" + in.Repo + "/pull/99", nil
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	run("init")
	run("config", "user.email", "test@sablecourt.local")
	run("config", "user.name", "Sablecourt Test")
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o600)
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}

func TestBuiltinGitOpsActions(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepo(t)
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.Migrate(ctx)

	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{
			"default": {
				Metadata: domain.ObjectMeta{Name: "default"},
				Spec: domain.PolicySpec{
					DefaultEffect: domain.EffectDeny,
					Rules: []domain.PolicyRule{
						{Workload: "ops-agent", Capability: "action:git.create_branch", Effect: domain.EffectAllow},
						{Workload: "ops-agent", Capability: "action:git.open_pr", Effect: domain.EffectAllow},
						{Workload: "ops-agent", Capability: "action:sandbox.propose_promotion", Effect: domain.EffectAllow},
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{
			"ops-agent": {
				Metadata: domain.ObjectMeta{Name: "ops-agent", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						Actions: []string{"git.create_branch", "git.open_pr", "sandbox.propose_promotion"},
					},
				},
			},
			"exp-sandbox": {
				Metadata: domain.ObjectMeta{Name: "exp-sandbox", Zone: domain.ZoneSandbox},
				Spec:     domain.WorkloadSpec{Class: domain.ClassDeterministicController},
			},
		},
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "ops-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d1", AgentCardJSON: "{}",
	})
	_ = st.CreateTask(ctx, domain.Task{
		ID: "t1", IdempotencyKey: "k1", WorkloadName: "ops-agent", State: domain.TaskStateWorking,
	})
	token, _ := model.MintTaskToken(ctx, st, "t1", "ops-agent", time.Hour)

	eng := policy.NewEngine([]domain.Policy{bundle.Policies["default"]}, nil)
	gate := New(Options{Store: st, Bundle: bundle, Policy: eng, Approvals: approval.NewQueue(st, autoApprover{})})
	prStub := &stubPROpener{}
	RegisterBuiltinGitOps(gate, repo, prStub)

	h := gate.ActionHandler()

	// 1. Rejects unsafe branch name (main).
	req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.create_branch", strings.NewReader(`{"payload":{"branch":"main"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected failure for protected branch main, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Creates sablecourt/fix-1 branch in repo.
	req = httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.create_branch", strings.NewReader(`{"payload":{"branch":"sablecourt/fix-1"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 creating branch, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Opens PR via stub PROpener.
	req = httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/fix-1","base":"main","title":"Fix 1"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/pull/99") {
		t.Fatalf("expected PR URL, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Proposes sandbox promotion for exp-sandbox.
	req = httptest.NewRequest(http.MethodPost, "http://sablecourt.local/sandbox.propose_promotion", strings.NewReader(`{"payload":{"workload":"exp-sandbox","branch":"sablecourt/promote-exp","reason":"tests passed"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for sandbox promotion proposal, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "proposal_ready" {
		t.Fatalf("unexpected promotion response: %+v", out)
	}
}
