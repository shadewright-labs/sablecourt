package controlplane

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// The control plane's audit writes by class (issue 107 row 3). Submitting,
// delegating and resuming start work, so each is recorded first and refused
// when the record cannot be written. Cancelling and suspending stop work, so
// they go ahead and return the failed record. A background record is logged
// and the work it describes continues.
func TestControlPlaneAuditWriteFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// prepare runs with audit writes succeeding.
		prepare func(t *testing.T, r *Reconciler, task domain.Task)
		fail    func(domain.AuditRecord) bool
		act     func(ctx context.Context, r *Reconciler, task domain.Task) error
		wantErr bool
		// wantEffects are the effects the call records before the failed
		// write, with {id} for the task ID; the answer of
		// store.AuditFailureText lists exactly these.
		wantEffects []string
		check       func(t *testing.T, st store.Store, task domain.Task)
	}{
		{
			name: "submit is refused and creates no task",
			fail: audittest.Action("task.submit"),
			act: func(ctx context.Context, r *Reconciler, _ domain.Task) error {
				_, err := r.SubmitTask(ctx, "worker", "second", "{}")
				return err
			},
			wantErr: true,
			check: func(t *testing.T, st store.Store, _ domain.Task) {
				if _, err := st.GetTaskByIdempotencyKey(ctx, "second"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("task created without its record: %v", err)
				}
			},
		},
		{
			name: "an idempotent replay writes nothing and still returns the task",
			fail: audittest.Action("task.submit"),
			act: func(ctx context.Context, r *Reconciler, _ domain.Task) error {
				_, err := r.SubmitTask(ctx, "worker", "original", "{}")
				return err
			},
		},
		{
			name: "delegation is refused and creates no child",
			fail: audittest.Action("delegate:worker"),
			act: func(ctx context.Context, r *Reconciler, task domain.Task) error {
				_, err := r.ProposeChildTask(ctx, task.ID, "worker", "child-1", "{}", "")
				return err
			},
			wantErr: true,
			check: func(t *testing.T, st store.Store, _ domain.Task) {
				if _, err := st.GetTaskByIdempotencyKey(ctx, "child-1"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("child created without its record: %v", err)
				}
			},
		},
		{
			name: "resume is refused and the task stays suspended",
			prepare: func(t *testing.T, r *Reconciler, task domain.Task) {
				launchAndSuspend(t, r, task)
			},
			fail:    audittest.Action("task.resume"),
			act:     func(ctx context.Context, r *Reconciler, task domain.Task) error { return r.ResumeTask(ctx, task.ID) },
			wantErr: true,
			check: func(t *testing.T, st store.Store, task domain.Task) {
				requireState(t, st, task.ID, domain.TaskStateSuspended)
			},
		},
		{
			name: "cancel goes ahead and reports the record",
			fail: audittest.Action("task.cancel"),
			act: func(ctx context.Context, r *Reconciler, task domain.Task) error {
				return r.CancelTask(ctx, task.ID, "")
			},
			wantErr:     true,
			wantEffects: []string{"task {id} moved from submitted to cancelled", "the leases of task {id} were revoked"},
			check: func(t *testing.T, st store.Store, task domain.Task) {
				requireState(t, st, task.ID, domain.TaskStateCancelled)
			},
		},
		{
			name: "suspend goes ahead and reports the record",
			prepare: func(t *testing.T, r *Reconciler, task domain.Task) {
				if err := launchForTest(ctx, r, task, r.bundle.Workloads["worker"], r.adapters[domain.ClassDeterministicController]); err != nil {
					t.Fatal(err)
				}
			},
			fail: audittest.Action("task.suspend"),
			act: func(ctx context.Context, r *Reconciler, task domain.Task) error {
				return r.SuspendTask(ctx, task.ID, "")
			},
			wantErr:     true,
			wantEffects: []string{"the runtime of task {id} was suspended", "the runtime of task {id} was stopped", "a checkpoint of task {id} was saved", "the leases of task {id} were revoked", "task {id} moved from working to suspended"},
			check: func(t *testing.T, st store.Store, task domain.Task) {
				requireState(t, st, task.ID, domain.TaskStateSuspended)
			},
		},
		{
			name: "background auto remediation is logged and goes ahead",
			prepare: func(t *testing.T, r *Reconciler, task domain.Task) {
				failTask(t, r.store, task.ID, domain.TaskStateSubmitted)
			},
			fail: audittest.Action("task.auto_remediate"),
			act: func(ctx context.Context, r *Reconciler, task domain.Task) error {
				failed, err := r.store.GetTask(ctx, task.ID)
				if err != nil {
					return err
				}
				failed.InputJSON = `{"auto_remediate":true}`
				return r.maybeAutoRemediate(ctx, failed, "test")
			},
			check: func(t *testing.T, st store.Store, task domain.Task) {
				if _, err := st.GetTaskByIdempotencyKey(ctx, "retry-"+task.ID+"-1"); err != nil {
					t.Fatalf("remediation task missing: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, st, _, task := regressionFixture(t)
			faults := audittest.Wrap(st)
			r.store = faults
			if tc.prepare != nil {
				tc.prepare(t, r, task)
			}
			faults.Fail(tc.fail)
			callCtx, effects := store.TrackEffects(ctx)
			err := tc.act(callCtx, r, task)
			if tc.wantErr != errors.Is(err, store.ErrAuditWrite) {
				t.Fatalf("got %v, want ErrAuditWrite: %v", err, tc.wantErr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr {
				msg, _ := store.AuditFailureText(callCtx, err, "op")
				if want := audittest.Answer("op", effects.List()...); msg != want {
					t.Fatalf("answer %q does not list the recorded effects %q", msg, effects.List())
				}
				var want []string
				for _, e := range tc.wantEffects {
					want = append(want, strings.ReplaceAll(e, "{id}", task.ID))
				}
				if !slices.Equal(effects.List(), want) {
					t.Fatalf("effects %q, want %q", effects.List(), want)
				}
			}
			if tc.check != nil {
				tc.check(t, st, task)
			}
		})
	}
}

func launchAndSuspend(t *testing.T, r *Reconciler, task domain.Task) {
	t.Helper()
	ctx := context.Background()
	if err := launchForTest(ctx, r, task, r.bundle.Workloads["worker"], r.adapters[domain.ClassDeterministicController]); err != nil {
		t.Fatal(err)
	}
	if err := r.SuspendTask(ctx, task.ID, ""); err != nil {
		t.Fatal(err)
	}
}
