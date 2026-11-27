package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
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
	cacheMig, err := migrationFS.ReadFile("migrations/0011_budget_cache_tokens.sql")
	if err != nil {
		t.Fatalf("read 0011_budget_cache_tokens.sql: %v", err)
	}
	for _, col := range []string{"cache_write_tokens", "cache_read_tokens"} {
		if !strings.Contains(string(cacheMig), col) {
			t.Fatalf("missing column %q in 0011_budget_cache_tokens.sql", col)
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

func TestLivePostgresConsumeApprovalIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	ctx := context.Background()
	st, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	taskID := "task-consume-" + suffix
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"})
	if err := st.CreateTask(ctx, domain.Task{ID: taskID, IdempotencyKey: taskID, WorkloadName: "triage-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	now := time.Now().UTC()
	seed := func(id string, expires time.Time) {
		t.Helper()
		if err := st.CreateApproval(ctx, domain.Approval{ID: id, TaskID: taskID, WorkloadName: "triage-agent", Capability: "action:git.open_pr", PayloadCanonicalJSON: `{}`, PayloadSHA256: "d1", ExpiresAt: expires}); err != nil {
			t.Fatalf("CreateApproval: %v", err)
		}
		if err := st.DecideApproval(ctx, id, "d1", domain.ApprovalApproved, "operator", `{}`); err != nil {
			t.Fatalf("DecideApproval: %v", err)
		}
	}

	live := "apr-live-" + suffix
	seed(live, now.Add(time.Minute))
	if err := st.ConsumeApproval(ctx, live, taskID, "action:git.open_pr", "d2"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong digest: err %v, want ErrConflict", err)
	}
	var wg sync.WaitGroup
	var ok atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st.ConsumeApproval(ctx, live, taskID, "action:git.open_pr", "d1") == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d uses succeeded, want exactly 1", ok.Load())
	}
	if got, _ := st.GetApproval(ctx, live); got.Status != domain.ApprovalConsumed {
		t.Fatalf("status %q, want consumed", got.Status)
	}

	old := "apr-old-" + suffix
	seed(old, now.Add(-time.Second))
	for i := range 2 { // a refusal changes nothing and refuses again
		if err := st.ConsumeApproval(ctx, old, taskID, "action:git.open_pr", "d1"); !errors.Is(err, store.ErrExpired) {
			t.Fatalf("expired, use %d: err %v, want ErrExpired", i+1, err)
		}
		if got, _ := st.GetApproval(ctx, old); got.Status != domain.ApprovalApproved {
			t.Fatalf("expired, use %d: status %q, want approved", i+1, got.Status)
		}
	}

	// The expiry is compared as a time, not as text: "...:05Z" sorts after
	// "...:05.5Z" as text although it is earlier.
	whole := time.Now().UTC().Truncate(time.Second)
	textTrap := "apr-text-" + suffix
	seed(textTrap, whole)
	if err := st.ConsumeApproval(ctx, textTrap, taskID, "action:git.open_pr", "d1"); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("use after a whole-second expiry: err %v, want ErrExpired", err)
	}
	fraction := "apr-frac-" + suffix
	// Its own deadline, taken now and well ahead, so setup time cannot pass it.
	seed(fraction, time.Now().UTC().Truncate(time.Second).Add(10*time.Second+500*time.Millisecond))
	if err := st.ConsumeApproval(ctx, fraction, taskID, "action:git.open_pr", "d1"); err != nil {
		t.Fatalf("use before a fractional expiry: %v", err)
	}
}

// A use that waits for a row lock held across the expiry is refused, whatever
// the holder does with the row: a committed update makes PostgreSQL evaluate
// an UPDATE's predicate again, but a rolled-back update or a lock taken by
// SELECT FOR UPDATE or FOR SHARE does not, so a clock read before the wait
// would pass. Each case first sees the use blocked by the holder while the
// approval is still live, and sees it still blocked after the expiry.
func TestLivePostgresConsumeApprovalAfterLockWaitIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	ctx := context.Background()
	st, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	taskID := "task-wait-" + suffix
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"})
	if err := st.CreateTask(ctx, domain.Task{ID: taskID, IdempotencyKey: taskID, WorkloadName: "triage-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	cases := []struct {
		name    string
		lock    string
		release func(*sql.Tx) error
	}{
		{"committed update", `UPDATE approvals SET decided_by = decided_by WHERE id = $1`, (*sql.Tx).Commit},
		{"rolled back update", `UPDATE approvals SET decided_by = decided_by WHERE id = $1`, (*sql.Tx).Rollback},
		{"select for update", `SELECT id FROM approvals WHERE id = $1 FOR UPDATE`, (*sql.Tx).Commit},
		{"select for share", `SELECT id FROM approvals WHERE id = $1 FOR SHARE`, (*sql.Tx).Commit},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("apr-wait-%d-%s", i, suffix)
			expires := time.Now().UTC().Add(700 * time.Millisecond)
			if err := st.CreateApproval(ctx, domain.Approval{ID: id, TaskID: taskID, WorkloadName: "triage-agent", Capability: "action:git.open_pr", PayloadCanonicalJSON: `{}`, PayloadSHA256: "d1", ExpiresAt: expires}); err != nil {
				t.Fatalf("CreateApproval: %v", err)
			}
			if err := st.DecideApproval(ctx, id, "d1", domain.ApprovalApproved, "operator", `{}`); err != nil {
				t.Fatalf("DecideApproval: %v", err)
			}

			holder, err := st.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			released := false
			defer func() {
				if !released {
					_ = holder.Rollback()
				}
			}()
			var holderPID int
			if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				t.Fatal(err)
			}
			rows, err := holder.QueryContext(ctx, tc.lock, id)
			if err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()

			done := make(chan error, 1)
			go func() { done <- st.ConsumeApproval(ctx, id, taskID, "action:git.open_pr", "d1") }()

			// Wait until a backend is blocked by the holder, and require that
			// to happen while the approval is still live.
			for {
				var blocked int
				if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, holderPID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked > 0 {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("use returned without waiting for the lock: %v", err)
				default:
				}
				if !time.Now().Before(expires.Add(-200 * time.Millisecond)) {
					t.Fatal("the use was not blocked by the holder well before the expiry")
				}
				time.Sleep(5 * time.Millisecond)
			}

			time.Sleep(time.Until(expires) + 200*time.Millisecond)
			select {
			case err := <-done:
				t.Fatalf("use returned while the holder kept the lock: %v", err)
			default:
			}
			released = true
			if err := tc.release(holder); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, store.ErrExpired) {
					t.Fatalf("wait spanning the expiry: err %v, want ErrExpired", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("use still blocked after the holder released the lock")
			}
			if got, _ := st.GetApproval(ctx, id); got.Status != domain.ApprovalApproved {
				t.Fatalf("status %q, want approved", got.Status)
			}
		})
	}
}

