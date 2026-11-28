package postgres

import (
	"context"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func TestLivePostgresTaskLedgerModelsIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	ctx := context.Background()
	st, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassCodingAgent, AgentCardJSON: "{}"})
	taskA, taskB := "task-ledger-a-"+suffix, "task-ledger-b-"+suffix
	for _, id := range []string{taskA, taskB} {
		if err := st.CreateTask(ctx, domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "coder", State: domain.TaskStateWorking}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	for _, e := range []struct{ task, model string }{
		{taskA, "gpt-6.2-astra"}, {taskA, "claude-opus-5-5"}, {taskA, "gpt-6.2-astra"}, {taskB, "gemini-3.8-pro"},
	} {
		if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: e.task, WorkloadName: "coder", Provider: "p", Model: e.model, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("RecordBudgetUsage: %v", err)
		}
	}
	for task, want := range map[string][]string{
		taskA:                        {"claude-opus-5-5", "gpt-6.2-astra"},
		taskB:                        {"gemini-3.8-pro"},
		"task-ledger-none-" + suffix: nil,
	} {
		got, err := st.TaskLedgerModels(ctx, task)
		if err != nil {
			t.Fatalf("TaskLedgerModels(%s): %v", task, err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("TaskLedgerModels(%s) = %v, want %v", task, got, want)
		}
	}
}
