package conclave

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func limitsBundle(wards domain.ConclaveWards, roles ...domain.ConclaveVassal) *config.Bundle {
	return &config.Bundle{
		Conclaves: map[string]domain.Conclave{"council": {Spec: domain.ConclaveSpec{Wards: wards, Vassals: roles}}},
		Workloads: map[string]domain.Workload{"worker": {Metadata: domain.ObjectMeta{Name: "worker"}}},
	}
}

func TestReplyAtWardLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wards domain.ConclaveWards
		limit int
	}{
		{"default", domain.ConclaveWards{}, 2},
		{"message count", domain.ConclaveWards{MaxMessages: 1}, 1},
		{"ping pong", domain.ConclaveWards{MaxPingPong: 1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			bundle := limitsBundle(tc.wards, domain.ConclaveVassal{Role: "reader", CanMessage: []string{"operator"}})
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			var reply domain.ConclaveMessage
			for i := 1; i <= tc.limit; i++ {
				reply, err = b.PostMessage(ctx, room.ID, "reader", "operator", "review", fmt.Sprint(i), "")
				if err != nil {
					t.Fatal(err)
				}
			}
			room, err = st.GetConclave(ctx, room.ID)
			if err != nil || room.State != domain.ConclaveStateInputRequired {
				t.Errorf("after final reply: state = %s, error = %v", room.State, err)
			}
			list, err := st.ListConclaveMessages(ctx, room.ID, reply.ID-1)
			if err != nil || len(list) == 0 || list[0] != reply {
				t.Errorf("final reply missing: %v, %v", list, err)
			}
			if _, err := b.PostMessage(ctx, room.ID, "reader", "operator", "review", "extra", ""); !errors.Is(err, ErrConclavePaused) {
				t.Errorf("extra reply error = %v", err)
			}
		})
	}
}

type concurrentWardStore struct {
	store.Store
	read sync.WaitGroup
}

func (s *concurrentWardStore) ListConclaveMessages(ctx context.Context, id string, since int64) ([]domain.ConclaveMessage, error) {
	msgs, err := s.Store.ListConclaveMessages(ctx, id, since)
	s.read.Done()
	s.read.Wait()
	return msgs, err
}

func TestConcurrentPostsAtWardLimit(t *testing.T) {
	for _, cap := range []int{1, 3} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			ctx := context.Background()
			base := brokerTestStore(t)
			st := &concurrentWardStore{Store: base}
			const workers = 8
			st.read.Add(workers)
			bundle := limitsBundle(domain.ConclaveWards{MaxMessages: cap, MaxPingPong: workers}, domain.ConclaveVassal{Role: "reader", CanMessage: []string{"operator"}})
			options := Options{Store: st, GetBundle: func() *config.Bundle { return bundle }}
			room, err := NewBroker(options).Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			var posts sync.WaitGroup
			for range workers {
				posts.Go(func() {
					b := NewBroker(options)
					_, err := b.PostMessage(ctx, room.ID, "reader", "operator", "review", "reply", "")
					if err != nil && !errors.Is(err, ErrConclavePaused) {
						t.Errorf("post: %v", err)
					}
				})
			}
			posts.Wait()
			msgs, err := base.ListConclaveMessages(ctx, room.ID, 0)
			var replies int
			for _, msg := range msgs {
				if msg.FromRole == "reader" {
					replies++
				}
			}
			if err != nil || replies != cap {
				t.Fatalf("stored %d replies, want %d, read error = %v", replies, cap, err)
			}
		})
	}
}

func TestDefaultRunAndWallTimeLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		steps  int
		elapse time.Duration
		want   int
	}{
		{"run limit across brokers", 8, 0, 6},
		{"deadline from open", 1, 15 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			bundle := limitsBundle(domain.ConclaveWards{}, domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationOnMention})
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
				t.Fatal(err)
			}
			clock := time.Now().UTC()
			opts := Options{Store: st, GetBundle: func() *config.Bundle { return bundle }, Now: func() time.Time { return clock },
				Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})}
			room, err := NewBroker(opts).Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			clock = clock.Add(tc.elapse)
			var posts sync.WaitGroup
			for range tc.steps {
				posts.Go(func() {
					_, err := NewBroker(opts).PostMessage(ctx, room.ID, "operator", "all", "decree", "review", "")
					if err != nil && !errors.Is(err, ErrConclavePaused) && !errors.Is(err, ErrWardTripped) {
						t.Errorf("post: %v", err)
					}
				})
			}
			posts.Wait()
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != tc.want {
				t.Fatalf("submitted %d tasks, want %d, error = %v", len(tasks), tc.want, err)
			}
		})
	}
}

func TestOpenReportsSubmitFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		first      bool
		activation domain.ConclaveActivation
	}{
		{"missing opening workload", false, domain.ActivationAlwaysOn},
		{"missing workload after first submit", true, domain.ActivationAlwaysOn},
		{"missing opening decree workload", true, domain.ActivationOnMention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			bundle := limitsBundle(domain.ConclaveWards{})
			tpl := bundle.Conclaves["council"]
			if tc.first {
				tpl.Spec.Vassals = append(tpl.Spec.Vassals, domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationAlwaysOn})
				if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
					t.Fatal(err)
				}
			}
			tpl.Spec.Vassals = append(tpl.Spec.Vassals, domain.ConclaveVassal{Role: "missing", Workload: "missing", Activation: tc.activation})
			bundle.Conclaves["council"] = tpl
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
				Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
			if _, err := b.Open(ctx, "council", "", "review"); err == nil {
				t.Error("open accepted an undispatchable vassal")
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range tasks {
				if task.State != domain.TaskStateCancelled {
					t.Errorf("partial open left a %s task", task.State)
				}
			}
			rooms, err := st.ListConclaves(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, room := range rooms {
				if room.State != domain.ConclaveStateCancelled {
					t.Errorf("partial open left a %s council", room.State)
				}
			}
		})
	}
}

type decreeFailureStore struct {
	store.Store
	err error
}

func (s decreeFailureStore) AppendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.Store.AppendConclaveMessage(ctx, msg)
}

func TestDecreeAppendFailureDoesNotResume(t *testing.T) {
	for _, reason := range []string{"max_messages_reached(1)", "ping_pong_detected(1)"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			base := brokerTestStore(t)
			room := domain.ConclaveSession{ID: "room", State: domain.ConclaveStateInputRequired, WardReason: reason, CreatedAt: time.Now()}
			if err := base.CreateConclave(ctx, room); err != nil {
				t.Fatal(err)
			}
			fault := errors.New("append unavailable")
			b := NewBroker(Options{Store: decreeFailureStore{Store: base, err: fault}})
			if _, err := b.PostMessage(ctx, room.ID, "operator", "all", "decree", "resume", ""); !errors.Is(err, fault) {
				t.Errorf("post error = %v", err)
			}
			room, err := base.GetConclave(ctx, room.ID)
			if err != nil || room.State != domain.ConclaveStateInputRequired || room.WardReason != reason {
				t.Errorf("failed decree changed session: %+v, %v", room, err)
			}
		})
	}
}

func TestOpeningPromptIsRedacted(t *testing.T) {
	for _, tc := range []string{"api_key", "access_token"} {
		t.Run(tc, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			bundle := limitsBundle(domain.ConclaveWards{}, domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationAlwaysOn})
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
				t.Fatal(err)
			}
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
				Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
			value := "fixture-sensitive-text-1234567"
			if _, err := b.Open(ctx, "council", "", tc+"="+value); err != nil {
				t.Fatal(err)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != 1 || strings.Contains(tasks[0].InputJSON, value) {
				t.Errorf("opening prompt was not redacted or was unavailable: %v", err)
			}
		})
	}
}

