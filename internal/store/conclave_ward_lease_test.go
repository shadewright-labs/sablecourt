package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// newConclaveRigAt is newConclaveRig with the database path returned, so a
// test can install triggers through a second connection.
func newConclaveRigAt(t *testing.T) (*conclaveRig, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conclave.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox}); err != nil {
		t.Fatal(err)
	}
	return &conclaveRig{st: st, now: time.Now().UTC()}, path
}

// runningTurn reserves a turn in room for role and gives it a live lease, as
// a launched turn has. It returns the lease token hash.
func (r *conclaveRig) runningTurn(t *testing.T, room, role string) string {
	t.Helper()
	task := turnTask(t, room, role, 0)
	if err := r.submit(t, room, task, r.limits()); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimConclaveTask(t.Context(), r.st, task.ID, room, func() time.Time { return r.now }); err != nil {
		t.Fatal(err)
	}
	return r.lease(t, task.ID)
}

func (r *conclaveRig) lease(t *testing.T, taskID string) string {
	t.Helper()
	hash := "lease-" + taskID + "-" + time.Now().Format(time.RFC3339Nano)
	if err := r.st.CreateLease(t.Context(), domain.Lease{
		TokenSHA256: hash, TaskID: taskID, WorkloadName: "worker",
		ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return hash
}

func leaseLive(t *testing.T, st store.Store, hash string) bool {
	t.Helper()
	_, err := st.GetActiveLeaseByTokenHash(t.Context(), hash, time.Now().UTC())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// A Ward trip that refuses further Vassal messages revokes the leases of the
// room's running turns in the transaction that pauses the room, so every gate
// that looks a lease up refuses the turn's token at once.
func TestWardTripRevokesTurnLeases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits func(store.ConclaveLimits) store.ConclaveLimits
		posts  []string
		reason string
	}{
		{"max messages", func(l store.ConclaveLimits) store.ConclaveLimits { l.MaxMessages = 2; return l }, []string{"a", "b"}, "max_messages_reached(2)"},
		{"ping pong", func(l store.ConclaveLimits) store.ConclaveLimits { l.MaxPingPong = 2; return l }, []string{"a", "b"}, "ping_pong_detected(2)"},
		{"budget evidence missing", func(l store.ConclaveLimits) store.ConclaveLimits { l.BudgetMissing = true; return l }, []string{"a"}, "budget_evidence_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newConclaveRig(t)
			r.room(t, "room", domain.ConclaveStateActive, 0)
			r.room(t, "other", domain.ConclaveStateActive, 0)
			first := r.runningTurn(t, "room", "a")
			second := r.runningTurn(t, "room", "b")
			elsewhere := r.runningTurn(t, "other", "a")
			limits := tc.limits(r.limits())
			var reason string
			for _, from := range tc.posts {
				var err error
				_, reason, _, err = r.post(t, "room", from, limits, false, false)
				if err != nil && !errors.Is(err, store.ErrWardTripped) {
					t.Fatalf("post from %s: %v", from, err)
				}
			}
			if reason != tc.reason {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
			if leaseLive(t, r.st, first) || leaseLive(t, r.st, second) {
				t.Fatal("a running turn kept a live lease after the Ward tripped")
			}
			if !leaseLive(t, r.st, elsewhere) {
				t.Fatal("a turn in another room lost its lease")
			}
		})
	}

	t.Run("a trip at turn reservation revokes too", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		running := r.runningTurn(t, "room", "a")
		limits := r.limits()
		limits.MaxMessages = 1
		// Seed one message under a high ceiling, then reserve at the ceiling.
		if _, _, _, err := r.post(t, "room", "a", r.limits(), false, false); err != nil {
			t.Fatal(err)
		}
		if err := r.submit(t, "room", turnTask(t, "room", "b", 0), limits); !errors.Is(err, store.ErrConclavePaused) {
			t.Fatalf("turn at the ceiling = %v", err)
		}
		if c := roomState(t, r.st, "room"); c.WardReason != "max_messages_reached(1)" {
			t.Fatalf("room = %+v", c)
		}
		if leaseLive(t, r.st, running) {
			t.Fatal("running turn kept its lease after the reservation trip")
		}
	})

	t.Run("a run ceiling admits in-flight replies and keeps their leases", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		running := r.runningTurn(t, "room", "a")
		limits := r.limits()
		limits.MaxRuns = 1
		if err := r.submit(t, "room", turnTask(t, "room", "b", 0), limits); !errors.Is(err, store.ErrConclaveRunLimit) {
			t.Fatalf("turn past the run limit = %v", err)
		}
		if !leaseLive(t, r.st, running) {
			t.Fatal("max_runs revoked a lease whose reply the room still admits")
		}
	})

	t.Run("a revocation that cannot be written is not a clean trip", func(t *testing.T) {
		r, path := newConclaveRigAt(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		running := r.runningTurn(t, "room", "a")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER fail_lease_revoke BEFORE UPDATE OF revoked_at ON leases
			BEGIN SELECT RAISE(ABORT, 'lease store unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		limits := r.limits()
		limits.MaxMessages = 1
		_, reason, _, err := r.post(t, "room", "a", limits, false, false)
		if err == nil || reason != "" || !strings.Contains(err.Error(), "lease store unavailable") {
			t.Fatalf("trip with a failed revocation = %q, %v", reason, err)
		}
		if c := roomState(t, r.st, "room"); c.State != domain.ConclaveStateActive || c.WardReason != "" {
			t.Fatalf("room recorded a trip whose revocation failed: %+v", c)
		}
		if got := countRoomAudits(t, r.st, "conclave.ward", "room"); len(got) != 0 {
			t.Fatalf("ward audit written for a failed revocation: %+v", got)
		}
		msgs, err := r.st.ListConclaveMessages(t.Context(), "room", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("messages stored by a rolled back trip: %+v", msgs)
		}
		if !leaseLive(t, r.st, running) {
			t.Fatal("lease state changed by a rolled back trip")
		}
	})
}

// Operator messages do not restart the maxMessages count, and a Decree cannot
// lift the ceiling once it trips.
func TestOperatorMessagesDoNotResetMaxMessages(t *testing.T) {
	r := newConclaveRig(t)
	r.room(t, "room", domain.ConclaveStateActive, 0)
	limits := r.limits()
	limits.MaxMessages = 3
	for _, from := range []string{"a", "b", "operator"} {
		if _, reason, _, err := r.post(t, "room", from, limits, false, false); err != nil || reason != "" {
			t.Fatalf("post from %s = %q, %v", from, reason, err)
		}
	}
	_, reason, _, err := r.post(t, "room", "c", limits, false, false)
	if err != nil || reason != "max_messages_reached(3)" {
		t.Fatalf("third Vassal message after an operator message = %q, %v", reason, err)
	}
	// A Decree sent after the trip cannot resume the room or restart the count.
	if _, _, _, err := r.post(t, "room", "operator", limits, true, false); !errors.Is(err, store.ErrConclavePaused) || !errors.Is(err, store.ErrWardTripped) {
		t.Fatalf("resume past maxMessages = %v", err)
	}
	if c := roomState(t, r.st, "room"); c.State != domain.ConclaveStateInputRequired || c.WardReason != "max_messages_reached(3)" {
		t.Fatalf("room after a refused resume = %+v", c)
	}
	if _, _, _, err := r.post(t, "room", "a", limits, false, false); !errors.Is(err, store.ErrConclavePaused) {
		t.Fatalf("Vassal post after the refused resume = %v", err)
	}
	if err := r.submit(t, "room", turnTask(t, "room", "d", 0), limits); !errors.Is(err, store.ErrConclavePaused) {
		t.Fatalf("turn after the refused resume = %v", err)
	}

	t.Run("a turn reservation counts messages before the last operator message", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		limits := r.limits()
		limits.MaxMessages = 2
		for _, from := range []string{"a", "b"} {
			if _, _, _, err := r.post(t, "room", from, r.limits(), false, false); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, _, err := r.post(t, "room", "operator", r.limits(), false, false); err != nil {
			t.Fatal(err)
		}
		if err := r.submit(t, "room", turnTask(t, "room", "c", 0), limits); !errors.Is(err, store.ErrConclavePaused) {
			t.Fatalf("turn past maxMessages after an operator message = %v", err)
		}
	})

	t.Run("an operator message still restarts the ping pong window", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		limits := r.limits()
		limits.MaxPingPong = 3
		for _, from := range []string{"a", "b", "operator", "a"} {
			if _, reason, _, err := r.post(t, "room", from, limits, false, false); err != nil || reason != "" {
				t.Fatalf("post from %s = %q, %v", from, reason, err)
			}
		}
	})
}

