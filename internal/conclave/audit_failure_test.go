package conclave

import (
	"context"
	"errors"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// The broker's audit writes by class (issue 107 row 3): opening a room and
// storing a relic are recorded first and refused when the record cannot be
// written; a refused message stays refused with its record logged.
func TestBrokerAuditWriteFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		fail    func(domain.AuditRecord) bool
		act     func(b *Broker, roomID string) error
		wantErr error
		check   func(t *testing.T, st store.Store, roomID string)
	}{
		{
			name:    "open is refused and creates no room",
			fail:    audittest.Action("conclave.open"),
			act:     func(b *Broker, _ string) error { _, err := b.Open(ctx, "council", "second", ""); return err },
			wantErr: store.ErrAuditWrite,
			check: func(t *testing.T, st store.Store, _ string) {
				rooms, err := st.ListConclaves(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(rooms) != 1 {
					t.Fatalf("%d rooms, want only the one opened before the fault", len(rooms))
				}
			},
		},
		{
			name: "relic is refused and not stored",
			fail: audittest.Action("conclave.relic_put"),
			act: func(b *Broker, roomID string) error {
				_, err := b.PutRelic(ctx, roomID, "coder", "notes", "", "body")
				return err
			},
			wantErr: store.ErrAuditWrite,
			check: func(t *testing.T, st store.Store, roomID string) {
				if _, err := st.GetConclaveRelic(ctx, roomID, "notes"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("relic stored without its record: %v", err)
				}
			},
		},
		{
			name: "a refused message stays refused",
			fail: audittest.Action("conclave.post"),
			act: func(b *Broker, roomID string) error {
				_, err := b.PostMessage(ctx, roomID, "coder", "reviewer", "proposal", "not allowed", "")
				return err
			},
			wantErr: ErrMessageDenied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := brokerTestStore(t)
			faults := audittest.Wrap(st)
			bundle := limitsBundle(domain.ConclaveWards{}, domain.ConclaveVassal{Role: "coder", CanMessage: []string{"operator"}})
			b := NewBroker(Options{Store: faults, GetBundle: func() *config.Bundle { return bundle }})
			room, err := b.Open(ctx, "council", "first", "")
			if err != nil {
				t.Fatal(err)
			}
			faults.Fail(tc.fail)
			if err := tc.act(b, room.ID); !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
			if len(faults.Refused()) == 0 {
				t.Fatal("no audit write was attempted")
			}
			if tc.check != nil {
				tc.check(t, st, room.ID)
			}
		})
	}
}
