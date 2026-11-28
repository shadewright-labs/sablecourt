package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// A lease minted after a Ward trip (a launch that claimed its turn before the
// trip and issued its token after it) is revoked by the next reconcile pass
// instead of renewed, so the gate refuses it within one pass.
func TestWardPausedRoomRevokesTurnLeaseOnObserve(t *testing.T) {
	for _, tc := range []struct {
		reason string
		live   bool
	}{
		{"max_messages_reached(1)", false},
		{"ping_pong_detected(2)", false},
		{"budget_evidence_unavailable", false},
		{store.ConclaveMaxRunsWard, true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, _ := regressionFixture(t)
			w := r.bundle.Workloads["worker"]
			w.Metadata.Zone = domain.ZoneSandbox
			w.Spec.Class = domain.ClassCodingAgent
			w.Spec.Coding = &domain.CodingConfig{RepoPath: "project"}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: w.Metadata.Zone, Class: w.Spec.Class}); err != nil {
				t.Fatal(err)
			}
			duration := time.Hour
			seat := domain.ConclaveVassal{Role: "reader", Workload: "worker", CanMessage: []string{"all"}}
			spec := store.CouncilSpec{Question: "Review?", Members: []store.CouncilMember{{Vassal: seat, Zone: domain.ZoneSandbox}},
				Checkout: store.CouncilCheckout{Source: "project"}, Duration: &duration}
			if err := st.CreateCouncil(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive, CreatedAt: r.now()}, spec); err != nil {
				t.Fatal(err)
			}
			a := store.ConclaveAssignment{ConclaveID: "room", Role: "reader", Round: 1, Attempt: 1}
			input := `{"conclaveId":"room","role":"reader","round":1,"attempt":1,"context_packet":"review"}`
			task, err := store.SubmitConclaveTurn(ctx, "room", a.TurnKey(), store.ConclaveLimits{}, func(ctx context.Context) (domain.Task, error) {
				return r.SubmitTask(ctx, "worker", a.TurnKey(), input)
			})
			if err != nil {
				t.Fatal(err)
			}
			adapter := &writerObservation{}
			adapter.nextState = scruntime.StateRunning
			if err := launchForTest(ctx, r, task, w, adapter); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateConclaveState(ctx, "room", domain.ConclaveStateInputRequired, tc.reason); err != nil {
				t.Fatal(err)
			}
			late := "late-lease-" + task.ID
			if err := st.CreateLease(ctx, domain.Lease{TokenSHA256: late, TaskID: task.ID, WorkloadName: "worker",
				ExpiresAt: time.Now().UTC().Add(time.Minute), CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			task, err = st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.observeTask(ctx, task); err != nil {
				t.Fatalf("observeTask: %v", err)
			}
			_, err = st.GetActiveLeaseByTokenHash(ctx, late, r.now())
			if live := err == nil; live != tc.live {
				t.Fatalf("late lease live = %v (%v), want %v", live, err, tc.live)
			}
			if !tc.live && !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("late lease lookup = %v", err)
			}
			_, err = st.GetActiveLeaseByTokenHash(ctx, model.HashToken(adapter.lastSpec.TaskToken), r.now())
			if live := err == nil; live != tc.live {
				t.Fatalf("launch lease live = %v (%v), want %v", live, err, tc.live)
			}
		})
	}
}
