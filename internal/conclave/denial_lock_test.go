package conclave

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// stallDenialStore holds the conclave.post denial row until release closes
// or the write's context ends.
type stallDenialStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
}

func (s *stallDenialStore) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	if rec.Action == "conclave.post" && rec.Outcome == "denied" {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.AppendAudit(ctx, rec)
}

// A refused post to an on-mention vassal writes its denial row after the
// room's wake lock is released, so a stalled audit store does not hold up a
// valid post to the same room (issue 179).
func TestDenialRowWrittenOutsideWakeLock(t *testing.T) {
	ctx := context.Background()
	base := brokerTestStore(t)
	if err := base.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	st := &stallDenialStore{Store: base, entered: make(chan struct{}), release: make(chan struct{})}
	bundle := limitsBundle(domain.ConclaveWards{},
		domain.ConclaveVassal{Role: "coder", Workload: "worker", Activation: domain.ActivationOnMention, CanMessage: []string{"operator"}},
		domain.ConclaveVassal{Role: "reviewer", Workload: "worker", Activation: domain.ActivationOnMention, CanMessage: []string{"operator"}},
	)
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle },
		Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
	room, err := b.Open(ctx, "council", "t", "")
	if err != nil {
		t.Fatal(err)
	}

	denied := make(chan error, 1)
	go func() {
		_, err := b.PostMessage(ctx, room.ID, "coder", "reviewer", "proposal", "not allowed", "")
		denied <- err
	}()
	<-st.entered

	valid := make(chan error, 1)
	go func() {
		_, err := b.PostMessage(ctx, room.ID, "operator", "reviewer", "decree", "review this", "")
		valid <- err
	}()
	waited := false
	select {
	case err := <-valid:
		if err != nil {
			t.Errorf("valid post: %v", err)
		}
	case <-time.After(time.Second):
		waited = true
		t.Error("valid post waited on the stalled denial row")
	}
	close(st.release)
	if err := <-denied; !errors.Is(err, ErrMessageDenied) {
		t.Fatalf("refused post: %v, want ErrMessageDenied", err)
	}
	if waited {
		if err := <-valid; err != nil {
			t.Errorf("valid post: %v", err)
		}
	}
}
