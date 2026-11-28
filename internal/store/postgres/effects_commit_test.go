package postgres

import (
	"database/sql"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

var (
	faultOnce   sync.Once
	faultDriver *storetest.FaultDriver
)

// No store method records an effect before the transaction that makes it
// commits: with every commit failing, a write must fail and record nothing.
func TestLivePostgresEffectsFollowCommitIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	faultOnce.Do(func() {
		faultDriver = storetest.NewFaultDriver(stdlib.GetDefaultDriver())
		sql.Register("pgx-commit-fault", faultDriver)
	})
	open := func(t *testing.T) store.Store {
		t.Helper()
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close()
		schema := "sc_effects_" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
			t.Fatalf("create schema: %v", err)
		}
		t.Cleanup(func() {
			if a, err := sql.Open("pgx", dsn); err == nil {
				_, _ = a.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
				_ = a.Close()
			}
		})
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			// The parse error may quote the DSN, so it is not printed.
			t.Fatal("SABLECOURT_TEST_POSTGRES_DSN cannot be parsed as a pgx connection string")
		}
		cfg.RuntimeParams["search_path"] = schema
		name := stdlib.RegisterConnConfig(cfg)
		t.Cleanup(func() { stdlib.UnregisterConnConfig(name) })
		db, err := sql.Open("pgx-commit-fault", name)
		if err != nil {
			t.Fatal(err)
		}
		st := &PostgresStore{db: db}
		t.Cleanup(func() { _ = st.Close() })
		if err := st.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		return st
	}
	storetest.CheckEffectsFollowCommit(t, faultDriver, open, storetest.StoreScenarios())
}
