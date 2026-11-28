package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// A retry whose task.retry row could not be written answers 500. Repeating it
// returns the task the first call created, and that answer is backed by a
// task.retry row naming the original, so no successor is left without its
// retried_from link.
func TestRetryReplayWritesMissingRetryRow(t *testing.T) {
	var faults *audittest.Store
	env := newAPIEnv(t, apiEnvConfig{wrap: func(st store.Store) store.Store {
		faults = audittest.Wrap(st)
		return faults
	}})
	env.seedTask(t, "task_old", domain.TaskStateFailed)
	faults.Fail(audittest.Action("task.retry"))
	if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_old/retry", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("first retry %d %s, want 500", rec.Code, rec.Body.String())
	}
	faults.Heal()
	rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_old/retry", "")
	var retried domain.Task
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &retried) != nil || retried.ID == "" {
		t.Fatalf("repeated retry %d %s", rec.Code, rec.Body.String())
	}
	tasks, err := env.st.ListTasks(context.Background())
	if err != nil || len(tasks) != 2 {
		t.Fatalf("the repeat created another task: %d tasks, %v", len(tasks), err)
	}
	audit, err := env.st.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range audit {
		if a.Action == "task.retry" && a.TaskID == retried.ID && strings.Contains(a.DetailJSON, `"retried_from":"task_old"`) {
			return
		}
	}
	t.Fatalf("no task.retry row for %s after the repeat: %+v", retried.ID, audit)
}
