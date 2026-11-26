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
			err := st.ConsumeApproval(ctx, "apr-c", tc.task, tc.cap, tc.digest, fixed(now))
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
		if err := st.ConsumeApproval(ctx, "nope", "task-1", "action:git.open_pr", "d1", fixed(now)); !errors.Is(err, store.ErrNotFound) {
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
				if err := st.ConsumeApproval(ctx, "apr-c", "task-1", "action:git.open_pr", "d1", fixed(now)); err == nil {
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
		if err := st.ConsumeApproval(ctx, "apr-c", "task-1", "action:git.open_pr", "d1", fixed(now)); !errors.Is(err, store.ErrConflict) {
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

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

// The expiry is compared as a time, not as text: RFC 3339 text drops trailing
// zeros of the fraction, so "...:05Z" sorts after "...:05.5Z" although it is
// earlier.
func TestConsumeApprovalComparesExpiryAsTime(t *testing.T) {
	ctx := context.Background()
	whole := time.Date(2026, 11, 26, 12, 0, 5, 0, time.UTC)
	cases := []struct {
		name    string
		expires time.Time
		now     time.Time
		wantErr error
	}{
		{"expiry on a whole second, use half a second later", whole, whole.Add(500 * time.Millisecond), store.ErrExpired},
		{"expiry half a second past, use on the whole second", whole.Add(500 * time.Millisecond), whole, nil},
		{"use one nanosecond after the expiry", whole.Add(time.Millisecond), whole.Add(time.Millisecond + time.Nanosecond), store.ErrExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			seedApproval(t, st, "apr-t", tc.expires, domain.ApprovalApproved)
			if err := st.ConsumeApproval(ctx, "apr-t", "task-1", "action:git.open_pr", "d1", fixed(tc.now)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// The clock is read only once the store's connection is held, so a wait for
// another call of this process cannot carry a time from before the expiry
// past it.
func TestConsumeApprovalReadsClockAfterTakingTheConnection(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	start := time.Now().UTC()
	seedApproval(t, st, "apr-w", start.Add(time.Minute), domain.ApprovalApproved)

	held, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clock := start
	read := false
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		read = true
		return clock
	}
	done := make(chan error, 1)
	go func() { done <- st.ConsumeApproval(ctx, "apr-w", "task-1", "action:git.open_pr", "d1", now) }()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if read {
		mu.Unlock()
		t.Fatal("clock read while the connection was still held elsewhere")
	}
	// The wait spans the expiry.
	clock = start.Add(2 * time.Minute)
	mu.Unlock()
	_ = held.Close()
	if err := <-done; !errors.Is(err, store.ErrExpired) {
		t.Fatalf("err %v, want ErrExpired", err)
	}
}
