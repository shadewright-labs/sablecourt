package conclave

import (
	"context"
	"database/sql"
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
	"github.com/shadewright-labs/sablecourt/internal/notify"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

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
			path := filepath.Join(t.TempDir(), "test.db")
			st, err := sqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			bundle := limitsBundle(wards, domain.ConclaveVassal{Role: "coder", CanMessage: []string{"all"}})
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "ALTER TABLE conclave_messages RENAME TO unavailable_messages"); err != nil {
				t.Fatal(err)
			}
			if _, err := b.PostMessage(ctx, room.ID, "coder", "all", "proposal", "must not persist", ""); err == nil {
				t.Error("participant message accepted without Ward evidence")
			}
			if _, err := db.ExecContext(ctx, "ALTER TABLE unavailable_messages RENAME TO conclave_messages"); err != nil {
				t.Fatal(err)
			}
			msgs, err := st.ListConclaveMessages(ctx, room.ID, 0)
			if err != nil || len(msgs) != 0 {
				t.Fatalf("failed evidence read changed the room: %v, %v", msgs, err)
			}
			if _, err := b.PostMessage(ctx, room.ID, "coder", "all", "proposal", "retry", ""); err != nil {
				t.Fatalf("post after restoring evidence: %v", err)
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

	// 4. The second reply paused the Conclave. The next post is refused.
	_, err = broker.PostMessage(ctx, sess.ID, "coder", "reviewer", "question", "Which timeout?", "")
	if !errors.Is(err, ErrConclavePaused) {
		t.Fatalf("expected ErrConclavePaused, got %v", err)
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
	paid, err := broker.PostMessage(ctx, sess.ID, "coder", "reviewer", "proposal", "Reply at budget cap", tasks[0].ID)
	if err != nil || paid.ID == 0 {
		t.Fatalf("paid reply: %+v, %v", paid, err)
	}
	paused, err := st.GetConclave(ctx, sess.ID)
	if err != nil || paused.State != domain.ConclaveStateInputRequired || !strings.Contains(paused.WardReason, "budget_exceeded") {
		t.Fatalf("expected budget pause, got %+v, %v", paused, err)
	}
}

func TestMCPPostKind(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	wl := domain.Workload{Metadata: domain.ObjectMeta{Name: "worker"}, Spec: domain.WorkloadSpec{Grants: domain.GrantSet{MCPTools: []string{"conclave.post"}}}}
	bundle := limitsBundle(domain.ConclaveWards{MaxMessages: 3, MaxPingPong: 3}, domain.ConclaveVassal{Role: "reader", Workload: "worker", CanMessage: []string{"all"}})
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
	session, err := b.Open(ctx, "council", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	task := domain.Task{
		ID: "worker-task", WorkloadName: "worker", State: domain.TaskStateWorking,
		IdempotencyKey: "conclave-" + session.ID + "-init-reader",
		InputJSON:      fmt.Sprintf(`{"conclaveId":%q,"role":"reader","activation":"always-on"}`, session.ID),
	}
	if err := st.CreateTask(ctx, task); err != nil {
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

func TestMCPPostUsesTaskAssignment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		taskInput func(string) string
		key       func(string) string
		want      string
	}{
		{"second role", func(id string) string {
			return fmt.Sprintf(`{"conclaveId":%q,"role":"second","activation":"always-on"}`, id)
		},
			func(id string) string { return "conclave-" + id + "-init-second" }, "second"},
		{"other council", func(string) string { return `{"conclaveId":"other","role":"first","activation":"always-on"}` },
			func(id string) string { return "conclave-" + id + "-init-first" }, ""},
		{"unassigned task", func(id string) string {
			return fmt.Sprintf(`{"conclaveId":%q,"role":"first","activation":"always-on"}`, id)
		},
			func(string) string { return "unrelated" }, ""},
		{"wrong type", func(string) string { return `{"conclaveId":123,"role":true}` }, func(string) string { return "wrong-type" }, ""},
		{"empty", func(string) string { return "" }, func(string) string { return "empty" }, ""},
		{"null", func(string) string { return "null" }, func(string) string { return "null" }, ""},
		{"oversized", func(id string) string {
			return fmt.Sprintf(`{"conclaveId":%q,"role":"first","activation":"always-on","prompt":%q}`, id, strings.Repeat("x", 2<<20))
		},
			func(id string) string { return "conclave-" + id + "-init-first" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			wl := domain.Workload{Metadata: domain.ObjectMeta{Name: "worker"}, Spec: domain.WorkloadSpec{Grants: domain.GrantSet{MCPTools: []string{"conclave.post"}}}}
			bundle := limitsBundle(domain.ConclaveWards{},
				domain.ConclaveVassal{Role: "first", Workload: "worker", CanMessage: []string{"all"}},
				domain.ConclaveVassal{Role: "second", Workload: "worker", CanMessage: []string{"all"}},
			)
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
				t.Fatal(err)
			}
			task := domain.Task{ID: "task", WorkloadName: "worker", IdempotencyKey: tc.key(room.ID), State: domain.TaskStateWorking, InputJSON: tc.taskInput(room.ID)}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			eng := policy.NewEngine([]domain.Policy{{Spec: domain.PolicySpec{Rules: []domain.PolicyRule{{Workload: "worker", Capability: "mcp:conclave.post", Effect: domain.EffectAllow}}}}}, nil)
			g := mcpgate.New(mcpgate.Options{Store: st, Policy: eng})
			b.RegisterMCPTools(g)
			srv := g.NewServerForWorkload(domain.Lease{TaskID: task.ID}, wl)
			clientTransport, serverTransport := mcp.NewInMemoryTransports()
			ss, err := srv.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			cs, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "conclave.post", Arguments: map[string]string{"conclave_id": room.ID, "to": "all", "body": "reply"}})
			if err != nil || res.IsError != (tc.want == "") {
				t.Errorf("post returned %+v, %v", res, err)
			}
			msgs, err := st.ListConclaveMessages(ctx, room.ID, 0)
			if err != nil || (tc.want == "" && len(msgs) != 0) || (tc.want != "" && (len(msgs) != 1 || msgs[0].FromRole != tc.want)) {
				t.Errorf("expected role %q, stored %+v, error %v", tc.want, msgs, err)
			}
		})
	}
}

