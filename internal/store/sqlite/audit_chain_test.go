package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func TestAuditHashChainDetectsTampering(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := st.AppendAudit(ctx, domain.AuditRecord{
			Category:     "policy",
			WorkloadName: "triage-agent",
			Action:       "action:git.open_pr",
			Outcome:      "allow",
			DetailJSON:   `{"step":1}`,
			CreatedAt:    time.Now().UTC().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("AppendAudit %d: %v", i, err)
		}
	}

	n, err := st.VerifyAuditChain(ctx)
	if err != nil || n != 3 {
		t.Fatalf("expected 3 verified rows, got n=%d err=%v", n, err)
	}

	// Tamper with row 2 directly in SQLite and confirm VerifyAuditChain catches it.
	if _, err := st.db.ExecContext(ctx, `UPDATE audit_log SET outcome = 'deny' WHERE id = 2`); err != nil {
		t.Fatalf("tamper row 2: %v", err)
	}
	_, err = st.VerifyAuditChain(ctx)
	if err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("expected tamper detection error, got %v", err)
	}
}
