package postgres

import (
	"context"
	"errors"
	"io/fs"
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
	if err := st.ConsumeApproval(ctx, live, taskID, "action:git.open_pr", "d2", fixed(now)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong digest: err %v, want ErrConflict", err)
	}
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st.ConsumeApproval(ctx, live, taskID, "action:git.open_pr", "d1", fixed(now)) == nil {
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
	if err := st.ConsumeApproval(ctx, old, taskID, "action:git.open_pr", "d1", fixed(now)); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("expired: err %v, want ErrExpired", err)
	}
	if got, _ := st.GetApproval(ctx, old); got.Status != domain.ApprovalExpired {
		t.Fatalf("status %q, want expired", got.Status)
	}

	// The expiry is compared as a time, not as text: "...:05Z" sorts after
	// "...:05.5Z" as text although it is earlier.
	whole := time.Date(2026, 11, 26, 12, 0, 5, 0, time.UTC)
	textTrap := "apr-text-" + suffix
	seed(textTrap, whole)
	if err := st.ConsumeApproval(ctx, textTrap, taskID, "action:git.open_pr", "d1", fixed(whole.Add(500*time.Millisecond))); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("use after a whole-second expiry: err %v, want ErrExpired", err)
	}
	fraction := "apr-frac-" + suffix
	seed(fraction, whole.Add(500*time.Millisecond))
	if err := st.ConsumeApproval(ctx, fraction, taskID, "action:git.open_pr", "d1", fixed(whole)); err != nil {
		t.Fatalf("use before a fractional expiry: %v", err)
	}
}

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }
