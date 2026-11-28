package conclave

import (
	"context"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// An opening that fails for another reason and then cannot record the
// cancellation of a task it already submitted says that the room was opened
// and cancelled, not that nothing was changed (issue 107 row 3).
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
	_, err := b.Open(ctx, "council", "t", "")
	msg, ok := store.AuditFailureText(err, "opening the conclave")
	if !ok {
		t.Fatalf("got %v, want a failed audit write", err)
	}
	if strings.Contains(msg, "nothing was changed") || !strings.Contains(msg, "was opened and then cancelled") || !strings.Contains(msg, "was cancelled, but an audit record could not be written") {
		t.Fatalf("text %q", msg)
	}
	if len(faults.Refused()) == 0 {
		t.Fatal("no audit write was attempted")
	}
}