type wardSecretLookup struct {
	secret.SecretStore
	observed chan context.Context
}

func (s wardSecretLookup) Get(ctx context.Context, _ string) (string, error) {
	s.observed <- ctx
	return "", secret.ErrSecretNotFound
}

func TestWardNotificationCancellation(t *testing.T) {
	t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", "")
	lookup := wardSecretLookup{observed: make(chan context.Context, 1)}
	broker := NewBroker(Options{Store: brokerTestStore(t), Notifier: notify.New(notify.Options{Secrets: lookup})})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	broker.recordWard(ctx, domain.ConclaveSession{ID: "room"}, "", "limit")
	observed := <-lookup.observed
	cancel()
	if observed.Err() != context.Canceled {
		t.Fatal("notification outlived the ward request")
	}
}

// A Relic name is its stored key. A name that holds a daemon-held value is
// refused instead of stored or rewritten, and a name that only matches a
// pattern rule is kept as written, so two Relics never collide under one
// redacted key.
func TestPutRelicKeepsNamesAsKeys(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}); err != nil {
		t.Fatal(err)
	}
	held := "relic_meta_" + strings.Repeat("k9", 12)
	redact.Register(held)
	b := NewBroker(Options{Store: st})
	if _, err := b.PutRelic(ctx, "room", "coder", "report-"+held, "text/plain", "body"); err == nil {
		t.Fatal("relic name holding a daemon-held value was accepted")
	} else if strings.Contains(err.Error(), held) {
		t.Fatalf("refusal echoes the held value: %v", err)
	}
	for _, name := range []string{"password=report1.md", "password=report2.md"} {
		relic, err := b.PutRelic(ctx, "room", "coder", name, "text/"+held, "body of "+name)
		if err != nil {
			t.Fatal(err)
		}
		if relic.Name != name {
			t.Fatalf("relic name = %q, want %q", relic.Name, name)
		}
	}
	stored, err := st.ListConclaveRelics(ctx, "room")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d relics, want 2: one overwrote another", len(stored))
	}
	for _, r := range stored {
		if strings.Contains(r.Name, held) || strings.Contains(r.ContentType, held) {
			t.Fatalf("stored relic metadata kept a held value: %q %q", r.Name, r.ContentType)
		}
	}
}