func TestPaidReplyPausesWithoutWaking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget domain.PerTaskBudget
	}{
		{"cost", domain.PerTaskBudget{MaxCostUSD: 1}},
		{"output tokens", domain.PerTaskBudget{MaxOutputTokens: 12}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			bundle := limitsBundle(domain.ConclaveWards{},
				domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationAlwaysOn, CanMessage: []string{"reviewer"}},
				domain.ConclaveVassal{Role: "reviewer", Workload: "worker", Activation: domain.ActivationOnMention})
			council := bundle.Conclaves["council"]
			council.Spec.BudgetRef = "budget"
			bundle.Conclaves["council"] = council
			bundle.Budgets = map[string]domain.Budget{"budget": {Spec: domain.BudgetSpec{PerTask: tc.budget}}}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
				t.Fatal(err)
			}
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
				Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("opening tasks = %d, error = %v", len(tasks), err)
			}
			if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: tasks[0].ID, WorkloadName: "worker", CostMicroUSD: 1_000_000, OutputTokens: 12}); err != nil {
				t.Fatal(err)
			}
			reply, err := b.PostMessage(ctx, room.ID, "reader", "reviewer", "review", "paid reply", tasks[0].ID)
			if err != nil || reply.ID == 0 {
				t.Errorf("paid reply was refused: %v", err)
			}
			msgs, err := st.ListConclaveMessages(ctx, room.ID, 0)
			if err != nil || len(msgs) == 0 || msgs[0].Body != "paid reply" {
				t.Errorf("paid reply missing: %v, %v", msgs, err)
			}
			tasks, err = st.ListTasks(ctx)
			if err != nil || len(tasks) != 1 {
				t.Errorf("tasks after paid reply = %d, error = %v", len(tasks), err)
			}
			room, err = st.GetConclave(ctx, room.ID)
			if err != nil || room.State != domain.ConclaveStateInputRequired {
				t.Errorf("council state = %s, error = %v", room.State, err)
			}
		})
	}
}

func TestWardPauseWriteRollsBackReply(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault string
	}{
		{"system message", `CREATE TRIGGER fail_ward BEFORE INSERT ON conclave_messages
			WHEN NEW.from_role = 'system' BEGIN SELECT RAISE(ABORT, 'ward unavailable'); END`},
		{"state update", `CREATE TRIGGER fail_ward BEFORE UPDATE OF state ON conclaves
			WHEN NEW.state = 'input-required' BEGIN SELECT RAISE(ABORT, 'ward unavailable'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			bundle := limitsBundle(domain.ConclaveWards{MaxMessages: 1}, domain.ConclaveVassal{Role: "reader", CanMessage: []string{"operator"}})
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, tc.fault); err != nil {
				t.Fatal(err)
			}
			if _, err := b.PostMessage(ctx, room.ID, "reader", "operator", "review", "reply", ""); err == nil {
				t.Error("post succeeded without recording the Ward pause")
			}
			msgs, err := st.ListConclaveMessages(ctx, room.ID, 0)
			if err != nil || len(msgs) != 0 {
				t.Errorf("failed write left messages: %v, %v", msgs, err)
			}
			if _, err := db.ExecContext(ctx, "DROP TRIGGER fail_ward"); err != nil {
				t.Fatal(err)
			}
			if _, err := b.PostMessage(ctx, room.ID, "reader", "operator", "review", "reply", ""); err != nil {
				t.Fatalf("retry: %v", err)
			}
			room, err = st.GetConclave(ctx, room.ID)
			if err != nil || room.State != domain.ConclaveStateInputRequired {
				t.Errorf("retry left room %s, %v", room.State, err)
			}
		})
	}
}

func TestBudgetBlocksWakingStoredMessage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		budgets map[string]domain.Budget
	}{
		{"spent", map[string]domain.Budget{"budget": {Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxInputTokens: 1}}}}},
		{"missing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			bundle := limitsBundle(domain.ConclaveWards{},
				domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationAlwaysOn},
				domain.ConclaveVassal{Role: "reviewer", Workload: "worker", Activation: domain.ActivationOnMention})
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
				t.Fatal(err)
			}
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
				Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("opening tasks = %d, error = %v", len(tasks), err)
			}
			if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: tasks[0].ID, WorkloadName: "worker", InputTokens: 1}); err != nil {
				t.Fatal(err)
			}
			msg := domain.ConclaveMessage{
				ConclaveID: room.ID, FromRole: "operator", ToRole: "reviewer", Kind: "decree", Body: "review",
			}
			msg.ID, err = st.AppendConclaveMessage(ctx, msg)
			if err != nil {
				t.Fatal(err)
			}
			tpl := bundle.Conclaves["council"]
			tpl.Spec.BudgetRef = "budget"
			bundle.Conclaves["council"] = tpl
			bundle.Budgets = tc.budgets
			if err := b.WakeMentions(ctx, tpl, msg); err != nil {
				t.Fatal(err)
			}
			tasks, err = st.ListTasks(ctx)
			if err != nil || len(tasks) != 1 {
				t.Errorf("wake added a task at the budget Ward: %d total, %v", len(tasks), err)
			}
		})
	}
}
