package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type autoApprover struct{}

func (autoApprover) Confirm(_ context.Context, req approval.ConfirmationRequest) (approval.Proof, error) {
	return approval.Proof{Method: "test_oob", ConfirmedAt: time.Now().UTC(), Digest: req.PayloadSHA256}, nil
}

func setupMCPGateTest(t *testing.T) (*Gate, *sqlite.SQLiteStore, *approval.Queue, string) {
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
						{Workload: "triage-agent", Capability: "mcp:github.get_issue", Effect: domain.EffectAllow},
						{Workload: "triage-agent", Capability: "action:git.open_pr", Effect: domain.EffectApprove},
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{
			"triage-agent": {
				Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						MCPTools: []string{"github.get_issue"},
						Actions:  []string{"git.open_pr"},
					},
				},
			},
		},
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent,
		ManifestDigest: "m1", AgentCardJSON: "{}",
	})
	_ = st.CreateTask(ctx, domain.Task{
		ID: "task-1", IdempotencyKey: "idem-1", WorkloadName: "triage-agent", State: domain.TaskStateWorking,
	})
	token, err := model.MintTaskToken(ctx, st, "task-1", "triage-agent", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	breaker := policy.NewCircuitBreaker(2, time.Minute)
	eng := policy.NewEngine([]domain.Policy{bundle.Policies["default"]}, breaker)
	queue := approval.NewQueue(st, autoApprover{})

	gate := New(Options{
		Store:     st,
		Bundle:    bundle,
		Policy:    eng,
		Breaker:   breaker,
		Approvals: queue,
	})

	gate.RegisterTool(ToolDef{
		Name:        "github.get_issue",
		Description: "Fetch a GitHub issue",
		Handler: func(_ context.Context, _ domain.Workload, _ string, _ json.RawMessage) (string, error) {
			return "Issue #7 body leaked token ghp_1234567890abcdefghijklmnopqrstuvwx in logs", nil
		},
	})
	gate.RegisterTool(ToolDef{
		Name:        "github.delete_repo",
		Description: "Dangerous ungranted tool",
		Handler: func(_ context.Context, _ domain.Workload, _ string, _ json.RawMessage) (string, error) {
			return "deleted", nil
		},
	})

	gate.RegisterAction("git.open_pr", func(_ context.Context, _ domain.Workload, _ string, payload json.RawMessage) (any, error) {
		return map[string]any{"pr_url": "https://github.com/shadewright-labs/sablecourt/pull/1", "echo": string(payload)}, nil
	})

	return gate, st, queue, token
}

func TestMCPServerFiltersToolsAndRedactsOutput(t *testing.T) {
	ctx := context.Background()
	gate, _, _, _ := setupMCPGateTest(t)

	lease := domain.Lease{TaskID: "task-1", WorkloadName: "triage-agent"}
	wl := gate.bundle.Workloads["triage-agent"]
	srv := gate.NewServerForWorkload(lease, wl)

	ct, st := mcp.NewInMemoryTransports()
	srvSession, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer srvSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	cliSession, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cliSession.Close()

	listRes, err := cliSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listRes.Tools) != 1 || listRes.Tools[0].Name != "github.get_issue" {
		t.Fatalf("expected only allowlisted tool github.get_issue, got %+v", listRes.Tools)
	}

	callRes, err := cliSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "github.get_issue",
		Arguments: map[string]any{"number": 7},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if callRes.IsError || len(callRes.Content) != 1 {
		t.Fatalf("unexpected call result: %+v", callRes)
	}
	txt, ok := callRes.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", callRes.Content[0])
	}
	if strings.Contains(txt.Text, "ghp_1234") || !strings.Contains(txt.Text, "[REDACTED:github_token]") {
		t.Fatalf("expected redacted GitHub token in MCP output, got %q", txt.Text)
	}
}

func TestTypedActionRequiresApprovalAndVerifiesPinnedDigest(t *testing.T) {
	ctx := context.Background()
	gate, _, queue, token := setupMCPGateTest(t)
	h := gate.ActionHandler()

	// Step 1: initial call pauses with 202 approval_required and pinned SHA-256.
	req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(`{"payload":{"branch":"sablecourt/task-1","title":"Fix bug"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("unmarshal 202 body: %v", err)
	}
	if pending.ApprovalID == "" || pending.PayloadSHA256 == "" {
		t.Fatalf("missing approval id or digest: %+v", pending)
	}

	// Step 2: approve out-of-band.
	if _, err := queue.Approve(ctx, pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatalf("queue.Approve: %v", err)
	}

	// Step 3: replaying with a modified payload must fail with 403.
	tampered := `{"approvalId":"` + pending.ApprovalID + `","payload":{"branch":"main","title":"Fix bug"}}`
	req = httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(tampered))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on tampered payload, got %d: %s", rec.Code, rec.Body.String())
	}

	// Step 4: replaying with exact approved payload succeeds with 200.
	exact := `{"approvalId":"` + pending.ApprovalID + `","payload":{"branch":"sablecourt/task-1","title":"Fix bug"}}`
	req = httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(exact))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with exact approved payload, got %d: %s", rec.Code, rec.Body.String())
	}
}
