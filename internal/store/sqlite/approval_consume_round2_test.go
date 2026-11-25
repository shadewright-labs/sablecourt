package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// Medium 3: another connection holds the database's writer lock while the
// approval expires. The use must wait for the writer before it reads the
// clock, so it sees the expiry and refuses.
func TestConsumeApprovalTakesTheWriterBeforeTheClock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sablecourt.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	seedApproval(t, st, "apr-writer", start.Add(time.Minute), domain.ApprovalApproved)

	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	clock := start
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	done := make(chan error, 1)
	go func() {
		done <- st.ConsumeApproval(ctx, store.ConsumeRequest{
			ID: "apr-writer", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: now,
			Audit: domain.AuditRecord{Category: "approval", TaskID: "task-1", Action: "action:git.open_pr", Outcome: "consumed"},
		})
	}()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	clock = start.Add(2 * time.Minute)
	mu.Unlock()
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrExpired) {
		t.Fatalf("use that waited for the writer past expiry: err %v, want ErrExpired", err)
	}
	if got, _ := st.GetApproval(ctx, "apr-writer"); got.Status != domain.ApprovalExpired {
		t.Fatalf("status %q, want expired", got.Status)
	}
}

// Medium 4: a use without an audit record is refused and leaves the approval
// unused; there is no consumption without its audit row.
func TestConsumeApprovalRequiresAnAuditRecord(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedApproval(t, st, "apr-noaudit", time.Now().UTC().Add(time.Minute), domain.ApprovalApproved)
	for _, rec := range []domain.AuditRecord{{}, {Category: "approval"}, {Category: "approval", Action: "action:git.open_pr"}} {
		err := st.ConsumeApproval(ctx, store.ConsumeRequest{ID: "apr-noaudit", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1",
			Now: func() time.Time { return time.Now().UTC() }, Audit: rec})
		if !errors.Is(err, store.ErrAuditRequired) {
			t.Fatalf("audit %+v: err %v, want ErrAuditRequired", rec, err)
		}
	}
	if got, _ := st.GetApproval(ctx, "apr-noaudit"); got.Status != domain.ApprovalApproved {
		t.Fatalf("status %q, want approved", got.Status)
	}
	if recs, _ := st.ListAudit(ctx, 10); len(recs) != 0 {
		t.Fatalf("audit rows written: %+v", recs)
	}
}

// useAudit is a complete consumption audit record for store tests.
var useAudit = domain.AuditRecord{Category: "approval", TaskID: "task-1", Action: "action:git.open_pr", Outcome: "consumed"}
