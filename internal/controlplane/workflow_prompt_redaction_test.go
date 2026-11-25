package controlplane

import (
	"fmt"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
)

// TestWorkflowStepPromptMessageIsRedacted advances a workflow whose next step
// prompt carries each corpus secret. The system message announcing the step
// quotes the prompt, so it must reach history redacted.
func TestWorkflowStepPromptMessageIsRedacted(t *testing.T) {
	ctx := t.Context()
	for i, tc := range redacttest.Corpus() {
		t.Run(tc.Name, func(t *testing.T) {
			r, st, _, unused := regressionFixture(t)
			if err := r.CancelTask(ctx, unused.ID, ""); err != nil {
				t.Fatal(err)
			}
			steps := []domain.WorkflowStep{{Name: "first", Workload: "worker"}, {Name: "second", Workload: "worker", Prompt: tc.Text}}
			r.bundle.Workflows = map[string]domain.Workflow{"flow": {Spec: domain.WorkflowSpec{Steps: steps}}}
			root, err := r.StartWorkflow(ctx, "flow", fmt.Sprintf("start %d", i))
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateTaskState(ctx, root.ID, root.State, domain.TaskStateCompleted, "runtime_exit_observed", "{}"); err != nil {
				t.Fatal(err)
			}
			root.State = domain.TaskStateCompleted
			if err := r.advanceWorkflowStep(ctx, root); err != nil {
				t.Fatal(err)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			advanced := false
			for _, task := range tasks {
				msgs, err := st.ListTaskMessages(ctx, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range msgs {
					if m.Role == "system" {
						advanced = true
					}
					if frag, ok := redacttest.Leak(m.Content, tc.Secrets); ok {
						t.Fatalf("task %s %s message holds %q", task.ID, m.Role, frag)
					}
				}
				if frag, ok := redacttest.Leak(task.InputJSON, tc.Secrets); ok {
					t.Fatalf("task %s input holds %q", task.ID, frag)
				}
			}
			if !advanced {
				t.Fatal("workflow did not advance")
			}
		})
	}
}
