package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// Every approval decision is recorded before it is stored, so an audit store
// that refuses the write leaves the approval as it was (issue 107 row 3).
func TestApprovalDecisionsFailClosedOnAuditWrite(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// act runs with audit writes failing and returns the error and the
		// approval ID whose status must be unchanged ("" when none exists).
		act        func(t *testing.T, q *Queue, pm *PairingManager, apr domain.Approval) (string, error)
		wantStatus domain.ApprovalStatus
	}{
		{
			name: "request",
			act: func(t *testing.T, q *Queue, _ *PairingManager, _ domain.Approval) (string, error) {
				_, err := q.Request(ctx, "task-1", "triage-agent", "action:git.open_pr", map[string]string{"b": "y"}, time.Minute)
				return "", err
			},
		},
		{
			name: "approve",
			act: func(t *testing.T, q *Queue, _ *PairingManager, apr domain.Approval) (string, error) {
				_, err := q.Approve(ctx, apr.ID, apr.PayloadSHA256, "operator")
				return apr.ID, err
			},
			wantStatus: domain.ApprovalPending,
		},
		{
			name: "approve with a device signature",
			act: func(t *testing.T, q *Queue, pm *PairingManager, apr domain.Approval) (string, error) {
				priv, pubPEM := p256PEM(t)
				q.store.(*audittest.Store).Heal()
				code, _, _ := pm.StartPairing(time.Minute)
				dev, err := pm.CompletePairing(ctx, code, "phone", "ios", pubPEM)
				if err != nil {
					t.Fatal(err)
				}
				q.store.(*audittest.Store).Fail(nil)
				_, err = q.ApproveWithSignature(ctx, apr.ID, apr.PayloadSHA256, dev.ID, signApproval(t, priv, apr.ID, apr.PayloadSHA256))
				return apr.ID, err
			},
			wantStatus: domain.ApprovalPending,
		},
		{
			name: "deny",
			act: func(t *testing.T, q *Queue, _ *PairingManager, apr domain.Approval) (string, error) {
				_, err := q.Deny(ctx, apr.ID, "operator", "no")
				return apr.ID, err
			},
			wantStatus: domain.ApprovalPending,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := setupApprovalStore(t)
			faults := audittest.Wrap(st)
			q := NewQueue(faults, &countingApprover{})
			pm := NewPairingManager(faults)
			apr := requestOne(t, q)
			faults.Fail(nil)
			id, err := tc.act(t, q, pm, apr)
			if !errors.Is(err, store.ErrAuditWrite) {
				t.Fatalf("got %v, want ErrAuditWrite", err)
			}
			if id != "" {
				if s := approvalStatus(t, st, id); s != tc.wantStatus {
					t.Fatalf("status %s, want %s", s, tc.wantStatus)
				}
			}
			list, err := st.ListApprovals(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 {
				t.Fatalf("%d approvals stored, want only the one requested before the fault", len(list))
			}
		})
	}
}

// A device can approve, so pairing one is recorded first: a refused audit
// write stores no device.
func TestDevicePairingFailsClosedOnAuditWrite(t *testing.T) {
	ctx := context.Background()
	st := setupApprovalStore(t)
	faults := audittest.Wrap(st)
	pm := NewPairingManager(faults)
	_, pubPEM := p256PEM(t)
	code, _, _ := pm.StartPairing(time.Minute)
	faults.Fail(nil)
	if _, err := pm.CompletePairing(ctx, code, "phone", "ios", pubPEM); !errors.Is(err, store.ErrAuditWrite) {
		t.Fatalf("got %v, want ErrAuditWrite", err)
	}
	devs, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 0 {
		t.Fatalf("%d devices stored", len(devs))
	}
}
