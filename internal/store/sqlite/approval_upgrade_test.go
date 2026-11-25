package sqlite

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// lastSchemaBeforeSingleUse is the newest migration of the schema that left
// used approvals in state approved.
const lastSchemaBeforeSingleUse = "0014_budget_reservations.sql"

// openPreviousSchema builds a database with the migrations up to the previous
// schema only, as an existing installation has it before the upgrade.
func openPreviousSchema(t *testing.T) *SQLiteStore {
	t.Helper()
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.db.ExecContext(ctx, `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.Name() > lastSchemaBeforeSingleUse {
			continue
		}
		raw, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, string(raw)); err != nil {
			t.Fatalf("apply %s: %v", e.Name(), err)
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, e.Name(), nowRFC3339()); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// M1: an approval the old code left approved after using it cannot be used
// once more after the upgrade. Every approved row from before the upgrade is
// expired, since the old code never recorded a use.
func TestUpgradeExpiresApprovalsApprovedBeforeIt(t *testing.T) {
	ctx := context.Background()
	st := openPreviousSchema(t)
	future := time.Now().UTC().Add(10 * time.Minute)
	seedApproval(t, st, "apr-used-by-old-code", future, domain.ApprovalApproved)
	seedApproval(t, st, "apr-pending", future, domain.ApprovalPending)
	seedApproval(t, st, "apr-denied", future, domain.ApprovalDenied)

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	want := map[string]domain.ApprovalStatus{
		"apr-used-by-old-code": domain.ApprovalExpired,
		"apr-pending":          domain.ApprovalPending,
		"apr-denied":           domain.ApprovalDenied,
	}
	for id, status := range want {
		got, err := st.GetApproval(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != status {
			t.Fatalf("%s: status %q after upgrade, want %q", id, got.Status, status)
		}
	}
	if err := consumeForTest(st, "apr-used-by-old-code"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("replay after upgrade: err %v, want ErrConflict", err)
	}

	// The migration runs once: an approval granted after the upgrade stays usable.
	seedApproval(t, st, "apr-after-upgrade", future, domain.ApprovalApproved)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := consumeForTest(st, "apr-after-upgrade"); err != nil {
		t.Fatalf("approval granted after the upgrade: %v", err)
	}
}

func consumeForTest(st *SQLiteStore, id string) error {
	return st.ConsumeApproval(context.Background(), store.ConsumeRequest{
		ID: id, TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1",
		Now: func() time.Time { return time.Now().UTC() },
	})
}
