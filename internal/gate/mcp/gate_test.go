package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
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

func TestMCPHTTPSessionUsesReloadedPolicyAndGrants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate, st := newMemoryGateTest(t)
	initial := gate.snapshot()
	wl := initial.bundle.Workloads["coder"]
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: wl.Metadata.Name, Zone: wl.Metadata.Zone, Class: wl.Spec.Class,
		ManifestDigest: "m1", AgentCardJSON: "{}",
	}); err != nil {
		t.Fatalf("UpsertWorkload: %v", err)
	}
	if err := st.CreateTask(ctx, domain.Task{
		ID: "task-mem-1", IdempotencyKey: "idem-mem-1", WorkloadName: wl.Metadata.Name, State: domain.TaskStateWorking,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	token, err := model.MintTaskToken(ctx, st, "task-mem-1", wl.Metadata.Name, time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	handler := gate.MCPHandler()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "/mcp",
		DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			return rec.Result(), nil
		})},
	}, nil)
	if err != nil {
		t.Fatalf("connect HTTP MCP client: %v", err)
	}
	defer session.Close()

	put := func(key string) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "memory.put",
			Arguments: map[string]string{"namespace": "notes", "key": key, "value": "note"},
		})
		if err != nil {
			t.Fatalf("call memory.put: %v", err)
		}
		return res
	}
	if res := put("before-reload"); res.IsError {
		t.Fatalf("write before reload failed: %+v", res.Content)
	}
	deny := domain.Policy{Spec: domain.PolicySpec{DefaultEffect: domain.EffectDeny}}
	updated := &config.Bundle{
		Workloads: map[string]domain.Workload{wl.Metadata.Name: wl},
		Policies:  map[string]domain.Policy{"default": deny},
	}
	gate.Reload(updated, policy.NewEngine([]domain.Policy{deny}, nil))
	if res := put("after-reload"); !res.IsError {
		t.Errorf("write succeeded after policy reload")
	}
	if _, err := st.GetMemory(ctx, "notes", "after-reload"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected denied write to leave no row, got %v", err)
	}

	wl.Spec.Grants.MCPTools = nil
	gate.Reload(&config.Bundle{
		Workloads: map[string]domain.Workload{wl.Metadata.Name: wl},
		Policies:  initial.bundle.Policies,
	}, initial.policy)
	res, err := session.ListTools(ctx, nil)
	if err != nil || len(res.Tools) != 0 {
		t.Fatalf("expected empty tool list after grant removal, got %+v, err %v", res, err)
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

func TestGateReloadConcurrentRequestsNoRace(t *testing.T) {
	makeConfig := func() (*config.Bundle, *policy.Engine) {
		b := &config.Bundle{
			Policies: map[string]domain.Policy{
				"default": {
					Metadata: domain.ObjectMeta{Name: "default"},
					Spec: domain.PolicySpec{
						DefaultEffect: domain.EffectDeny,
						Rules: []domain.PolicyRule{
							{Workload: "ops-agent", Capability: "mcp:ping", Effect: domain.EffectAllow},
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
							MCPTools: []string{"ping"},
							Actions:  []string{"sandbox.propose_promotion"},
						},
					},
				},
				"exp-sandbox": {
					Metadata: domain.ObjectMeta{Name: "exp-sandbox", Zone: domain.ZoneSandbox},
					Spec:     domain.WorkloadSpec{Class: domain.ClassDeterministicController},
				},
			},
		}
		return b, policy.NewEngine([]domain.Policy{b.Policies["default"]}, nil)
	}

	cases := []struct {
		name string
		run  func(t *testing.T, gate *Gate, token string, lease domain.Lease, wl domain.Workload)
	}{
		{
			name: "invokeTool",
			run: func(t *testing.T, gate *Gate, _ string, lease domain.Lease, wl domain.Workload) {
				def := ToolDef{
					Name: "ping",
					Handler: func(_ context.Context, _ domain.Workload, _ string, _ json.RawMessage) (string, error) {
						return "pong", nil
					},
				}
				res, err := gate.invokeTool(context.Background(), lease, wl, def, json.RawMessage(`{}`))
				if err != nil || res == nil || res.IsError {
					t.Errorf("invokeTool failed: res=%+v err=%v", res, err)
				}
			},
		},
		{
			name: "authenticate",
			run: func(t *testing.T, gate *Gate, token string, _ domain.Lease, _ domain.Workload) {
				req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/mcp", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				if _, ok := gate.authenticate(rec, req); !ok {
					t.Errorf("authenticate failed: code=%d body=%s", rec.Code, rec.Body.String())
				}
			},
		},
		{
			name: "ActionHandler and sandbox.propose_promotion",
			run: func(t *testing.T, gate *Gate, token string, _ domain.Lease, _ domain.Workload) {
				h := gate.ActionHandler()
				body := `{"payload":{"workload":"exp-sandbox","branch":"sablecourt/promote-exp","reason":"ready"}}`
				req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/sandbox.propose_promotion", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("ActionHandler failed: code=%d body=%s", rec.Code, rec.Body.String())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}

			initBundle, initPol := makeConfig()
			_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
				Name: "ops-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent,
				ManifestDigest: "m1", AgentCardJSON: "{}",
			})
			_ = st.CreateTask(ctx, domain.Task{
				ID: "task-race-1", IdempotencyKey: "idem-race-1", WorkloadName: "ops-agent", State: domain.TaskStateWorking,
			})
			token, err := model.MintTaskToken(ctx, st, "task-race-1", "ops-agent", time.Hour)
			if err != nil {
				t.Fatalf("MintTaskToken: %v", err)
			}

			gate := New(Options{
				Store:  st,
				Bundle: initBundle,
				Policy: initPol,
			})
			RegisterBuiltinGitOps(gate, "", nil)

			lease := domain.Lease{TaskID: "task-race-1", WorkloadName: "ops-agent"}
			wl := initBundle.Workloads["ops-agent"]

			const iterations = 60
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						b, pol := makeConfig()
						gate.Reload(b, pol)
					}
				}
			}()

			for range iterations {
				tc.run(t, gate, token, lease, wl)
			}
			close(stop)
			wg.Wait()
		})
	}
}

