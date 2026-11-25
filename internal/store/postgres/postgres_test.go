package postgres

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if err := st.ConsumeApproval(ctx, store.ConsumeRequest{ID: live, TaskID: taskID, Capability: "action:git.open_pr", PayloadSHA256: "d2", Now: func() time.Time { return now }}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong digest: err %v, want ErrConflict", err)
	}
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st.ConsumeApproval(ctx, store.ConsumeRequest{ID: live, TaskID: taskID, Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: func() time.Time { return now }}) == nil {
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
	if err := st.ConsumeApproval(ctx, store.ConsumeRequest{ID: old, TaskID: taskID, Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: func() time.Time { return now }}); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("expired: err %v, want ErrExpired", err)
	}
	if got, _ := st.GetApproval(ctx, old); got.Status != domain.ApprovalExpired {
		t.Fatalf("status %q, want expired", got.Status)
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
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("SABLECOURT_TEST_POSTGRES_DSN must be a URL for this test: %v", err)
	}
	u.Path = "/" + name
	st, err := Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// M1: approvals left approved by the previous schema are expired by the upgrade.
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
		raw, _ := migrationFS.ReadFile("migrations/" + e.Name())
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
	for id, status := range map[string]domain.ApprovalStatus{"apr-used": domain.ApprovalApproved, "apr-pending": domain.ApprovalPending} {
		if err := st.CreateApproval(ctx, domain.Approval{ID: id, TaskID: "task-1", WorkloadName: "triage-agent", Capability: "action:git.open_pr", PayloadCanonicalJSON: `{}`, PayloadSHA256: "d1", ExpiresAt: future}); err != nil {
			t.Fatal(err)
		}
		if status != domain.ApprovalPending {
			if err := st.DecideApproval(ctx, id, "d1", status, "operator", `{}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if got, _ := st.GetApproval(ctx, "apr-used"); got.Status != domain.ApprovalExpired {
		t.Fatalf("legacy approved row: status %q, want expired", got.Status)
	}
	if got, _ := st.GetApproval(ctx, "apr-pending"); got.Status != domain.ApprovalPending {
		t.Fatalf("pending row: status %q, want pending", got.Status)
	}
	err = st.ConsumeApproval(ctx, store.ConsumeRequest{ID: "apr-used", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: func() time.Time { return time.Now().UTC() }})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("replay after upgrade: err %v, want ErrConflict", err)
	}
}

// M3: on Postgres too, a failed audit insert rolls the consumption back.
func TestLivePostgresConsumeAuditInOneTransactionIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	ctx := context.Background()
	st := openFreshPostgres(t, dsn)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"})
	if err := st.CreateTask(ctx, domain.Task{ID: "task-1", IdempotencyKey: "task-1", WorkloadName: "triage-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateApproval(ctx, domain.Approval{ID: "apr-a", TaskID: "task-1", WorkloadName: "triage-agent", Capability: "action:git.open_pr", PayloadCanonicalJSON: `{}`, PayloadSHA256: "d1", ExpiresAt: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideApproval(ctx, "apr-a", "d1", domain.ApprovalApproved, "operator", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `
		CREATE FUNCTION fail_audit() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER fail_audit BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION fail_audit();`); err != nil {
		t.Fatal(err)
	}
	req := store.ConsumeRequest{
		ID: "apr-a", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1",
		Now:   func() time.Time { return time.Now().UTC() },
		Audit: domain.AuditRecord{Category: "approval", TaskID: "task-1", Action: "action:git.open_pr", Outcome: "consumed"},
	}
	if err := st.ConsumeApproval(ctx, req); err == nil || errors.Is(err, store.ErrConflict) {
		t.Fatalf("failed audit: err %v, want a plain error", err)
	}
	if got, _ := st.GetApproval(ctx, "apr-a"); got.Status != domain.ApprovalApproved {
		t.Fatalf("status %q after a failed audit, want approved", got.Status)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TRIGGER fail_audit ON audit_log`); err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeApproval(ctx, req); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got, _ := st.GetApproval(ctx, "apr-a"); got.Status != domain.ApprovalConsumed {
		t.Fatalf("status %q, want consumed", got.Status)
	}
}
