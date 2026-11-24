package controlplane

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
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
