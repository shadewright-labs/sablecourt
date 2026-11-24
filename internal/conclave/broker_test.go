package conclave

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type brokerReadFailure struct {
	store.Store
}

func (s brokerReadFailure) ListConclaveMessages(context.Context, string, int64) ([]domain.ConclaveMessage, error) {
	return nil, errors.New("messages unavailable")
}

func brokerTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPostMessageDerivesKindFromSender(t *testing.T) {
	for _, tc := range []struct{ role, kind, want string }{
		{"coder", "decree", "proposal"},
		{"coder", "decision", "proposal"},
		{"coder", "system", "proposal"},
		{"coder", "review", "review"},
		{"operator", "proposal", "decree"},
		{"@operator", "", "decree"},
	} {
		t.Run(tc.role+"/"+tc.kind, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}); err != nil {
				t.Fatal(err)
			}
			b := NewBroker(Options{Store: st})
			msg, err := b.PostMessage(ctx, "room", tc.role, "all", tc.kind, "message", "")
			if err != nil {
				t.Fatal(err)
			}
			stored, err := st.ListConclaveMessages(ctx, "room", 0)
			if err != nil || len(stored) != 1 || stored[0].Kind != tc.want || msg.Kind != tc.want {
				t.Fatalf("want kind %q, returned %+v, stored %+v, error %v", tc.want, msg, stored, err)
			}
		})
	}
}

func TestWardMessageReadFailsClosed(t *testing.T) {
	for _, wards := range []domain.ConclaveWards{{}, {MaxMessages: 1}, {MaxPingPong: 1}} {
		t.Run(fmt.Sprintf("%+v", wards), func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", TemplateName: "template", State: domain.ConclaveStateActive}); err != nil {
				t.Fatal(err)
			}
			b := NewBroker(Options{Store: brokerReadFailure{st}, GetBundle: func() *config.Bundle {
				return &config.Bundle{Conclaves: map[string]domain.Conclave{"template": {Spec: domain.ConclaveSpec{
					Wards: wards, Vassals: []domain.ConclaveVassal{{Role: "coder", CanMessage: []string{"all"}}},
				}}}}
			}})
			_, err := b.PostMessage(ctx, "room", "coder", "all", "proposal", "must not persist", "")
			if !errors.Is(err, ErrWardTripped) {
				t.Errorf("want Ward refusal, got %v", err)
			}
			msgs, err := st.ListConclaveMessages(ctx, "room", 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, msg := range msgs {
				if msg.FromRole == "coder" {
					t.Error("participant message persisted despite unreadable Ward evidence")
				}
			}
			session, err := st.GetConclave(ctx, "room")
			if err != nil || session.State != domain.ConclaveStateInputRequired {
				t.Fatalf("want paused session, got %+v, %v", session, err)
			}
		})
	}
}

func TestPutRelicRequiresActiveConclave(t *testing.T) {
	for _, tc := range []struct {
		state domain.ConclaveState
		want  error
	}{
		{domain.ConclaveStateActive, nil},
		{domain.ConclaveStateCompleted, ErrConclaveClosed},
		{domain.ConclaveStateCancelled, ErrConclaveClosed},
		{domain.ConclaveStateInputRequired, ErrConclavePaused},
		{"", ErrConclaveClosed},
		{"unknown", ErrConclaveClosed},
	} {
		for _, role := range []string{"coder", "operator"} {
			t.Run(string(tc.state)+"/"+role, func(t *testing.T) {
				ctx := context.Background()
				st := brokerTestStore(t)
				if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: tc.state}); err != nil {
					t.Fatal(err)
				}
				b := NewBroker(Options{Store: st})
				_, err := b.PutRelic(ctx, "room", role, "report", "", "body")
				if !errors.Is(err, tc.want) {
					t.Errorf("want %v, got %v", tc.want, err)
				}
				relics, err := st.ListConclaveRelics(ctx, "room")
				if err != nil || (tc.want != nil && len(relics) != 0) {
					t.Fatalf("unexpected relics %+v, %v", relics, err)
				}
			})
		}
	}
}

