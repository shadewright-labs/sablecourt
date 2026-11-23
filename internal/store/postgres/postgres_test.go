package postgres

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
)

func TestEmbeddedPostgresMigrationsPresent(t *testing.T) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one Postgres migration file")
	}
	raw, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read 0001_init.sql: %v", err)
	}
	sqlText := string(raw)
	for _, tbl := range []string{"workloads", "tasks", "task_events", "task_messages", "leases", "budget_ledger", "approvals", "checkpoints", "audit_log", "provider_prefs"} {
		if !strings.Contains(sqlText, "CREATE TABLE IF NOT EXISTS "+tbl) {
			t.Fatalf("missing table %q in Postgres migration", tbl)
		}
	}
}

func TestLivePostgresIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	st, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}
