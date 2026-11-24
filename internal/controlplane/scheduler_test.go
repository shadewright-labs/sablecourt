package controlplane

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestSchedulerIdempotentFireAndReload(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.Migrate(ctx)

	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{
			"health-ctrl": {
				Metadata: domain.ObjectMeta{Name: "health-ctrl", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassDeterministicController},
			},
		},
		Schedules: map[string]domain.Schedule{
			"every-15m": {
				Metadata: domain.ObjectMeta{Name: "every-15m"},
				Spec: domain.ScheduleSpec{
					Cron:       "*/15 * * * *",
					TargetKind: "workload",
					TargetName: "health-ctrl",
					Enabled:    true,
				},
			},
		},
	}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "health-ctrl", Zone: domain.ZoneRuntime, Class: domain.ClassDeterministicController, ManifestDigest: "d1", AgentCardJSON: "{}",
	})

	rec := NewReconciler(Options{Store: st, Bundle: bundle})
	sched := NewScheduler(rec)
	fixed := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	sched.now = func() time.Time { return fixed }

	if err := sched.Reload(ctx, bundle); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	defer sched.Stop()

	sc := bundle.Schedules["every-15m"]
	if err := sched.Fire(ctx, sc); err != nil {
		t.Fatalf("first Fire: %v", err)
	}
	if err := sched.Fire(ctx, sc); err != nil {
		t.Fatalf("second Fire in the same minute slot: %v", err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("two Fires in one minute slot made %d tasks, want 1", len(tasks))
	}
}

type blockingScheduleStore struct {
	store.Store
	blocked atomic.Bool
	entered chan struct{}
	release chan struct{}
	fail    error
}

func (s *blockingScheduleStore) CreateTask(ctx context.Context, task domain.Task) error {
	if task.WorkloadName == "slow" && s.blocked.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
		return s.fail
	}
	return s.Store.CreateTask(ctx, task)
}

