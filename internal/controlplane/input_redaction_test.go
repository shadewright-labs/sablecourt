package controlplane

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
)

// TestSubmittedInputIsRedacted covers the two entry points that create a task
// row: submitTask (API, A2A, UI, webhooks, schedules, workflow steps, retries
// and council turns all call it) and ProposeChildTask.
func TestSubmittedInputIsRedacted(t *testing.T) {
	ctx := t.Context()
	r, st, _, parent := regressionFixture(t)
	key, keyBody := redacttest.UnterminatedKey(0)
	cases := append(redacttest.Corpus(), redacttest.Case{Name: "unterminated key", Text: key, Secrets: []string{keyBody}})
	for i, tc := range cases {
		wrapped, _ := json.Marshal(map[string]string{"prompt": tc.Text})
		for _, input := range []string{tc.Text, string(wrapped)} {
			for _, path := range []string{"submit", "child"} {
				var task domain.Task
				var err error
				idem := fmt.Sprintf("%s-%d-%d", path, i, len(input))
				if path == "submit" {
					task, err = r.SubmitTask(ctx, "worker", idem, input)
				} else {
					task, err = r.ProposeChildTask(ctx, parent.ID, "worker", idem, input, "{}")
				}
				if err != nil {
					t.Fatalf("%s/%s: %v", tc.Name, path, err)
				}
				stored, err := st.GetTask(ctx, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if frag, ok := redacttest.Leak(stored.InputJSON, tc.Secrets); ok {
					t.Fatalf("%s/%s: input_json holds %q", tc.Name, path, frag)
				}
				if json.Valid([]byte(input)) && !json.Valid([]byte(stored.InputJSON)) {
					t.Fatalf("%s/%s: JSON input stored as invalid JSON", tc.Name, path)
				}
				msgs, err := st.ListTaskMessages(ctx, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range msgs {
					if frag, ok := redacttest.Leak(m.Content, tc.Secrets); ok {
						t.Fatalf("%s/%s: task message holds %q", tc.Name, path, frag)
					}
				}
			}
		}
	}
}