// openFreshPostgres creates an empty database on the server of dsn, so a test
// can build a schema of its own choosing.
func openFreshPostgres(t *testing.T, dsn string) *PostgresStore {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "sc_upgrade_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if a, err := sql.Open("pgx", dsn); err == nil {
			_, _ = a.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
			_ = a.Close()
		}
	})
	cfg, err := withDatabase(dsn, name)
	if err != nil {
		// The parse error may quote the DSN, so it is not printed.
		t.Fatal("SABLECOURT_TEST_POSTGRES_DSN cannot be parsed as a pgx connection string")
	}
	st := &PostgresStore{db: stdlib.OpenDB(*cfg)}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// withDatabase parses dsn as pgx does (URL or keyword/value) and returns its
// configuration with the database set to name. Working on the parsed
// configuration, not the text, means a dbname or database parameter cannot
// override the choice and no other value is re-encoded.
func withDatabase(dsn, name string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.Database = name
	return cfg, nil
}

func TestWithDatabaseSelectsTheNewDatabase(t *testing.T) {
	cases := []struct{ dsn, password string }{
		{"postgres://localhost/postgres", ""},
		{"postgres://localhost/postgres?dbname=sablecourt_test", ""},
		{"postgresql://user@localhost:5432/postgres?sslmode=disable&database=sablecourt_test", ""},
		{"postgres://localhost?dbname=sablecourt_test", ""},
		{"host=localhost dbname=sablecourt_test", ""},
		{"postgres://user@localhost/postgres?password=a%20b+c", "a b+c"},
	}
	for _, tc := range cases {
		cfg, err := withDatabase(tc.dsn, "sc_upgrade_x")
		if err != nil {
			t.Fatalf("withDatabase(%q): %v", tc.dsn, err)
		}
		if cfg.Database != "sc_upgrade_x" {
			t.Errorf("withDatabase(%q) selects database %q, want sc_upgrade_x", tc.dsn, cfg.Database)
		}
		if cfg.Password != tc.password {
			t.Errorf("withDatabase(%q) password %q, want %q", tc.dsn, cfg.Password, tc.password)
		}
	}
}

// An approval the old code left approved after using it cannot be used once
// more after the upgrade, and the migration runs once: an approval granted
// after the upgrade stays usable.
func TestLivePostgresUpgradeExpiresLegacyApprovalsIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	ctx := context.Background()
	st := openFreshPostgres(t, dsn)
	const previous = "0010_budget_reservations.sql"
	if _, err := st.db.ExecContext(ctx, `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() > previous {
			continue
		}
		raw, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, string(raw)); err != nil {
			t.Fatalf("apply %s: %v", e.Name(), err)
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`, e.Name(), nowRFC3339()); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"})
	if err := st.CreateTask(ctx, domain.Task{ID: "task-1", IdempotencyKey: "task-1", WorkloadName: "triage-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(10 * time.Minute)
	seed := func(id string, status domain.ApprovalStatus) {
		t.Helper()
		if err := st.CreateApproval(ctx, domain.Approval{ID: id, TaskID: "task-1", WorkloadName: "triage-agent", Capability: "action:git.open_pr", PayloadCanonicalJSON: `{}`, PayloadSHA256: "d1", ExpiresAt: future}); err != nil {
			t.Fatal(err)
		}
		if status != domain.ApprovalPending {
			if err := st.DecideApproval(ctx, id, "d1", status, "operator", `{}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("apr-used", domain.ApprovalApproved)
	seed("apr-pending", domain.ApprovalPending)
	seed("apr-denied", domain.ApprovalDenied)

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for id, want := range map[string]domain.ApprovalStatus{"apr-used": domain.ApprovalExpired, "apr-pending": domain.ApprovalPending, "apr-denied": domain.ApprovalDenied} {
		if got, _ := st.GetApproval(ctx, id); got.Status != want {
			t.Fatalf("%s: status %q after upgrade, want %q", id, got.Status, want)
		}
	}
	if err := st.ConsumeApproval(ctx, "apr-used", "task-1", "action:git.open_pr", "d1"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("replay after upgrade: err %v, want ErrConflict", err)
	}

	seed("apr-after-upgrade", domain.ApprovalApproved)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeApproval(ctx, "apr-after-upgrade", "task-1", "action:git.open_pr", "d1"); err != nil {
		t.Fatalf("approval granted after the upgrade: %v", err)
	}
}
