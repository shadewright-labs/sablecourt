package conclave

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// An audit row a store writes inside its own transaction fails like any other
// audit write: the error wraps store.ErrAuditWrite, the answer comes from
// AuditFailureText without the database error, and nothing is committed.
func TestTransactionalConclaveCloseAuditWrapsErrAuditWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	bundle := limitsBundle(domain.ConclaveWards{}, domain.ConclaveVassal{Role: "coder", CanMessage: []string{"operator"}})
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
	room, err := b.Open(ctx, "council", "close-test", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER fail_close_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = 'conclave.close' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	closeCtx, _ := store.TrackEffects(ctx)
	err = st.UpdateConclaveState(closeCtx, room.ID, domain.ConclaveStateCompleted, "")
	if !errors.Is(err, store.ErrAuditWrite) {
		t.Fatalf("got %v, want errors.Is(err, store.ErrAuditWrite)", err)
	}
	msg, ok := store.AuditFailureText(closeCtx, err, "closing the conclave")
	if !ok || strings.Contains(msg, "audit unavailable") || strings.Contains(err.Error(), "audit unavailable") {
		t.Fatalf("answer %q (%v) for %v", msg, ok, err)
	}
	if want := "closing the conclave refused: its audit record could not be written, nothing was changed"; msg != want {
		t.Fatalf("answer %q, want %q", msg, want)
	}
	if got, err := st.GetConclave(ctx, room.ID); err != nil || got.State != domain.ConclaveStateActive {
		t.Fatalf("conclave %+v, %v; want it still active", got, err)
	}
}
