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
		// A refusal changes nothing: an expired row stays approved, and the
		// expiry clause keeps refusing it.
		{"expired", domain.ApprovalApproved, now.Add(-time.Second), "task-1", "action:git.open_pr", "d1", store.ErrExpired, domain.ApprovalApproved},
		{"expires exactly now", domain.ApprovalApproved, now, "task-1", "action:git.open_pr", "d1", store.ErrExpired, domain.ApprovalApproved},
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
			for i := range 2 { // a refusal leaves the row able to refuse again
				err := st.ConsumeApproval(ctx, "apr-c", tc.task, tc.cap, tc.digest)
				if i == 1 && tc.wantErr == nil {
					tc.wantErr = store.ErrConflict // the second use of a spent approval
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("use %d: err %v, want %v", i+1, err, tc.wantErr)
				}
				got, err := st.GetApproval(ctx, "apr-c")
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != tc.wantStatus {
					t.Fatalf("use %d: status %q, want %q", i+1, got.Status, tc.wantStatus)
				}
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		st := openTestStore(t)
		if err := st.ConsumeApproval(ctx, "nope", "task-1", "action:git.open_pr", "d1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err %v, want ErrNotFound", err)
		}
	})

	t.Run("second use and concurrent use", func(t *testing.T) {
		st := openTestStore(t)
		seedApproval(t, st, "apr-c", now.Add(time.Minute), domain.ApprovalApproved)
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := st.ConsumeApproval(ctx, "apr-c", "task-1", "action:git.open_pr", "d1"); err == nil {
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
		if err := st.ConsumeApproval(ctx, "apr-c", "task-1", "action:git.open_pr", "d1"); !errors.Is(err, store.ErrConflict) {
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

// The expiry is compared as a time, not as text: RFC 3339 text drops trailing
// zeros of the fraction, so "...:05Z" sorts after "...:05.5Z" although it is
// earlier. The database clock cannot be set, so each case opens its store
// first, then waits until the current second is at least 300 ms old and seeds
// its expiry around that second, so setup time cannot move the clock past it.
func TestConsumeApprovalComparesExpiryAsTime(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		expires func(whole time.Time) time.Time
		wantErr error
	}{
		{"expiry on the whole second just past", func(whole time.Time) time.Time { return whole }, store.ErrExpired},
		{"expiry with a fraction in a later second", func(whole time.Time) time.Time { return whole.Add(10*time.Second + 500*time.Millisecond) }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			now := time.Now().UTC()
			if frac := now.Sub(now.Truncate(time.Second)); frac < 300*time.Millisecond || frac > 700*time.Millisecond {
				time.Sleep(time.Second - frac + 400*time.Millisecond)
				now = time.Now().UTC()
			}
			seedApproval(t, st, "apr-t", tc.expires(now.Truncate(time.Second)), domain.ApprovalApproved)
			if err := st.ConsumeApproval(ctx, "apr-t", "task-1", "action:git.open_pr", "d1"); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// The expiry is compared with SQLite's clock inside the statement, after the
// statement has taken the write lock. A wait for a lock held by another
// connection that spans the expiry refuses the approval: no time read before
// the wait is used. The test requires the use to start well before the expiry
// and to be still blocked after it, so the refusal cannot come from a use
// that started late.
func TestConsumeApprovalWaitingForTheLockSpansTheExpiry(t *testing.T) {
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
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	other.SetMaxOpenConns(1)
	expires := time.Now().UTC().Add(600 * time.Millisecond)
	seedApproval(t, st, "apr-w", expires, domain.ApprovalApproved)
	if _, err := other.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	started := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() {
		started <- time.Now()
		done <- st.ConsumeApproval(ctx, "apr-w", "task-1", "action:git.open_pr", "d1")
	}()
	if at := <-started; !at.Before(expires.Add(-300 * time.Millisecond)) {
		t.Fatalf("the use started %v before the expiry, want at least 300ms", expires.Sub(at))
	}
	time.Sleep(time.Until(expires) + 200*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("use returned while the other connection held the write lock: %v", err)
	default:
	}
	if _, err := other.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrExpired) {
			t.Fatalf("err %v, want ErrExpired", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("use still blocked after the other connection released the lock")
	}
	if got, _ := st.GetApproval(ctx, "apr-w"); got.Status != domain.ApprovalApproved {
		t.Fatalf("status %q, want approved", got.Status)
	}
}
