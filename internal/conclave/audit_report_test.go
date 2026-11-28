package conclave

import (
	"context"
	"slices"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// An opening that fails for another reason and then cannot record the
// cancellation of a task it already submitted lists what the opening and its
// cleanup changed (issue 107 row 3, issue 179).
func TestOpenCleanupAuditFailureSaysWhatHappened(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	faults := audittest.Wrap(st)
	bundle := limitsBundle(domain.ConclaveWards{},
		domain.ConclaveVassal{Role: "first", Workload: "worker", Activation: domain.ActivationAlwaysOn},
		domain.ConclaveVassal{Role: "second", Workload: "undeclared", Activation: domain.ActivationAlwaysOn},
	)
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(Options{Store: faults, GetBundle: func() *config.Bundle { return bundle },
		Reconciler: controlplane.NewReconciler(controlplane.Options{Store: faults, Bundle: bundle})})
	faults.Fail(audittest.Action("task.cancel"))
	openCtx, effects := store.TrackEffects(ctx)
	_, err := b.Open(openCtx, "council", "t", "")
	msg, ok := store.AuditFailureText(openCtx, err, "opening the conclave")
	if !ok {
		t.Fatalf("got %v, want a failed audit write", err)
	}
	if msg != audittest.Answer("opening the conclave", effects.List()...) {
		t.Fatalf("answer %q does not list the recorded effects %q", msg, effects.List())
	}
	rooms, err := st.ListConclaves(ctx)
	if err != nil || len(rooms) != 1 {
		t.Fatalf("rooms %v %v", rooms, err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks %v %v", tasks, err)
	}
	want := []string{
		"conclave " + rooms[0].ID + " was opened",
		"task " + tasks[0].ID + " was created",
		"a message was added to task " + tasks[0].ID,
		"conclave " + rooms[0].ID + " moved to cancelled",
		"task " + tasks[0].ID + " moved from submitted to cancelled",
		// The task held no lease, so revoking its leases changed nothing.
	}
	if got := effects.List(); !slices.Equal(got, want) {
		t.Fatalf("effects %q, want %q", got, want)
	}
	if len(faults.Refused()) == 0 {
		t.Fatal("no audit write was attempted")
	}
}
