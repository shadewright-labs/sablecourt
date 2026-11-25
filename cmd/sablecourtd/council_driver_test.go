package main

import (
	"context"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestDaemonCouncilPass(t *testing.T) {
	for _, phase := range []string{"startup", "tick"} {
		t.Run(phase, func(t *testing.T) {
			st := daemonTestStore(t)
			cs, err := store.Councils(st)
			if err != nil {
				t.Fatal(err)
			}
			r := controlplane.NewReconciler(controlplane.Options{Store: st})
			taskPass := r.Recover
			if phase == "tick" {
				taskPass = r.ReconcileOnce
			}
			b := conclave.NewBroker(conclave.Options{Store: st, Reconciler: r})
			room := domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive, CreatedAt: time.Now().Add(-time.Hour)}
			spec := store.CouncilSpec{Question: "Review?", Members: []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: domain.ZoneSandbox}}}
			if err := cs.CreateCouncil(t.Context(), room, spec); err != nil {
				t.Fatal(err)
			}
			if err := reconcileDaemonPass(t.Context(), func(ctx context.Context) error {
				s, err := cs.CouncilSnapshot(ctx, room.ID)
				if err == nil && s.Session.State != domain.ConclaveStateActive {
					t.Fatal("council ran before task reconciliation")
				}
				if err != nil {
					return err
				}
				return taskPass(ctx)
			}, b); err != nil {
				t.Fatal(err)
			}
			s, err := cs.CouncilSnapshot(t.Context(), room.ID)
			if err != nil || s.Session.WardReason != "deadline" {
				t.Fatal("expired council was not closed", err)
			}
		})
	}
}