func TestConclaveLifecycleWardsAndActivation(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, name := range []string{"coder-wl", "reviewer-wl"} {
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name:  name,
			Zone:  domain.ZoneRuntime,
			Class: domain.ClassLongLivedAgent,
		}); err != nil {
			t.Fatalf("UpsertWorkload %s: %v", name, err)
		}
	}

	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{
			"coder-wl": {
				Metadata: domain.ObjectMeta{Name: "coder-wl", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Runtime: domain.RuntimeConfig{Type: "docker", Image: "alpine:3.21"}},
			},
			"reviewer-wl": {
				Metadata: domain.ObjectMeta{Name: "reviewer-wl", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Runtime: domain.RuntimeConfig{Type: "docker", Image: "alpine:3.21"}},
			},
		},
		Conclaves: map[string]domain.Conclave{
			"feature-delivery": {
				Metadata: domain.ObjectMeta{Name: "feature-delivery"},
				Spec: domain.ConclaveSpec{
					Wards: domain.ConclaveWards{MaxMessages: 10, MaxPingPong: 2},
					Vassals: []domain.ConclaveVassal{
						{Role: "coder", Workload: "coder-wl", Activation: domain.ActivationAlwaysOn, CanMessage: []string{"reviewer", "operator"}},
						{Role: "reviewer", Workload: "reviewer-wl", Activation: domain.ActivationOnMention, CanMessage: []string{"coder", "operator"}},
					},
				},
			},
		},
	}

	rec := controlplane.NewReconciler(controlplane.Options{
		Store:  st,
		Bundle: bundle,
		Policy: policy.NewEngine(nil, policy.NewCircuitBreaker(3, time.Minute)),
	})
	broker := NewBroker(Options{
		Store:      st,
		Reconciler: rec,
		GetBundle:  func() *config.Bundle { return bundle },
	})

	// 1. Open dispatches always-on Vassal (coder) immediately, not on-mention (reviewer).
	sess, err := broker.Open(ctx, "feature-delivery", "Add retry logic", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].WorkloadName != "coder-wl" {
		t.Fatalf("expected 1 always-on task for coder-wl, got %+v", tasks)
	}

	// 2. Coder mentions reviewer -> wakes on-mention Vassal + redacts secret.
	msg1, err := broker.PostMessage(ctx, sess.ID, "coder", "reviewer", "proposal", "Ready for review; token sk-1234567890abcdefghij123456", tasks[0].ID)
	if err != nil {
		t.Fatalf("PostMessage coder->reviewer: %v", err)
	}
	if strings.Contains(msg1.Body, "sk-1234567890") {
		t.Fatalf("expected secret redaction in message body, got %q", msg1.Body)
	}
	tasks, _ = st.ListTasks(ctx)
	if len(tasks) != 2 {
		t.Fatalf("expected on-mention reviewer task to be dispatched, got %d tasks", len(tasks))
	}

	for _, wake := range tasks {
		if wake.WorkloadName == "reviewer-wl" && strings.Contains(wake.InputJSON, "Ready for review") {
			t.Error("wake input copied peer message into task intent")
		}
	}

	// 3. Reviewer replies to coder (2nd message between same 2 roles).
	if _, err := broker.PostMessage(ctx, sess.ID, "reviewer", "coder", "review", "Please add timeout test", ""); err != nil {
		t.Fatalf("PostMessage reviewer->coder: %v", err)
	}

	// 4. 3rd ping-pong message trips MaxPingPong=2 Ward and pauses Conclave.
	_, err = broker.PostMessage(ctx, sess.ID, "coder", "reviewer", "question", "Which timeout?", "")
	if !errors.Is(err, ErrWardTripped) {
		t.Fatalf("expected ErrWardTripped, got %v", err)
	}
	updated, _ := st.GetConclave(ctx, sess.ID)
	if updated.State != domain.ConclaveStateInputRequired {
		t.Fatalf("expected input-required after ward trip, got %s", updated.State)
	}

	// 5. Operator Decree resumes Conclave and resets ping-pong counter.
	if _, err := broker.PostMessage(ctx, sess.ID, "operator", "all", "decree", "Use 5s timeout and ship it", ""); err != nil {
		t.Fatalf("operator Decree failed: %v", err)
	}
	resumed, _ := st.GetConclave(ctx, sess.ID)
	if resumed.State != domain.ConclaveStateActive {
		t.Fatalf("expected active after operator Decree, got %s", resumed.State)
	}

	// 6. Store and retrieve a redacted Relic.
	relic, err := broker.PutRelic(ctx, sess.ID, "coder", "patch.diff", "text/x-diff", "diff --git\napi_key=supersecretvalue12345")
	if err != nil {
		t.Fatalf("PutRelic: %v", err)
	}
	if strings.Contains(relic.Body, "supersecretvalue12345") {
		t.Fatalf("expected secret redaction in Relic, got %q", relic.Body)
	}
	gotRelic, err := st.GetConclaveRelic(ctx, sess.ID, "patch.diff")
	if err != nil || gotRelic.SHA256 != relic.SHA256 {
		t.Fatalf("GetConclaveRelic mismatch: %v %+v", err, gotRelic)
	}

	// 7. Session budget Ward: attach a $0.50 budget, record $0.60 spend on coder's task, verify Ward trips.
	bundle.Budgets = map[string]domain.Budget{
		"conclave-cap": {
			Metadata: domain.ObjectMeta{Name: "conclave-cap"},
			Spec:     domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxCostUSD: 0.50}},
		},
	}
	cc := bundle.Conclaves["feature-delivery"]
	cc.Spec.BudgetRef = "conclave-cap"
	bundle.Conclaves["feature-delivery"] = cc
	if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{
		TaskID:       tasks[0].ID,
		WorkloadName: "coder-wl",
		Provider:     "anthropic",
		Model:        "claude-sonnet-4-5",
		InputTokens:  1000,
		OutputTokens: 500,
		CostMicroUSD: 600_000, // $0.60 > $0.50 cap
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordBudgetUsage: %v", err)
	}
	_, err = broker.PostMessage(ctx, sess.ID, "coder", "reviewer", "proposal", "Another turn after budget cap", tasks[0].ID)
	if !errors.Is(err, ErrWardTripped) || !strings.Contains(err.Error(), "budget_exceeded") {
		t.Fatalf("expected budget_exceeded ErrWardTripped, got %v", err)
	}
}

