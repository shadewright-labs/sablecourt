package conclave

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type closeFailureStore struct {
	store.Store
	err error
}

func (s closeFailureStore) UpdateConclaveState(context.Context, string, domain.ConclaveState, string) error {
	return s.err
}

// A failed opening whose room cannot be closed still cancels the turns it
// already started, and reports the opening, close and cancellation errors.
func TestOpenCleanupCancelsTurnsWhenCloseFails(t *testing.T) {
	ctx := context.Background()
	base := brokerTestStore(t)
	closeErr := errors.New("close unavailable")
	st := closeFailureStore{Store: base, err: closeErr}
	bundle := limitsBundle(domain.ConclaveWards{},
		domain.ConclaveVassal{Role: "first", Workload: "worker", Activation: domain.ActivationAlwaysOn},
		domain.ConclaveVassal{Role: "second", Workload: "undeclared", Activation: domain.ActivationAlwaysOn},
	)
	if err := base.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
		Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
	_, err := b.Open(ctx, "council", "t", "")
	if err == nil || !errors.Is(err, closeErr) {
		t.Fatalf("open error %v, want it to carry the close error", err)
	}
	if !strings.Contains(err.Error(), `open vassal "second"`) {
		t.Fatalf("open error %v lost the opening failure", err)
	}
	tasks, err := base.ListTasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks %v %v", tasks, err)
	}
	if tasks[0].State != domain.TaskStateCancelled {
		t.Errorf("the started turn is %s, want cancelled although the close failed", tasks[0].State)
	}
}
