package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// stepTasks returns every task of the workflow step at index.
func (f *commandFixture) stepTasks(index int) []domain.Task {
	var out []domain.Task
	for _, task := range f.tasks() {
		var meta struct {
			Workflow  string `json:"workflow"`
			StepIndex int    `json:"step_index"`
		}
		if json.Unmarshal([]byte(task.InputJSON), &meta) == nil && meta.Workflow == commandWorkflow && meta.StepIndex == index {
			out = append(out, task)
		}
	}
	return out
}

func (f *commandFixture) messages(taskID, prefix string) int {
	f.t.Helper()
	msgs, err := f.st.ListTaskMessages(f.t.Context(), taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.Role == "system" && strings.HasPrefix(m.Content, prefix) {
			n++
		}
	}
	return n
}

// TestWorkflowDecisionReplayWritesOnce covers the decision writes a
// reconcile replay repeats (issue 249 and the same kind after it): the
// changes_requested message and audit row written before the remediation
// submit, and the messages and audit rows written after a step's submit
// (remediation, advance, auto-remediation), which a replay repeats when the
// step's closing write fails. One write fails once in each case; after the
// replay every decision has one message and one audit row.
func TestWorkflowDecisionReplayWritesOnce(t *testing.T) {
	t.Parallel()
	cond := func(role, reason string) string {
		return "NEW.reason = '" + reason + "' AND json_extract((SELECT input_json FROM tasks WHERE id = NEW.task_id), '$.role') = '" + role + "'"
	}
	cases := map[string]struct {
		table, cond   string
		authorRetries bool // the first author attempt fails and is retried
	}{
		"remediation submit fails":  {table: "tasks", cond: "NEW.idempotency_key LIKE 'wf:%:remediate:%'"},
		"verifier close fails":      {table: "task_events", cond: cond("verifier", "runtime_completed")},
		"author close fails":        {table: "task_events", cond: cond("author", "runtime_completed")},
		"failed author close fails": {table: "task_events", cond: cond("author", "runtime_failed"), authorRetries: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCommandFixture(t, checkCommand(0, 0), func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
				return "ok", "", 0, nil
			})
			steps := f.bundle.Workflows[commandWorkflow].Spec.Steps
			steps[2].MaxRetries = 1
			if tc.authorRetries {
				// The retried author carries retry_count 1, so the review
				// needs a budget of 2 to request one more change.
				steps[0].MaxRetries, steps[2].MaxRetries = 1, 2
			}
			var reviews, authors atomic.Int32
			defaultRun := f.runner.onRun
			f.runner.onRun = func(ctx context.Context, s scruntime.CouncilLaunch) (string, string, int, error) {
				switch {
				case s.Command != nil:
				case s.Checkout.Kind == scruntime.CouncilTask:
					if tc.authorRetries && authors.Add(1) == 1 {
						return "", "could not build", 1, nil
					}
				default:
					if reviews.Add(1) == 1 {
						return "the change needs a test\nVERDICT: CHANGES_REQUESTED", "", 0, nil
					}
				}
				return defaultRun(ctx, s)
			}
			fault := armWriteFault(t, f, tc.table, tc.cond, 1)
			f.start()
			// Every decision under test is written once the review that
			// requested changes is closed and the author it sent back is
			// submitted.
			f.runUntil("the closed review and its remediation", func() bool {
				reviews := f.stepTasks(2)
				if len(reviews) == 0 {
					return false
				}
				first := reviews[len(reviews)-1] // tasks are listed newest first
				if _, ok := f.event(first.ID, "runtime_completed"); !ok {
					return false
				}
				for _, a := range f.stepTasks(0) {
					if a.ParentTaskID == first.ID {
						return true
					}
				}
				return false
			})
			if fault.Load() > 0 {
				t.Fatal("the injected write failure never fired")
			}
			// More passes find nothing left to replay.
			for range 3 {
				_ = f.r.ReconcileOnce(t.Context())
				_ = waitForLaunches(t, f.r)
			}
			audits, err := f.st.ListAudit(t.Context(), 1000)
			if err != nil {
				t.Fatal(err)
			}
			perTask := map[string]int{}
			changes, remediations := 0, 0
			for _, a := range audits {
				switch {
				case a.Action == "workflow.verify" && a.Outcome == "changes_requested":
					changes++
				case a.Action == "workflow.advance":
					perTask[a.TaskID]++
				case a.Action == "task.auto_remediate":
					remediations++
				}
			}
			if changes != 1 {
				t.Errorf("changes_requested audit rows = %d, want 1", changes)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.authorRetries]; remediations != want {
				t.Errorf("task.auto_remediate audit rows = %d, want %d", remediations, want)
			}
			for id, n := range perTask {
				if n != 1 {
					t.Errorf("workflow.advance audit rows of %s = %d, want 1", id, n)
				}
			}
			for _, task := range f.tasks() {
				for _, prefix := range []string{"Verifier returned VERDICT", "Closed-loop remediation", "Workflow " + commandWorkflow + " advanced"} {
					if n := f.messages(task.ID, prefix); n > 1 {
						t.Errorf("task %s has %d %q messages, want at most 1", task.ID, n, prefix)
					}
				}
			}
			verifierMsgs, remediationMsgs := 0, 0
			for _, task := range f.tasks() {
				verifierMsgs += f.messages(task.ID, "Verifier returned VERDICT")
				remediationMsgs += f.messages(task.ID, "Closed-loop remediation")
			}
			if verifierMsgs != 1 || remediationMsgs != 1 {
				t.Errorf("changes_requested messages = %d, remediation messages = %d, want 1 each", verifierMsgs, remediationMsgs)
			}
		})
	}
}
