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

// A decree that resumes a paused conclave names the resume when a later audit
// write fails, and the same decree replayed while its wake is pending changes
// nothing, so it names nothing.
func TestDecreeResumeAndReplayEffects(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	faults := audittest.Wrap(st)
	bundle := limitsBundle(domain.ConclaveWards{MaxMessages: 1, MaxRuns: 5},
		domain.ConclaveVassal{Role: "coder", Workload: "worker", CanMessage: []string{"operator"}},
		domain.ConclaveVassal{Role: "reviewer", Workload: "worker", Activation: domain.ActivationOnMention, CanMessage: []string{"operator"}},
	)
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(Options{
		Store:      faults,
		GetBundle:  func() *config.Bundle { return bundle },
		Reconciler: controlplane.NewReconciler(controlplane.Options{Store: faults, Bundle: bundle}),
	})
	room, err := b.Open(ctx, "council", "resume-and-replay", "")
	if err != nil {
		t.Fatal(err)
	}
	// First vassal message reaches MaxMessages=1 and pauses the conclave.
	if _, err := b.PostMessage(ctx, room.ID, "coder", "operator", "note", "hit limit", ""); err != nil {
		t.Fatal(err)
	}
	faults.Fail(audittest.Action("task.submit"))

	// Decree resumes the paused conclave to active and commits, then fails on task.submit audit.
	ctx1, eff1 := store.TrackEffects(ctx)
	_, err = b.PostMessage(ctx1, room.ID, "operator", "all", "decree", "@reviewer check", "")
	if _, ok := store.AuditFailureText(ctx1, err, "posting the decree"); !ok {
		t.Fatalf("got %v, want audit failure", err)
	}
	gotRoom, err := st.GetConclave(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoom.State != domain.ConclaveStateActive {
		t.Fatalf("state %s, want active", gotRoom.State)
	}
	// Finding 3(a): eff1 lists that the conclave moved to active.
	if !slices.Contains(eff1.List(), "conclave "+room.ID+" moved to active") {
		t.Fatalf("effects %q omit committed transition to active", eff1.List())
	}

	// Finding 3(c): replaying the exact same decree hits wake_pending=1 and stores
	// nothing, so it records no effect.
	ctx2, eff2 := store.TrackEffects(ctx)
	_, err = b.PostMessage(ctx2, room.ID, "operator", "all", "decree", "@reviewer check", "")
	if _, ok := store.AuditFailureText(ctx2, err, "posting the decree"); !ok {
		t.Fatalf("got %v, want audit failure", err)
	}
	if len(eff2.List()) != 0 {
		t.Fatalf("no-op wake_pending replay recorded effects %q, want none", eff2.List())
	}
}
