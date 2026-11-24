package conclave

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type concurrentDecreeStore struct {
	store.Store
	read  sync.WaitGroup
	reads atomic.Int32
}

func (s *concurrentDecreeStore) GetConclave(ctx context.Context, id string) (domain.ConclaveSession, error) {
	room, err := s.Store.GetConclave(ctx, id)
	if s.reads.Add(-1) >= 0 {
		s.read.Done()
		s.read.Wait()
	}
	return room, err
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
			// Each concurrent decree starts from the active room, so none is a resume.
			barrier := &concurrentDecreeStore{Store: st}
			barrier.read.Add(tc.steps)
			barrier.reads.Store(int32(tc.steps))
			opts.Store = barrier
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
	for _, tc := range []struct{ name, format string }{
		{"api_key", "api_key=%s"},
		{"access_token", "access_token=%s"},
		{"quoted api key", `{"api_key":%q}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := brokerTestStore(t)
			bundle := limitsBundle(domain.ConclaveWards{}, domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationAlwaysOn})
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
				t.Fatal(err)
			}
			b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
				Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
			value := "fixture-sensitive-text-1234567"
			if _, err := b.Open(ctx, "council", "", fmt.Sprintf(tc.format, value)); err != nil {
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

func runLimitFixture(t *testing.T, wards domain.ConclaveWards, roles ...domain.ConclaveVassal) (store.Store, *Broker, *controlplane.Reconciler, *config.Bundle, *time.Time) {
	t.Helper()
	st := brokerTestStore(t)
	if err := st.UpsertWorkload(context.Background(), domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	bundle := limitsBundle(wards, roles...)
	clock := time.Now().UTC()
	now := func() time.Time { return clock }
	r := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle, Now: now})
	b := NewBroker(Options{Store: st, Reconciler: r, GetBundle: func() *config.Bundle { return bundle }, Now: now})
	return st, b, r, bundle, &clock
}

func TestRunExhaustionPausesAndKeepsReply(t *testing.T) {
	for _, externalWake := range []bool{false, true} {
		t.Run(fmt.Sprint(externalWake), func(t *testing.T) {
			ctx := context.Background()
			st, b, _, bundle, _ := runLimitFixture(t, domain.ConclaveWards{},
				domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationOnMention, CanMessage: []string{"reader", "operator"}})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			for i := range 6 {
				if _, err := b.PostMessage(ctx, room.ID, "operator", "all", "decree", fmt.Sprint(i), ""); err != nil {
					t.Fatal(err)
				}
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != 6 {
				t.Fatalf("reserved %d tasks, %v", len(tasks), err)
			}
			if externalWake {
				msg := domain.ConclaveMessage{ConclaveID: room.ID, FromRole: "operator", ToRole: "all", Kind: "decree", Body: "seventh decree"}
				msg.ID, err = st.AppendConclaveMessage(ctx, msg)
				if err == nil {
					err = b.WakeMentions(ctx, bundle.Conclaves["council"], msg)
				}
			} else {
				_, err = b.PostMessage(ctx, room.ID, "operator", "all", "decree", "seventh decree", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			room, err = st.GetConclave(ctx, room.ID)
			if err != nil || room.State != domain.ConclaveStateInputRequired || room.WardReason != "max_runs" {
				t.Errorf("run exhaustion left room %+v, %v", room, err)
			}
			reply, err := b.PostMessage(ctx, room.ID, "reader", "operator", "review", "paid reply", tasks[0].ID)
			if err != nil || reply.ID == 0 {
				t.Errorf("paid reply = %+v, %v", reply, err)
			}
			msgs, err := st.ListConclaveMessages(ctx, room.ID, 0)
			if err != nil || len(msgs) != 9 || msgs[7].FromRole != "system" || msgs[8] != reply {
				t.Errorf("want seventh decree, Ward notice and paid reply, got %+v, %v", msgs, err)
			}
			tasks, err = st.ListTasks(ctx)
			if err != nil || len(tasks) != 6 {
				t.Errorf("reserved %d tasks, %v", len(tasks), err)
			}
		})
	}
}

func TestCouncilRetryReservesAttempt(t *testing.T) {
	ctx := context.Background()
	st, b, r, _, _ := runLimitFixture(t, domain.ConclaveWards{},
		domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationAlwaysOn})
	room, err := b.Open(ctx, "council", "", "")
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("opening tasks = %d, %v", len(tasks), err)
	}
	current := tasks[0]
	for attempt := 2; attempt <= 7; attempt++ {
		if err := r.CancelTask(ctx, current.ID, "test"); err != nil {
			t.Fatal(err)
		}
		next, err := r.RetryTaskWithHandoff(ctx, current.ID, false, "")
		if attempt == 7 {
			if !errors.Is(err, store.ErrConclaveRunLimit) {
				t.Errorf("seventh attempt error = %v", err)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if role, err := b.ResolveRoleForTask(ctx, room.ID, "worker", next.ID); err != nil || role != "reader" {
			t.Errorf("attempt %d role = %q, %v", attempt, role, err)
		}
		replay, err := r.RetryTaskWithHandoff(ctx, current.ID, false, "")
		if err != nil || replay.ID != next.ID {
			t.Fatalf("attempt %d repeated as %q, %v", attempt, replay.ID, err)
		}
		current = next
	}
	if tasks, err := st.ListTasks(ctx); err != nil || len(tasks) != 6 {
		t.Errorf("reserved %d tasks, %v", len(tasks), err)
	}
}

func TestPaidReplyAtDeadline(t *testing.T) {
	for _, elapsed := range []time.Duration{15 * time.Minute, 16 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			ctx := context.Background()
			st, b, _, _, clock := runLimitFixture(t, domain.ConclaveWards{},
				domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationAlwaysOn, CanMessage: []string{"all"}},
				domain.ConclaveVassal{Role: "reviewer", Workload: "worker", Activation: domain.ActivationOnMention})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("opening tasks = %d, %v", len(tasks), err)
			}
			*clock = clock.Add(elapsed)
			reply, err := b.PostMessage(ctx, room.ID, "reader", "all", "review", "paid reply", tasks[0].ID)
			if err != nil || reply.ID == 0 {
				t.Errorf("paid reply = %+v, %v", reply, err)
			}
			if _, err := b.PostMessage(ctx, room.ID, "reader", "all", "review", "unassigned reply", ""); !errors.Is(err, ErrConclavePaused) {
				t.Errorf("unassigned reply error = %v", err)
			}
			msgs, err := st.ListConclaveMessages(ctx, room.ID, 0)
			if err != nil || len(msgs) != 2 || msgs[0] != reply || msgs[1].FromRole != "system" {
				t.Errorf("want paid reply and Ward notice, got %+v, %v", msgs, err)
			}
			room, err = st.GetConclave(ctx, room.ID)
			if err != nil || room.State != domain.ConclaveStateInputRequired || room.WardReason != "wall_time_reached" {
				t.Errorf("deadline left room %+v, %v", room, err)
			}
			if tasks, err := st.ListTasks(ctx); err != nil || len(tasks) != 1 {
				t.Errorf("reserved %d tasks, %v", len(tasks), err)
			}
		})
	}
}

func TestLegacyRoomUsesThreeSeatDefaults(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "legacy", State: domain.ConclaveStateActive, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(Options{Store: st})
	for i := 1; i <= 6; i++ {
		if _, err := b.PostMessage(ctx, "legacy", "reader", "operator", "review", fmt.Sprint(i), ""); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	if _, err := b.PostMessage(ctx, "legacy", "reader", "operator", "review", "extra", ""); !errors.Is(err, ErrConclavePaused) {
		t.Errorf("seventh post error = %v", err)
	}
	if limits := b.limits(nil, domain.Conclave{}, false); limits.MaxMessages != 6 || limits.MaxPingPong != 6 {
		t.Errorf("legacy limits = %+v", limits)
	}
}

func TestDecreeResumesRunWindow(t *testing.T) {
	for _, maxRuns := range []int{1, 6} {
		t.Run(fmt.Sprint(maxRuns), func(t *testing.T) {
			ctx := context.Background()
			st, b, _, _, _ := runLimitFixture(t, domain.ConclaveWards{MaxRuns: maxRuns},
				domain.ConclaveVassal{Role: "reader", Workload: "worker", Activation: domain.ActivationOnMention})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateConclaveState(ctx, room.ID, domain.ConclaveStateInputRequired, "max_runs"); err != nil {
				t.Fatal(err)
			}
			for i := range maxRuns + 1 {
				if _, err := b.PostMessage(ctx, room.ID, "operator", "all", "decree", fmt.Sprint(i), ""); err != nil {
					t.Fatal(err)
				}
			}
			if paused, err := st.GetConclave(ctx, room.ID); err != nil || paused.WardReason != "max_runs" {
				t.Fatalf("run window did not pause: %+v, %v", paused, err)
			}
			if _, err := b.PostMessage(ctx, room.ID, "operator", "all", "decree", "resume", ""); err != nil {
				t.Fatal(err)
			}
			if tasks, err := st.ListTasks(ctx); err != nil || len(tasks) != maxRuns+1 {
				t.Errorf("reserved %d tasks after resume, %v", len(tasks), err)
			}
			audit, err := st.ListAudit(ctx, 100)
			var resumes int
			for _, rec := range audit {
				if rec.Action == "conclave.run_window_reset" {
					resumes++
				}
			}
			if err != nil || resumes != 2 {
				t.Errorf("recorded %d resets, %v", resumes, err)
			}
		})
	}
}

func TestMessageBodySize(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"exact limit", strings.Repeat("x", 64<<10), true},
		{"over limit", strings.Repeat("x", 64<<10+1), false},
		{"oversize before trim", strings.Repeat(" ", 64<<10) + "x", false},
		{"bytes not runes", strings.Repeat("\u00e9", 32<<10) + "x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			_, b, _, _, _ := runLimitFixture(t, domain.ConclaveWards{})
			room, err := b.Open(ctx, "council", "", "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.PostMessage(ctx, room.ID, "operator", "all", "decree", tc.body, "")
			if (err == nil) != tc.want {
				t.Errorf("post error = %v, want success = %v", err, tc.want)
			}
			_, err = b.Open(ctx, "council", "", tc.body)
			if (err == nil) != tc.want {
				t.Errorf("opening decree error = %v, want success = %v", err, tc.want)
			}
		})
	}
}

type turnFailureStore struct {
	store.Store
	beforeCreate func(context.Context, domain.Task) error
}

func (s turnFailureStore) CreateTask(ctx context.Context, task domain.Task) error {
	if err := s.beforeCreate(ctx, task); err != nil {
		return err
	}
	return s.Store.CreateTask(ctx, task)
}

func TestRetryWakesUseOriginalMessage(t *testing.T) {
	ctx := context.Background()
	base, _, _, bundle, _ := runLimitFixture(t, domain.ConclaveWards{},
		domain.ConclaveVassal{Role: "first", Workload: "worker", Activation: domain.ActivationOnMention},
		domain.ConclaveVassal{Role: "second", Workload: "worker", Activation: domain.ActivationOnMention})
	fault := errors.New("second mention unavailable")
	failOnce := true
	st := turnFailureStore{Store: base, beforeCreate: func(_ context.Context, task domain.Task) error {
		if strings.HasSuffix(task.IdempotencyKey, "-second") && failOnce {
			failOnce = false
			return fault
		}
		return nil
	}}
	opts := Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
		Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})}
	b := NewBroker(opts)
	room, err := b.Open(ctx, "council", "", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := b.PostMessage(ctx, room.ID, "operator", "all", "decree", "review", "")
	if !errors.Is(err, fault) || first.ID == 0 {
		t.Fatalf("initial post = %+v, %v", first, err)
	}
	retry, err := NewBroker(opts).PostMessage(ctx, room.ID, "operator", "all", "decree", "review", "")
	if err != nil || retry != first {
		t.Errorf("retry = %+v, %v, want %+v", retry, err, first)
	}
	if msgs, err := st.ListConclaveMessages(ctx, room.ID, 0); err != nil || len(msgs) != 1 {
		t.Errorf("stored %d messages, %v", len(msgs), err)
	}
	if tasks, err := st.ListTasks(ctx); err != nil || len(tasks) != 2 {
		t.Errorf("reserved %d tasks, %v", len(tasks), err)
	}
}

func TestFailedOpenCancelsConcurrentWake(t *testing.T) {
	ctx := context.Background()
	base, _, _, bundle, _ := runLimitFixture(t, domain.ConclaveWards{},
		domain.ConclaveVassal{Role: "first", Workload: "worker", Activation: domain.ActivationAlwaysOn},
		domain.ConclaveVassal{Role: "second", Workload: "worker", Activation: domain.ActivationOnMention})
	fault := errors.New("initial turn unavailable")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	st := turnFailureStore{Store: base, beforeCreate: func(_ context.Context, task domain.Task) error {
		if strings.HasSuffix(task.IdempotencyKey, "-init-first") {
			close(entered)
			<-release
			return fault
		}
		return nil
	}}
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
		Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
	done := make(chan error, 1)
	go func() { _, err := b.Open(ctx, "council", "", ""); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("opening turn did not reach the store")
	}
	rooms, err := st.ListConclaves(ctx)
	if err != nil || len(rooms) != 1 {
		t.Fatalf("opening rooms = %d, %v", len(rooms), err)
	}
	if _, err := b.PostMessage(ctx, rooms[0].ID, "operator", "all", "decree", "wake second", ""); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-done; !errors.Is(err, fault) {
		t.Errorf("open error = %v", err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].State != domain.TaskStateCancelled {
		t.Errorf("failed open left tasks %+v, %v", tasks, err)
	}
}