func TestMCPPostKind(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	wl := domain.Workload{Metadata: domain.ObjectMeta{Name: "worker"}, Spec: domain.WorkloadSpec{Grants: domain.GrantSet{MCPTools: []string{"conclave.post"}}}}
	bundle := &config.Bundle{Conclaves: map[string]domain.Conclave{"council": {Spec: domain.ConclaveSpec{Vassals: []domain.ConclaveVassal{{Role: "reader", Workload: "worker", CanMessage: []string{"all"}}}}}}}
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
	session, err := b.Open(ctx, "council", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	eng := policy.NewEngine([]domain.Policy{{Spec: domain.PolicySpec{Rules: []domain.PolicyRule{{Workload: "worker", Capability: "mcp:conclave.post", Effect: domain.EffectAllow}}}}}, nil)
	g := mcpgate.New(mcpgate.Options{Store: st, Policy: eng})
	b.RegisterMCPTools(g)
	srv := g.NewServerForWorkload(domain.Lease{TaskID: "worker-task"}, wl)
	ct, transport := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for _, kind := range []string{"decision", "proposal"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "conclave.post", Arguments: map[string]string{"conclave_id": session.ID, "to": "all", "kind": kind, "body": "notes"}})
		if err != nil || res.IsError {
			t.Fatalf("post %s: %+v, %v", kind, res, err)
		}
		msgs, err := st.ListConclaveMessages(ctx, session.ID, 0)
		if err != nil || len(msgs) == 0 || msgs[len(msgs)-1].Kind != "proposal" {
			t.Errorf("%s produced %+v, %v", kind, msgs, err)
		}
	}
}

func TestResolveReservedVassal(t *testing.T) {
	for _, role := range []string{"@operator", "operator", "@system"} {
		t.Run(role, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			vassal := domain.ConclaveVassal{Role: role, Workload: "worker"}
			council := domain.Conclave{Spec: domain.ConclaveSpec{Vassals: []domain.ConclaveVassal{vassal}}}
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle {
				return &config.Bundle{Conclaves: map[string]domain.Conclave{"council": council}}
			}})
			sess, err := b.Open(ctx, "council", "test", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.ResolveRoleForWorkload(ctx, sess.ID, "worker"); !errors.Is(err, ErrMessageDenied) {
				t.Errorf("invalid participant role returned: %v", err)
			}
		})
	}
}
