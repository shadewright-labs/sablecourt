package sqlite

import (
	"context"
	"slices"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func TestTaskLedgerModels(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassCodingAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatalf("UpsertWorkload: %v", err)
	}
	for _, id := range []string{"task-a", "task-b"} {
		if err := st.CreateTask(ctx, domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "coder", State: domain.TaskStateWorking}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	for _, e := range []struct{ task, model string }{
		{"task-a", "gpt-6.2-astra"}, {"task-a", "claude-opus-5-5"}, {"task-a", "gpt-6.2-astra"}, {"task-b", "gemini-3.8-pro"},
	} {
		if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: e.task, WorkloadName: "coder", Provider: "p", Model: e.model}); err != nil {
			t.Fatalf("RecordBudgetUsage: %v", err)
		}
	}
	cases := []struct {
		task string
		want []string
	}{
		{"task-a", []string{"claude-opus-5-5", "gpt-6.2-astra"}},
		{"task-b", []string{"gemini-3.8-pro"}},
		{"task-none", nil},
	}
	for _, tc := range cases {
		got, err := st.TaskLedgerModels(ctx, tc.task)
		if err != nil {
			t.Fatalf("TaskLedgerModels(%s): %v", tc.task, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("TaskLedgerModels(%s) = %v, want %v", tc.task, got, tc.want)
		}
	}
}
