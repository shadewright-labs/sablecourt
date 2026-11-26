package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sablecourt.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestMigrateIdempotentAndWALMode(t *testing.T) {
	st := openTestStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate failed: %v", err)
	}

	var mode string
	if err := st.db.QueryRow(`PRAGMA journal_mode;`).Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected wal mode, got %q", mode)
	}
}

func TestWorkloadTaskApprovalBudgetAndAuditFlow(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name:           "triage-agent",
		Zone:           domain.ZoneRuntime,
		Class:          domain.ClassLongLivedAgent,
		ManifestDigest: "abc123",
		AgentCardJSON:  `{"description":"Triages issues"}`,
	})
	if err != nil {
		t.Fatalf("UpsertWorkload: %v", err)
	}

	workloads, err := st.ListWorkloads(ctx)
	if err != nil || len(workloads) != 1 {
		t.Fatalf("ListWorkloads: len=%d err=%v", len(workloads), err)
	}

	task := domain.Task{
		ID:             "task-1",
		IdempotencyKey: "idem-1",
		WorkloadName:   "triage-agent",
		State:          domain.TaskStateSubmitted,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := st.CreateTask(ctx, task); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate idempotency key, got %v", err)
	}

	if err := st.UpdateTaskState(ctx, "task-1", domain.TaskStateSubmitted, domain.TaskStateWorking, "dispatched", `{}`); err != nil {
		t.Fatalf("UpdateTaskState: %v", err)
	}
	if err := st.RecordHeartbeat(ctx, "task-1", time.Now().UTC()); err != nil {
		t.Fatalf("RecordHeartbeat: %v", err)
	}

	got, err := st.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != domain.TaskStateWorking || got.LastHeartbeatAt == nil {
		t.Fatalf("unexpected task state=%q hb=%v", got.State, got.LastHeartbeatAt)
	}

	err = st.CreateApproval(ctx, domain.Approval{
		ID:                   "apr-1",
		TaskID:               "task-1",
		WorkloadName:         "triage-agent",
		Capability:           "action:git.open_pr",
		PayloadCanonicalJSON: `{"branch":"sablecourt/task-1"}`,
		PayloadSHA256:        "deadbeef",
		ExpiresAt:            time.Now().UTC().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	if err := st.DecideApproval(ctx, "apr-1", "wrong-digest", domain.ApprovalApproved, "operator", `{}`); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on wrong digest, got %v", err)
	}
	if err := st.DecideApproval(ctx, "apr-1", "deadbeef", domain.ApprovalApproved, "operator", `{"method":"oob"}`); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}

	if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{
		TaskID:           "task-1",
		WorkloadName:     "triage-agent",
		Provider:         "gemini",
		Model:            "gemini-2.5-pro",
		InputTokens:      1200,
		CacheWriteTokens: 200,
		CacheReadTokens:  600,
		OutputTokens:     300,
		CostMicroUSD:     4500,
	}); err != nil {
		t.Fatalf("RecordBudgetUsage: %v", err)
	}

	totals, err := st.TaskBudgetTotals(ctx, "task-1")
	if err != nil {
		t.Fatalf("TaskBudgetTotals: %v", err)
	}
	if totals.InputTokens != 1200 || totals.CacheWriteTokens != 200 || totals.CacheReadTokens != 600 || totals.OutputTokens != 300 || totals.CostMicroUSD != 4500 {
		t.Fatalf("unexpected totals: %+v", totals)
	}
	allTotals, err := st.TotalBudgetTotals(ctx)
	if err != nil {
		t.Fatalf("TotalBudgetTotals: %v", err)
	}
	if allTotals != totals {
		t.Fatalf("TotalBudgetTotals = %+v, want %+v", allTotals, totals)
	}

	if err := st.AppendAudit(ctx, domain.AuditRecord{
		Category:     "policy",
		TaskID:       "task-1",
		WorkloadName: "triage-agent",
		Action:       "action:git.open_pr",
		Outcome:      "approved",
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	auditRows, err := st.ListAudit(ctx, 10)
	if err != nil || len(auditRows) != 1 {
		t.Fatalf("ListAudit: len=%d err=%v", len(auditRows), err)
	}

	now := time.Now().UTC()
	if err := st.CreateLease(ctx, domain.Lease{
		TokenSHA256:  "hash-1",
		TaskID:       "task-1",
		WorkloadName: "triage-agent",
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
	}); err != nil {
		t.Fatalf("CreateLease: %v", err)
	}
	if _, err := st.GetActiveLeaseByTokenHash(ctx, "hash-1", now); err != nil {
		t.Fatalf("GetActiveLeaseByTokenHash active: %v", err)
	}
	if _, err := st.GetActiveLeaseByTokenHash(ctx, "hash-1", now.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for expired lease, got %v", err)
	}
	if err := st.RevokeTaskLeases(ctx, "task-1"); err != nil {
		t.Fatalf("RevokeTaskLeases: %v", err)
	}
	if _, err := st.GetActiveLeaseByTokenHash(ctx, "hash-1", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for revoked lease, got %v", err)
	}

	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	daily, err := st.WorkloadDailyBudgetTotals(ctx, "triage-agent", dayStart)
	if err != nil || daily.CostMicroUSD != 4500 {
		t.Fatalf("WorkloadDailyBudgetTotals: %+v err=%v", daily, err)
	}
}

func TestMigratePendingRuntimeExits(t *testing.T) {
	ctx := t.Context()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	schema, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, string(schema)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id      string
		reasons []string
	}{
		{"processed", []string{"runtime_exit_observed", "runtime_completed"}},
		{"pending", []string{"runtime_exit_observed"}},
	} {
		if err := st.CreateTask(ctx, domain.Task{ID: tc.id, IdempotencyKey: tc.id, State: domain.TaskStateCompleted, WorkloadName: "worker"}); err != nil {
			t.Fatal(err)
		}
		for _, reason := range tc.reasons {
			if _, err := st.db.ExecContext(ctx, `INSERT INTO task_events(task_id, from_state, to_state, reason, created_at)
				VALUES (?, 'completed', 'completed', ?, ?)`, tc.id, reason, nowRFC3339()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ListReconcileTasks(ctx, nil)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "pending" {
		t.Fatalf("migrated candidates = %v, err = %v", tasks, err)
	}
}