// testWardTripRevokesLeases runs on both stores: a maxMessages trip revokes the
// live lease of a running turn, and a run-ceiling trip keeps it.
func testWardTripRevokesLeases(t *testing.T, st store.Store) {
	ctx := t.Context()
	now := time.Now().UTC()
	limits := store.ConclaveLimits{MaxMessages: 1, MaxPingPong: 10, MaxRuns: 1, MaxAge: store.ConclaveWallTime, Now: func() time.Time { return now }}
	for _, tc := range []struct {
		room string
		trip func() error
		live bool
	}{
		{"lease-messages", func() error {
			_, _, _, err := store.AppendConclavePost(ctx, st, domain.ConclaveMessage{
				ConclaveID: "lease-messages", FromRole: "a", ToRole: "all", Kind: "proposal", Body: "hi", CreatedAt: now,
			}, limits, false, false)
			return err
		}, false},
		{"lease-runs", func() error {
			task := turnTask(t, "lease-runs", "b", 0)
			_, err := store.SubmitConclaveTurn(ctx, "lease-runs", task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
				return task, st.CreateTask(ctx, task)
			})
			if !errors.Is(err, store.ErrConclaveRunLimit) {
				return err
			}
			return nil
		}, true},
	} {
		if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: tc.room, State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		task := turnTask(t, tc.room, "a", 0)
		if _, err := store.SubmitConclaveTurn(ctx, tc.room, task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
			return task, st.CreateTask(ctx, task)
		}); err != nil {
			t.Fatal(err)
		}
		hash := "lease-" + tc.room
		if err := st.CreateLease(ctx, domain.Lease{TokenSHA256: hash, TaskID: task.ID, WorkloadName: "worker",
			ExpiresAt: now.Add(time.Hour), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := tc.trip(); err != nil {
			t.Fatalf("%s trip: %v", tc.room, err)
		}
		if got := roomState(t, st, tc.room); got.State != domain.ConclaveStateInputRequired {
			t.Fatalf("%s did not trip: %+v", tc.room, got)
		}
		if live := leaseLive(t, st, hash); live != tc.live {
			t.Fatalf("%s lease live = %v, want %v", tc.room, live, tc.live)
		}
	}
}
