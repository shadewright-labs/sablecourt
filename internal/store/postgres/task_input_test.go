package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

// TestLivePostgresTaskInputRedactedAtStoreWrite runs the same store write
// rule as the SQLite test against a fresh database (issue 261).
func TestLivePostgresTaskInputRedactedAtStoreWrite(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	st := openFreshPostgres(t, dsn)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	storetest.CheckTaskInputRedacted(t, st)
}