type readAfterReload struct {
	io.Reader
	reload func()
}

func (r *readAfterReload) Read(p []byte) (int, error) {
	if r.reload != nil {
		r.reload()
		r.reload = nil
	}
	return r.Reader.Read(p)
}

func TestActionKeepsSnapshotDuringReload(t *testing.T) {
	gate, _, _, token := setupMCPGateTest(t)
	wl := gate.snapshot().bundle.Workloads["triage-agent"]
	capability := "action:sandbox.propose_promotion"
	wl.Spec.Grants.Actions = []string{"sandbox.propose_promotion"}
	allow := domain.Policy{Spec: domain.PolicySpec{
		DefaultEffect: domain.EffectDeny,
		Rules:         []domain.PolicyRule{{Workload: wl.Metadata.Name, Capability: capability, Effect: domain.EffectAllow}},
	}}
	deny := domain.Policy{Spec: domain.PolicySpec{DefaultEffect: domain.EffectDeny}}
	gate.Reload(&config.Bundle{
		Workloads: map[string]domain.Workload{
			wl.Metadata.Name: wl,
			"candidate":      {Metadata: domain.ObjectMeta{Name: "candidate", Zone: domain.ZoneSandbox}},
		},
		Policies: map[string]domain.Policy{"default": allow},
	}, policy.NewEngine([]domain.Policy{allow}, nil))
	RegisterBuiltinGitOps(gate, "", nil)
	handler := gate.ActionHandler()
	body := `{"payload":{"workload":"candidate","branch":"sablecourt/candidate","reason":"ready"}}`
	run := func(reader io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/sandbox.propose_promotion", reader)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := run(&readAfterReload{Reader: strings.NewReader(body), reload: func() {
		gate.Reload(&config.Bundle{
			Workloads: map[string]domain.Workload{wl.Metadata.Name: wl},
			Policies:  map[string]domain.Policy{"default": deny},
		}, policy.NewEngine([]domain.Policy{deny}, nil))
	}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "proposal_ready") {
		t.Fatalf("in-flight action lost its snapshot: %d %s", rec.Code, rec.Body)
	}
	if rec := run(strings.NewReader(body)); rec.Code != http.StatusForbidden {
		t.Fatalf("next action did not use reloaded policy: %d %s", rec.Code, rec.Body)
	}
}

