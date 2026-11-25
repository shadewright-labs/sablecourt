package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestConsumeApproval(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cases := []struct {
		name       string
		status     domain.ApprovalStatus
		expiresAt  time.Time
		task, cap  string
		digest     string
		wantErr    error
		wantStatus domain.ApprovalStatus
	}{
		{"approved and matching", domain.ApprovalApproved, now.Add(time.Minute), "task-1", "action:git.open_pr", "d1", nil, domain.ApprovalConsumed},
		{"expired", domain.ApprovalApproved, now.Add(-time.Second), "task-1", "action:git.open_pr", "d1", store.ErrExpired, domain.ApprovalExpired},
		{"expires exactly now", domain.ApprovalApproved, now, "task-1", "action:git.open_pr", "d1", store.ErrExpired, domain.ApprovalExpired},
		{"pending", domain.ApprovalPending, now.Add(time.Minute), "task-1", "action:git.open_pr", "d1", store.ErrConflict, domain.ApprovalPending},
		{"denied", domain.ApprovalDenied, now.Add(time.Minute), "task-1", "action:git.open_pr", "d1", store.ErrConflict, domain.ApprovalDenied},
		{"other task", domain.ApprovalApproved, now.Add(time.Minute), "task-2", "action:git.open_pr", "d1", store.ErrConflict, domain.ApprovalApproved},
		{"other capability", domain.ApprovalApproved, now.Add(time.Minute), "task-1", "action:git.create_branch", "d1", store.ErrConflict, domain.ApprovalApproved},
		{"other digest", domain.ApprovalApproved, now.Add(time.Minute), "task-1", "action:git.open_pr", "d2", store.ErrConflict, domain.ApprovalApproved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			seedApproval(t, st, "apr-c", tc.expiresAt, tc.status)
			err := st.ConsumeApproval(ctx, store.ConsumeRequest{ID: "apr-c", TaskID: tc.task, Capability: tc.cap, PayloadSHA256: tc.digest, Now: func() time.Time { return now }, Audit: useAudit})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err %v, want %v", err, tc.wantErr)
			}
			got, err := st.GetApproval(ctx, "apr-c")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status %q, want %q", got.Status, tc.wantStatus)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		st := openTestStore(t)
		if err := st.ConsumeApproval(ctx, store.ConsumeRequest{ID: "nope", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: func() time.Time { return now }, Audit: useAudit}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err %v, want ErrNotFound", err)
		}
	})

	t.Run("second use and concurrent use", func(t *testing.T) {
		st := openTestStore(t)
		seedApproval(t, st, "apr-c", now.Add(time.Minute), domain.ApprovalApproved)
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := st.ConsumeApproval(ctx, store.ConsumeRequest{ID: "apr-c", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: func() time.Time { return now }, Audit: useAudit}); err == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if ok != 1 {
			t.Fatalf("%d uses succeeded, want exactly 1", ok)
		}
		if err := st.ConsumeApproval(ctx, store.ConsumeRequest{ID: "apr-c", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: func() time.Time { return now }, Audit: useAudit}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("later use: err %v, want ErrConflict", err)
		}
	})
}

func seedApproval(t *testing.T, st *SQLiteStore, id string, expiresAt time.Time, status domain.ApprovalStatus) {
	t.Helper()
	ctx := context.Background()
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"})
	_ = st.CreateTask(ctx, domain.Task{ID: "task-1", IdempotencyKey: "k-" + id, WorkloadName: "triage-agent", State: domain.TaskStateWorking})
	if err := st.CreateApproval(ctx, domain.Approval{
		ID: id, TaskID: "task-1", WorkloadName: "triage-agent", Capability: "action:git.open_pr",
		PayloadCanonicalJSON: `{}`, PayloadSHA256: "d1", ExpiresAt: expiresAt,
	}); err != nil {
		t.Fatal(err)
	}
	if status != domain.ApprovalPending {
		if err := st.DecideApproval(ctx, id, "d1", status, "operator", `{}`); err != nil {
			t.Fatal(err)
		}
	}
}

// M2: a use that waits for the store while the approval expires is refused,
// because the time is read after the transaction holds the row.
func TestConsumeApprovalChecksExpiryAfterTheWait(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	start := time.Now().UTC()
	seedApproval(t, st, "apr-wait", start.Add(time.Minute), domain.ApprovalApproved)

	var mu sync.Mutex
	clock := start
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	hold, err := st.db.BeginTx(ctx, nil) // takes the store's only connection
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- st.ConsumeApproval(ctx, store.ConsumeRequest{ID: "apr-wait", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: now, Audit: useAudit})
	}()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	clock = start.Add(2 * time.Minute)
	mu.Unlock()
	_ = hold.Rollback()
	if err := <-done; !errors.Is(err, store.ErrExpired) {
		t.Fatalf("use that waited past expiry: err %v, want ErrExpired", err)
	}
}

// M3: the consumption and its audit row commit or roll back together.
func TestConsumeApprovalAuditIsInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedApproval(t, st, "apr-audit", time.Now().UTC().Add(time.Minute), domain.ApprovalApproved)
	req := store.ConsumeRequest{
		ID: "apr-audit", TaskID: "task-1", Capability: "action:git.open_pr", PayloadSHA256: "d1",
		Now:   func() time.Time { return time.Now().UTC() },
		Audit: domain.AuditRecord{Category: "approval", TaskID: "task-1", Action: "action:git.open_pr", Outcome: "consumed", DetailJSON: `{"approval_id":"apr-audit"}`},
	}
	if _, err := st.db.ExecContext(ctx, `CREATE TRIGGER fail_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	err := st.ConsumeApproval(ctx, req)
	if err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrExpired) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed audit: err %v, want a plain error", err)
	}
	if got, _ := st.GetApproval(ctx, "apr-audit"); got.Status != domain.ApprovalApproved {
		t.Fatalf("status %q after a failed audit, want approved", got.Status)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TRIGGER fail_audit`); err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeApproval(ctx, req); err != nil {
		t.Fatalf("consume: %v", err)
	}
	recs, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 || recs[0].Outcome != "consumed" || recs[0].CreatedAt.IsZero() {
		t.Fatalf("consumption audit row missing: %+v", recs)
	}
	if n, err := st.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("audit chain broken after %d rows: %v", n, err)
	}
}
