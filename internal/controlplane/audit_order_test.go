package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// A resume whose task.resume row cannot be written changes nothing: no
// lease is minted and the suspended runtime is not stopped (issue 107 row 3).
func TestRefusedResumeMintsNoLeaseAndKeepsRuntime(t *testing.T) {
	ctx := context.Background()
	r, st, a, task := regressionFixture(t)
	launchAndSuspend(t, r, task)
	faults := audittest.Wrap(st)
	leases := &recordingLeases{Store: faults}
	r.store = leases
	a.mu.Lock()
	stopsBefore := a.stops
	a.mu.Unlock()
	faults.Fail(audittest.Action("task.resume"))
	if err := r.ResumeTask(ctx, task.ID); !errors.Is(err, store.ErrAuditWrite) {
		t.Fatalf("got %v, want ErrAuditWrite", err)
	}
	a.mu.Lock()
	stops := a.stops
	a.mu.Unlock()
	if len(leases.leases) != 0 || stops != stopsBefore {
		t.Fatalf("refused resume minted %d leases and stopped the runtime %d times", len(leases.leases), stops-stopsBefore)
	}
	requireState(t, st, task.ID, domain.TaskStateSuspended)
}

// A resume that fails after its task.resume row is followed by an error row,
// so the log does not claim a task resumed when it did not.
func TestFailedResumeWritesErrorRow(t *testing.T) {
	ctx := context.Background()
	r, st, _, task := regressionFixture(t)
	r.adapters[r.bundle.Workloads["worker"].Spec.Class] = &resumeFailure{}
	reconcile(t, r)
	if err := r.SuspendTask(ctx, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.ResumeTask(ctx, task.ID); err == nil {
		t.Fatal("expected failure")
	}
	rows, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var resumeRows []string
	for _, row := range rows {
		if row.Action == "task.resume" {
			resumeRows = append(resumeRows, row.Outcome+" "+row.DetailJSON)
		}
	}
	if len(resumeRows) != 2 || !strings.HasPrefix(resumeRows[0], "error ") || !strings.Contains(resumeRows[0], "runtime_resume_failed") {
		t.Fatalf("task.resume rows, newest first: %q", resumeRows)
	}
}

// replayingStore reports an earlier task for the first idempotency lookup
// that CreateTask then does not find, the race behind a late task.submit row.
type replayingStore struct {
	store.Store
	key  string
	done bool
}

func (s *replayingStore) GetTaskByIdempotencyKey(ctx context.Context, key string) (domain.Task, error) {
	if key == s.key && !s.done {
		s.done = true
		return domain.Task{ID: "task_gone", IdempotencyKey: key, WorkloadName: "worker"}, nil
	}
	return s.Store.GetTaskByIdempotencyKey(ctx, key)
}

// A task.submit row written after the task was created reports the task as
// created, not as refused.
func TestLateSubmitRowReportsCreatedTask(t *testing.T) {
	ctx := context.Background()
	r, st, _, _ := regressionFixture(t)
	faults := audittest.Wrap(st)
	r.store = &replayingStore{Store: faults, key: "late"}
	faults.Fail(audittest.Action("task.submit"))
	_, err := r.SubmitTask(ctx, "worker", "late", "{}")
	if _, getErr := st.GetTaskByIdempotencyKey(ctx, "late"); getErr != nil {
		t.Fatalf("task not created: %v (submit error %v)", getErr, err)
	}
	msg, ok := store.AuditFailureText(err, "task submission")
	if !ok || !strings.Contains(msg, "was created, but") {
		t.Fatalf("got %q %v from %v", msg, ok, err)
	}
}
