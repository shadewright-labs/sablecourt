package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func failTask(t *testing.T, st store.Store, id string, from domain.TaskState) {
	t.Helper()
	if err := st.UpdateTaskState(context.Background(), id, from, domain.TaskStateFailed, "fixture", "{}"); err != nil {
		t.Fatal(err)
	}
}

func TestRetryTaskWithHandoffRefusalsAndAttempts(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown task", func(t *testing.T) {
		r, _, _, _ := regressionFixture(t)
		if _, err := r.RetryTaskWithHandoff(ctx, "missing", false, ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("retry = %v, want ErrNotFound", err)
		}
	})

	t.Run("pending runtime cleanup", func(t *testing.T) {
		r, st, _, task := regressionFixture(t)
		failTask(t, st, task.ID, domain.TaskStateSubmitted)
		r.mu.Lock()
		r.handles[task.ID] = scruntime.Handle{AdapterName: "controlled", ExternalID: task.ID}
		r.mu.Unlock()
		if _, err := r.RetryTaskWithHandoff(ctx, task.ID, false, ""); !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "cleanup is pending") {
			t.Fatalf("retry = %v, want a pending cleanup refusal", err)
		}
		ready, err := r.CouncilRetryReady(ctx, task.ID)
		if err != nil || ready {
			t.Fatalf("CouncilRetryReady with a live handle = %v, %v", ready, err)
		}
	})

	t.Run("retry of a retry advances the attempt", func(t *testing.T) {
		r, st, _, task := regressionFixture(t)
		failTask(t, st, task.ID, domain.TaskStateSubmitted)
		first, err := r.RetryTaskWithHandoff(ctx, task.ID, false, "")
		if err != nil {
			t.Fatal(err)
		}
		failTask(t, st, first.ID, first.State)
		second, err := r.RetryTaskWithHandoff(ctx, first.ID, false, "")
		if err != nil {
			t.Fatal(err)
		}
		var in map[string]any
		if err := json.Unmarshal([]byte(second.InputJSON), &in); err != nil {
			t.Fatal(err)
		}
		if second.IdempotencyKey != fmt.Sprintf("retry-%s-2", first.ID) || in["retry_count"] != float64(2) || in["retried_from"] != first.ID {
			t.Fatalf("second retry = %s %s", second.IdempotencyKey, second.InputJSON)
		}
	})

	t.Run("exhausted retry count", func(t *testing.T) {
		r, st, _, _ := regressionFixture(t)
		maxed := domain.Task{ID: "maxed", WorkloadName: "worker", State: domain.TaskStateFailed, IdempotencyKey: "maxed",
			InputJSON: fmt.Sprintf(`{"retry_count":%d}`, int(^uint(0)>>1))}
		if err := st.CreateTask(ctx, maxed); err != nil {
			t.Fatal(err)
		}
		if _, err := r.RetryTaskWithHandoff(ctx, maxed.ID, false, ""); err == nil || !strings.Contains(err.Error(), "exhausted") {
			t.Fatalf("retry = %v, want exhausted", err)
		}
	})

	t.Run("plain text input becomes the prompt", func(t *testing.T) {
		r, st, _, _ := regressionFixture(t)
		plain := domain.Task{ID: "plain", WorkloadName: "worker", State: domain.TaskStateFailed, IdempotencyKey: "plain", InputJSON: "fix the build"}
		if err := st.CreateTask(ctx, plain); err != nil {
			t.Fatal(err)
		}
		retry, err := r.RetryTaskWithHandoff(ctx, plain.ID, true, "runtime_failed")
		if err != nil {
			t.Fatal(err)
		}
		var in map[string]any
		_ = json.Unmarshal([]byte(retry.InputJSON), &in)
		handoff, _ := in["failure_handoff"].(string)
		if in["prompt"] != "fix the build" || in["retry_count"] != float64(1) || !strings.Contains(handoff, "runtime_failed") {
			t.Fatalf("retry input = %s", retry.InputJSON)
		}
	})
}

func TestRetryConclaveTaskUsesRoomReservation(t *testing.T) {
	ctx := context.Background()
	newRoom := func(t *testing.T, template string) (*Reconciler, store.Store, domain.Task) {
		r, st, _, _ := regressionFixture(t)
		room := domain.ConclaveSession{ID: "ccl-room", TemplateName: template, Title: "t", State: domain.ConclaveStateActive}
		if err := st.CreateConclave(ctx, room); err != nil {
			t.Fatal(err)
		}
		task := domain.Task{ID: "turn0", WorkloadName: "worker", State: domain.TaskStateFailed,
			IdempotencyKey: "conclave-ccl-room-init-reader",
			InputJSON:      `{"conclaveId":"ccl-room","role":"reader","activation":"always-on"}`}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		return r, st, task
	}

	t.Run("template missing", func(t *testing.T) {
		r, _, task := newRoom(t, "gone")
		if _, err := r.RetryTaskWithHandoff(ctx, task.ID, false, ""); err == nil || !strings.Contains(err.Error(), "template not found") {
			t.Fatalf("retry = %v", err)
		}
	})

	t.Run("no bundle", func(t *testing.T) {
		r, _, task := newRoom(t, "council")
		r.mu.Lock()
		r.bundle = nil
		r.mu.Unlock()
		if _, err := r.RetryTaskWithHandoff(ctx, task.ID, false, ""); err == nil || !strings.Contains(err.Error(), "no workload bundle") {
			t.Fatalf("retry = %v", err)
		}
	})

	t.Run("room missing", func(t *testing.T) {
		r, st, _, _ := regressionFixture(t)
		task := domain.Task{ID: "lost", WorkloadName: "worker", State: domain.TaskStateFailed,
			IdempotencyKey: "conclave-ccl-lost-init-reader",
			InputJSON:      `{"conclaveId":"ccl-lost","role":"reader","activation":"always-on"}`}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := r.RetryTaskWithHandoff(ctx, task.ID, false, ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("retry = %v, want ErrNotFound", err)
		}
	})

	t.Run("retry is reserved in the room once", func(t *testing.T) {
		r, st, task := newRoom(t, "council")
		r.mu.Lock()
		r.bundle.Conclaves = map[string]domain.Conclave{"council": {Spec: domain.ConclaveSpec{Vassals: []domain.ConclaveVassal{{Role: "reader", Workload: "worker"}}}}}
		r.mu.Unlock()
		first, err := r.RetryTaskWithHandoff(ctx, task.ID, false, "")
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		second, err := r.RetryTaskWithHandoff(ctx, task.ID, false, "")
		if err != nil || second.ID != first.ID {
			t.Fatalf("second retry = %+v, %v; want the same task", second, err)
		}
		a, ok := store.TaskConclaveAssignment(first)
		if !ok || a.ConclaveID != "ccl-room" || a.Role != "reader" || first.IdempotencyKey == task.IdempotencyKey {
			t.Fatalf("retry = %+v (assignment %+v, %v)", first, a, ok)
		}
		ready, err := r.CouncilRetryReady(ctx, "not-pending")
		if err != nil || !ready {
			t.Fatalf("CouncilRetryReady for an idle id = %v, %v", ready, err)
		}
		pending, _ := st.ListReconcileTasks(ctx, nil)
		if len(pending) > 0 {
			if ready, _ := r.CouncilRetryReady(ctx, pending[0].ID); ready {
				t.Fatalf("CouncilRetryReady for pending task %s = true", pending[0].ID)
			}
		}
	})
}
