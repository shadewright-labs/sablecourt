package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
)

func TestPostedTaskMessageIsRedacted(t *testing.T) {
	ctx := t.Context()
	st := newUITestStore(t)
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "w", Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "task-ui", IdempotencyKey: "task-ui", WorkloadName: "w", State: domain.TaskStateSubmitted}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	dash, err := NewDashboard(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	dash.RegisterRoutes(mux)
	var secrets []string
	for _, tc := range redacttest.Corpus() {
		form := url.Values{"content": {tc.Text}}
		r := httptest.NewRequest(http.MethodPost, "/ui/tasks/task-ui/messages", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		mux.ServeHTTP(httptest.NewRecorder(), r)
		secrets = append(secrets, tc.Secrets...)
	}
	msgs, err := st.ListTaskMessages(ctx, "task-ui")
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages: %d, %v", len(msgs), err)
	}
	for _, m := range msgs {
		if frag, ok := redacttest.Leak(m.Content, secrets); ok {
			t.Fatalf("stored task message holds %q", frag)
		}
	}
}
