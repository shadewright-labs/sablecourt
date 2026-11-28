package controlplane

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// failCreateOnce fails the first CreateTask after armed is set, after the
// audit rows before it were written.
type failCreateOnce struct {
	store.Store
	armed bool
	// cancel, when set, cancels the caller's context and the create fails
	// with its error, as when a client goes away mid-request.
	cancel context.CancelFunc
}

func (s *failCreateOnce) CreateTask(ctx context.Context, task domain.Task) error {
	if s.armed {
		s.armed = false
		if s.cancel != nil {
			s.cancel()
			return ctx.Err()
		}
		return errors.New("injected create failure")
	}
	return s.Store.CreateTask(ctx, task)
}

// A retry whose successor is not created closes its task.retry row with a
// not_created row of the same action, so a reader of task.retry rows never
// finds one naming a task that does not exist (issue 179).
func TestRetryAuditRowClosedWhenSuccessorNotCreated(t *testing.T) {
	ctx := context.Background()
	base, st, _, task := regressionFixture(t)
	failTask(t, st, task.ID, domain.TaskStateSubmitted)
	wrapped := &failCreateOnce{Store: st, armed: true}
	r := NewReconciler(Options{Store: wrapped, Bundle: base.bundle})
	if _, err := r.RetryTaskWithHandoff(WithRetryAudit(ctx), task.ID, false, ""); err == nil {
		t.Fatal("first retry succeeded, want the injected create failure")
	}
	retried, err := r.RetryTaskWithHandoff(WithRetryAudit(ctx), task.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	audit, err := st.ListAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	closed := map[string]bool{}
	for _, a := range audit {
		if a.Action == "task.retry" && a.Outcome == "not_created" {
			closed[a.TaskID] = true
		}
	}
	submitted := 0
	for _, a := range audit {
		if a.Action != "task.retry" || a.Outcome != "submitted" {
			continue
		}
		submitted++
		if _, err := st.GetTask(ctx, a.TaskID); err == nil {
			if a.TaskID != retried.ID {
				t.Fatalf("task.retry row names %s, retry returned %s", a.TaskID, retried.ID)
			}
			continue
		}
		if !closed[a.TaskID] {
			t.Fatalf("task.retry/submitted row names %s, which does not exist and has no task.retry/not_created row: %+v", a.TaskID, audit)
		}
	}
	if submitted != 2 {
		t.Fatalf("task.retry/submitted rows = %d, want 2: %+v", submitted, audit)
	}
}

// A submission whose context ends while its task is created still writes the
// not_created rows that close the rows written before it: the closing rows do
// not share the context of the step they close (issue 179).
func TestNotCreatedRowsSurviveCancelledRequest(t *testing.T) {
	cases := []struct {
		name    string
		actions []string
		act     func(ctx context.Context, r *Reconciler, task domain.Task) error
	}{
		{"submit", []string{"task.submit"}, func(ctx context.Context, r *Reconciler, _ domain.Task) error {
			_, err := r.SubmitTask(ctx, "worker", "second", "{}")
			return err
		}},
		{"retry", []string{"task.submit", "task.retry"}, func(ctx context.Context, r *Reconciler, task domain.Task) error {
			_, err := r.RetryTaskWithHandoff(WithRetryAudit(ctx), task.ID, false, "")
			return err
		}},
		{"delegation", []string{"delegate:worker"}, func(ctx context.Context, r *Reconciler, task domain.Task) error {
			_, err := r.ProposeChildTask(ctx, task.ID, "worker", "child-1", "{}", "")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, st, _, task := regressionFixture(t)
			if tc.name == "retry" {
				failTask(t, st, task.ID, domain.TaskStateSubmitted)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wrapped := &failCreateOnce{Store: st, armed: true, cancel: cancel}
			r := NewReconciler(Options{Store: wrapped, Bundle: base.bundle, Policy: base.policy})
			if err := tc.act(ctx, r, task); err == nil {
				t.Fatal("call succeeded, want the cancelled create to fail it")
			}
			audit, err := st.ListAudit(context.Background(), 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range tc.actions {
				opened, closed := 0, 0
				for _, a := range audit {
					if a.Action != action || a.TaskID == task.ID {
						continue
					}
					switch a.Outcome {
					case "not_created":
						closed++
					default:
						opened++
					}
				}
				if opened != 1 || closed != 1 {
					t.Fatalf("%s rows: %d opened, %d not_created, want 1 and 1: %+v", action, opened, closed, audit)
				}
			}
		})
	}
}

// stallClosingStore holds every not_created row until the write's context
// ends, and counts the rows it held.
type stallClosingStore struct {
	store.Store
	held atomic.Int32
}

func (s *stallClosingStore) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	if rec.Outcome != "not_created" {
		return s.Store.AppendAudit(ctx, rec)
	}
	s.held.Add(1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
		return errors.New("closing row held without a deadline")
	}
}

// A failed retry closes its task.submit and task.retry rows under one shared
// budget: when each closing write waits for its context, the retry waits
// about one budget, not one per row (issue 179).
func TestClosingRowsOfOneFailureShareOneBudget(t *testing.T) {
	saved := store.ClosingAuditBudget
	store.ClosingAuditBudget = 300 * time.Millisecond
	t.Cleanup(func() { store.ClosingAuditBudget = saved })
	base, st, _, task := regressionFixture(t)
	failTask(t, st, task.ID, domain.TaskStateSubmitted)
	stall := &stallClosingStore{Store: &failCreateOnce{Store: st, armed: true}}
	r := NewReconciler(Options{Store: stall, Bundle: base.bundle})
	start := time.Now()
	if _, err := r.RetryTaskWithHandoff(WithRetryAudit(context.Background()), task.ID, false, ""); err == nil {
		t.Fatal("retry succeeded, want the injected create failure")
	}
	elapsed := time.Since(start)
	if n := stall.held.Load(); n != 2 {
		t.Fatalf("closing rows written = %d, want 2", n)
	}
	if elapsed > store.ClosingAuditBudget*3/2 {
		t.Fatalf("retry waited %v on its closing rows, want about one budget of %v", elapsed, store.ClosingAuditBudget)
	}
}