func TestSchedulerBlockedWorkflowDoesNotStallOtherSlots(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{
		Workloads: make(map[string]domain.Workload),
		Workflows: make(map[string]domain.Workflow),
	}
	for _, name := range []string{"slow", "fast"} {
		bundle.Workloads[name] = domain.Workload{Metadata: domain.ObjectMeta{Name: name}}
		bundle.Workflows[name] = domain.Workflow{Spec: domain.WorkflowSpec{
			Steps: []domain.WorkflowStep{{Name: "step", Workload: name}},
		}}
		if err := db.UpsertWorkload(ctx, domain.WorkloadRecord{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	st := &blockingScheduleStore{
		Store: db, entered: make(chan struct{}), release: make(chan struct{}), fail: errors.New("submission rejected"),
	}
	scheduler := NewScheduler(NewReconciler(Options{Store: st, Bundle: bundle}))
	scheduler.now = func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) }
	fire := func(name string) <-chan error {
		result := make(chan error, 1)
		go func() {
			result <- scheduler.Fire(ctx, domain.Schedule{
				Metadata: domain.ObjectMeta{Name: name},
				Spec:     domain.ScheduleSpec{TargetKind: "workflow", TargetName: name},
			})
		}()
		return result
	}
	slow := fire("slow")
	select {
	case <-st.entered:
	case err := <-slow:
		t.Fatalf("slow submission returned before entering store: %v", err)
	case <-time.After(5 * time.Second):
		close(st.release)
		<-slow
		t.Fatal("slow submission did not enter store")
	}
	fast, duplicate := fire("fast"), fire("slow")
	for _, run := range []struct {
		name   string
		result <-chan error
	}{
		{name: "unrelated schedule", result: fast},
		{name: "duplicate trigger", result: duplicate},
	} {
		select {
		case err := <-run.result:
			if err != nil {
				t.Errorf("%s: %v", run.name, err)
			}
		case <-time.After(time.Second):
			t.Errorf("%s stalled behind the blocked submission", run.name)
			defer func() { <-run.result }()
		}
	}
	close(st.release)
	if err := <-slow; !errors.Is(err, st.fail) {
		t.Errorf("slow submission error = %v, want %v", err, st.fail)
	}
	if t.Failed() {
		return
	}
	tasks, err := db.ListTasks(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].WorkloadName != "fast" {
		t.Fatalf("before retry: tasks=%v, err=%v", tasks, err)
	}
	if err := <-fire("slow"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	tasks, err = db.ListTasks(ctx)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("after retry: got %d tasks, want 2, err=%v", len(tasks), err)
	}
}

func TestSchedulerWorkflowDedupeAndReload(t *testing.T) {
	newBundle := func(withWorkflow bool) *config.Bundle {
		b := &config.Bundle{
			Workloads: map[string]domain.Workload{
				"health-ctrl": {
					Metadata: domain.ObjectMeta{Name: "health-ctrl", Zone: domain.ZoneRuntime},
					Spec:     domain.WorkloadSpec{Class: domain.ClassDeterministicController},
				},
			},
			Workflows: map[string]domain.Workflow{},
			Schedules: map[string]domain.Schedule{
				"wf-every-15m": {
					Metadata: domain.ObjectMeta{Name: "wf-every-15m"},
					Spec: domain.ScheduleSpec{
						Cron:       "*/15 * * * *",
						TargetKind: "workflow",
						TargetName: "triage-wf",
						Input:      `{"ok":true}`,
						Enabled:    true,
					},
				},
				"wf-other": {
					Metadata: domain.ObjectMeta{Name: "wf-other"},
					Spec: domain.ScheduleSpec{
						Cron:       "*/15 * * * *",
						TargetKind: "workflow",
						TargetName: "triage-wf",
						Input:      `{"other":true}`,
						Enabled:    true,
					},
				},
			},
		}
		if withWorkflow {
			b.Workflows["triage-wf"] = domain.Workflow{
				Metadata: domain.ObjectMeta{Name: "triage-wf"},
				Spec: domain.WorkflowSpec{
					Steps: []domain.WorkflowStep{
						{Name: "step1", Workload: "health-ctrl", Prompt: "check"},
					},
				},
			}
		}
		return b
	}

	type step struct {
		offset  time.Duration
		sched   string
		reload  *config.Bundle
		manual  bool
		wantErr bool
	}
	cases := []struct {
		name      string
		initialWF bool
		steps     []step
		wantTasks int
	}{
		{
			name:      "double fire in same minute slot creates one workflow run",
			initialWF: true,
			steps: []step{
				{offset: 0, sched: "wf-every-15m"},
				{offset: 30 * time.Second, sched: "wf-every-15m"},
			},
			wantTasks: 1,
		},
		{
			name:      "dedupe record survives reload in same minute slot",
			initialWF: true,
			steps: []step{
				{offset: 0, sched: "wf-every-15m"},
				{offset: 10 * time.Second, sched: "wf-every-15m", reload: newBundle(true)},
			},
			wantTasks: 1,
		},
		{
			name:      "next minute slot fires new workflow run",
			initialWF: true,
			steps: []step{
				{offset: 0, sched: "wf-every-15m"},
				{offset: time.Minute, sched: "wf-every-15m"},
			},
			wantTasks: 2,
		},
		{
			name:      "distinct schedules in same minute slot each run once",
			initialWF: true,
			steps: []step{
				{offset: 0, sched: "wf-every-15m"},
				{offset: 0, sched: "wf-other"},
				{offset: 5 * time.Second, sched: "wf-every-15m"},
				{offset: 5 * time.Second, sched: "wf-other"},
			},
			wantTasks: 2,
		},
		{
			name:      "failed fire does not burn minute slot",
			initialWF: false,
			steps: []step{
				{offset: 0, sched: "wf-every-15m", wantErr: true},
				{offset: 10 * time.Second, sched: "wf-every-15m", reload: newBundle(true)},
				{offset: 20 * time.Second, sched: "wf-every-15m"},
			},
			wantTasks: 1,
		},
		{
			name:      "manual TriggerNow still starts new run in same slot",
			initialWF: true,
			steps: []step{
				{offset: 0, sched: "wf-every-15m"},
				{offset: 5 * time.Second, sched: "wf-every-15m", manual: true},
			},
			wantTasks: 2,
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
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
				Name: "health-ctrl", Zone: domain.ZoneRuntime, Class: domain.ClassDeterministicController,
				ManifestDigest: "d1", AgentCardJSON: "{}",
			}); err != nil {
				t.Fatalf("UpsertWorkload: %v", err)
			}

			bundle := newBundle(tc.initialWF)
			rec := NewReconciler(Options{Store: st, Bundle: bundle})
			sched := NewScheduler(rec)
			base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
			cur := base
			sched.now = func() time.Time { return cur }

			if err := sched.Reload(ctx, bundle); err != nil {
				t.Fatalf("initial Reload: %v", err)
			}
			defer sched.Stop()

			for i, sp := range tc.steps {
				cur = base.Add(sp.offset)
				if sp.reload != nil {
					rec.Reload(sp.reload, nil)
					if err := sched.Reload(ctx, sp.reload); err != nil {
						t.Fatalf("step %d Reload: %v", i, err)
					}
					bundle = sp.reload
				}
				sc := bundle.Schedules[sp.sched]
				var fireErr error
				if sp.manual {
					fireErr = sched.TriggerNow(ctx, sc)
				} else {
					fireErr = sched.Fire(ctx, sc)
				}
				if (fireErr != nil) != sp.wantErr {
					t.Fatalf("step %d err=%v, wantErr=%v", i, fireErr, sp.wantErr)
				}
			}

			tasks, err := st.ListTasks(ctx)
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if len(tasks) != tc.wantTasks {
				t.Fatalf("got %d tasks, want %d", len(tasks), tc.wantTasks)
			}
		})
	}
}
