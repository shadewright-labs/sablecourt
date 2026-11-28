package controlplane

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

func taskInputReconciler(t *testing.T, adapter scruntime.Adapter) (*Reconciler, *sqlite.SQLiteStore) {
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
	wl := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "worker", Zone: domain.ZoneRuntime},
		Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "m", AgentCardJSON: "{}"}); err != nil {
		t.Fatalf("UpsertWorkload: %v", err)
	}
	pol := domain.Policy{Metadata: domain.ObjectMeta{Name: "default"}, Spec: domain.PolicySpec{DefaultEffect: domain.EffectDeny}}
	rec := NewReconciler(Options{
		Store:    st,
		Bundle:   &config.Bundle{Policies: map[string]domain.Policy{"default": pol}, Workloads: map[string]domain.Workload{"worker": wl}},
		Policy:   policy.NewEngine([]domain.Policy{pol}, nil),
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassLongLivedAgent: adapter},
	})
	return rec, st
}

// TestSubmittedTaskInputRedactedEverywhere submits input carrying a secret of
// every pattern rule and a daemon-held value, in every shape, and checks the
// submit answer, the stored row and message, every read path and the launch
// input handed to the runtime (issue 261).
func TestSubmittedTaskInputRedactedEverywhere(t *testing.T) {
	ctx := context.Background()
	for i, c := range storetest.TaskInputCorpus() {
		t.Run(c.Name, func(t *testing.T) {
			adapter := &fakeAdapter{nextState: scruntime.StateRunning}
			rec, st := taskInputReconciler(t, adapter)
			task, err := rec.SubmitTask(ctx, "worker", fmt.Sprintf("idem-%d", i), c.Input)
			if err != nil {
				t.Fatalf("SubmitTask: %v", err)
			}
			storetest.AssertNoSecret(t, "submit answer", c, TaskForRead(task).InputJSON)
			reconcile(t, rec)
			storetest.AssertNoSecret(t, "launch input", c, adapter.lastSpec.InputJSON)
			stored, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			storetest.AssertNoSecret(t, "stored input_json", c, stored.InputJSON)
			storetest.AssertNoSecret(t, "task read", c, TaskForRead(stored).InputJSON)
			msgs, err := st.ListTaskMessages(ctx, task.ID)
			if err != nil || len(msgs) == 0 {
				t.Fatalf("ListTaskMessages = %d, %v", len(msgs), err)
			}
			for _, m := range TaskMessagesForRead(msgs) {
				storetest.AssertNoSecret(t, "task message", c, m.Content)
			}
		})
	}
}

type undeliveredAdapter struct{ fakeAdapter }

func (a *undeliveredAdapter) Start(_ context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	a.lastSpec = spec
	return scruntime.Handle{AdapterName: "fake", ExternalID: spec.TaskID}, fmt.Errorf("attach: %w", scruntime.ErrInputNotDelivered)
}

// TestInputNotDeliveredFailsTask: a runtime that cannot hand the input to
// the workload fails the task with reason input_not_delivered (issue 261).
func TestInputNotDeliveredFailsTask(t *testing.T) {
	ctx := context.Background()
	rec, st := taskInputReconciler(t, &undeliveredAdapter{})
	task, err := rec.SubmitTask(ctx, "worker", "idem-undelivered", `{"prompt":"hello"}`)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if err := errors.Join(rec.ReconcileOnce(ctx), waitForLaunches(t, rec)); err == nil {
		t.Log("reconcile returned no error")
	}
	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != domain.TaskStateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	events, err := st.ListTaskEvents(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	last := events[len(events)-1]
	if last.Reason != "input_not_delivered" {
		t.Fatalf("last event = %s %s, want input_not_delivered", last.Reason, last.FactJSON)
	}
}