func approveOpenPR(t *testing.T, gate *Gate, queue *approval.Queue, token, payload string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(`{"payload":`+payload+`}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	gate.ActionHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Approve(context.Background(), pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	return pending.ApprovalID
}

func callWithApproval(gate *Gate, token, approvalID, payload string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(`{"approvalId":"`+approvalID+`","payload":`+payload+`}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	gate.ActionHandler().ServeHTTP(rec, req)
	return rec
}

// An approval authorizes one execution: the second call with the same
// approvalId is refused and the action does not run again.
func TestApprovalIsConsumedOnFirstUse(t *testing.T) {
	ctx := context.Background()
	gate, st, queue, token := setupMCPGateTest(t)
	runs := 0
	gate.RegisterAction("git.open_pr", func(context.Context, domain.Workload, string, json.RawMessage) (any, error) {
		runs++
		return map[string]string{"pr_url": "https://github.com/acme/widgets/pull/1"}, nil
	})
	payload := `{"head":"sablecourt/task-1","title":"Fix bug"}`
	id := approveOpenPR(t, gate, queue, token, payload)

	if rec := callWithApproval(gate, token, id, payload); rec.Code != http.StatusOK {
		t.Fatalf("first use: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := callWithApproval(gate, token, id, payload); rec.Code != http.StatusForbidden {
		t.Fatalf("second use: want 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if runs != 1 {
		t.Fatalf("action ran %d times, want 1", runs)
	}
	got, err := st.GetApproval(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ApprovalConsumed {
		t.Fatalf("status %q, want consumed", got.Status)
	}
}

// A consumed approval stays consumed when the side effect fails: the agent
// needs a fresh approval, and the failure is visible in the audit log.
func TestApprovalConsumedBeforeFailedSideEffectIsNotReusable(t *testing.T) {
	ctx := context.Background()
	gate, st, queue, token := setupMCPGateTest(t)
	runs := 0
	gate.RegisterAction("git.open_pr", func(context.Context, domain.Workload, string, json.RawMessage) (any, error) {
		runs++
		return nil, errors.New("github unavailable")
	})
	payload := `{"head":"sablecourt/task-1","title":"Fix bug"}`
	id := approveOpenPR(t, gate, queue, token, payload)

	if rec := callWithApproval(gate, token, id, payload); rec.Code != http.StatusBadGateway {
		t.Fatalf("first use: want 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := callWithApproval(gate, token, id, payload); rec.Code != http.StatusForbidden {
		t.Fatalf("retry: want 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if runs != 1 {
		t.Fatalf("action ran %d times, want 1", runs)
	}
	records, err := st.ListAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var consumed, failed bool
	for _, r := range records {
		if r.Category == "approval" && r.Outcome == "consumed" && strings.Contains(r.DetailJSON, id) {
			consumed = true
		}
		if r.Category == "data_action_gate" && r.Outcome == "error" && strings.Contains(r.DetailJSON, id) {
			failed = true
		}
	}
	if !consumed || !failed {
		t.Fatalf("audit must show the consumption and the failed use of %s (consumed=%v failed=%v)", id, consumed, failed)
	}
}

// An approved row past its expiry no longer authorizes anything.
func TestExpiredApprovedRowIsRefused(t *testing.T) {
	ctx := context.Background()
	gate, st, _, token := setupMCPGateTest(t)
	runs := 0
	gate.RegisterAction("git.open_pr", func(context.Context, domain.Workload, string, json.RawMessage) (any, error) {
		runs++
		return map[string]string{}, nil
	})
	payload := `{"head":"sablecourt/task-1","title":"Fix bug"}`
	var canonical any
	_ = json.Unmarshal([]byte(payload), &canonical)
	canon, digest, err := approval.CanonicalizeAndHash(canonical)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if err := st.CreateApproval(ctx, domain.Approval{
		ID: "apr_expired", TaskID: "task-1", WorkloadName: "triage-agent", Capability: "action:git.open_pr",
		PayloadCanonicalJSON: canon, PayloadSHA256: digest, ExpiresAt: past.Add(30 * time.Minute), CreatedAt: past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideApproval(ctx, "apr_expired", digest, domain.ApprovalApproved, "operator", `{}`); err != nil {
		t.Fatal(err)
	}
	if rec := callWithApproval(gate, token, "apr_expired", payload); rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for expired approval, got %d: %s", rec.Code, rec.Body.String())
	}
	if runs != 0 {
		t.Fatalf("action ran %d times, want 0", runs)
	}
}