// A Relic stored before a value became daemon-held is redacted when a
// workload reads it, in its metadata as well as its body.
func TestReadRelicForTaskRedactsStoredMetadata(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	bundle := &config.Bundle{Conclaves: map[string]domain.Conclave{"room": {Spec: domain.ConclaveSpec{
		Vassals: []domain.ConclaveVassal{{Role: "reader", Workload: "worker", CanMessage: []string{"all"}}},
	}}}}
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
	room, err := b.Open(ctx, "room", "", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := `{"conclaveId":"` + room.ID + `","role":"reader","activation":"always-on"}`
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "reader-task", IdempotencyKey: "conclave-" + room.ID + "-init-reader", WorkloadName: "worker", State: domain.TaskStateWorking, InputJSON: input, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	later := "relic_later_" + strings.Repeat("m3", 12)
	if err := st.PutConclaveRelic(ctx, domain.ConclaveRelic{ConclaveID: room.ID, Name: "report-" + later, AuthorRole: "operator", ContentType: "text/" + later, Body: "see " + later, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	redact.Register(later)
	relic, err := b.ReadRelicForTask(ctx, room.ID, "worker", "reader-task", "report-"+later)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(relic.Name, later) || strings.Contains(relic.ContentType, later) || strings.Contains(relic.Body, later) {
		t.Fatalf("relic read by a workload kept a held value: %q %q %q", relic.Name, relic.ContentType, relic.Body)
	}
	stored, err := st.GetConclaveRelic(ctx, room.ID, "report-"+later)
	if err != nil {
		t.Fatalf("stored relic key changed: %v", err)
	}
	if stored.ContentType != "text/"+later {
		t.Fatalf("read rewrote the stored relic: %q", stored.ContentType)
	}
}

// A message stored before its recipient role became a daemon-held value is
// redacted when a workload reads it, without rewriting the stored row.
func TestReadMessagesForTaskRedactsStoredToRole(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	bundle := &config.Bundle{Conclaves: map[string]domain.Conclave{"room": {Spec: domain.ConclaveSpec{
		Vassals: []domain.ConclaveVassal{{Role: "reader", Workload: "worker", CanMessage: []string{"all"}}},
	}}}}
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
	room, err := b.Open(ctx, "room", "", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := `{"conclaveId":"` + room.ID + `","role":"reader","activation":"always-on"}`
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "reader-task", IdempotencyKey: "conclave-" + room.ID + "-init-reader", WorkloadName: "worker", State: domain.TaskStateWorking, InputJSON: input, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	later := "torole_later_" + strings.Repeat("m5", 12)
	if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: room.ID, FromRole: "operator", ToRole: "role-" + later, Kind: "question", Body: "hello", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	redact.Register(later)
	msgs, err := b.ReadMessagesForTask(ctx, room.ID, "worker", "reader-task", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if strings.HasPrefix(m.ToRole, "role-") {
			found = true
		}
		if strings.Contains(m.ToRole, later) {
			t.Fatalf("message read by a workload kept a held recipient role: %q", m.ToRole)
		}
	}
	if !found {
		t.Fatalf("stored message not returned: %+v", msgs)
	}
	stored, err := st.ListConclaveMessages(ctx, room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, m := range stored {
		kept = kept || m.ToRole == "role-"+later
	}
	if !kept {
		t.Fatal("read rewrote the stored message")
	}
}

func TestClosePlainRoomKeepsTerminalStateAndAuditsOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  domain.ConclaveState
		second domain.ConclaveState
	}{
		{"completed then cancelled", domain.ConclaveStateCompleted, domain.ConclaveStateCancelled},
		{"completed then completed", domain.ConclaveStateCompleted, domain.ConclaveStateCompleted},
		{"cancelled then completed", domain.ConclaveStateCancelled, domain.ConclaveStateCompleted},
		{"cancelled then cancelled", domain.ConclaveStateCancelled, domain.ConclaveStateCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			st := brokerTestStore(t)
			bundle := limitsBundle(domain.ConclaveWards{}, domain.ConclaveVassal{Role: "coder", Workload: "worker"})
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
			sess, err := b.Open(ctx, "council", "plain room", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Close(ctx, sess.ID, tc.first); err != nil {
				t.Fatalf("first close: %v", err)
			}
			afterFirst, err := st.GetConclave(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if afterFirst.State != tc.first {
				t.Fatalf("state after first close = %s, want %s", afterFirst.State, tc.first)
			}
			if err := b.Close(ctx, sess.ID, tc.second); err != nil {
				t.Fatalf("second close: %v", err)
			}
			afterSecond, err := st.GetConclave(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if afterSecond.State != tc.first || !afterSecond.UpdatedAt.Equal(afterFirst.UpdatedAt) {
				t.Fatalf("second close mutated room: got %+v, want state %s updated_at %v", afterSecond, tc.first, afterFirst.UpdatedAt)
			}
			audits, err := st.ListAudit(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			var closes []domain.AuditRecord
			for _, rec := range audits {
				if rec.Action == "conclave.close" {
					closes = append(closes, rec)
				}
			}
			if len(closes) != 1 {
				t.Fatalf("conclave.close audit rows = %d, want 1: %+v", len(closes), closes)
			}
			if closes[0].Outcome != string(tc.first) || !strings.Contains(closes[0].DetailJSON, fmt.Sprintf(`"conclave_id":%q`, sess.ID)) {
				t.Fatalf("conclave.close audit = %+v, want outcome %s", closes[0], tc.first)
			}
			if _, err := st.VerifyAuditChain(ctx); err != nil {
				t.Fatalf("VerifyAuditChain: %v", err)
			}
		})
	}
}
