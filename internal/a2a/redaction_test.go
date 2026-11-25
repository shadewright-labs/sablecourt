package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// TestA2ASubmittedInputHoldsNoSecret sends each generated secret form through
// tasks/send and checks the stored input, the task messages and tasks/get.
func TestA2ASubmittedInputHoldsNoSecret(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{Workloads: map[string]domain.Workload{
		"llm-agent": {Metadata: domain.ObjectMeta{Name: "llm-agent", Zone: domain.ZoneRuntime}, Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent}},
	}}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "llm-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d1", AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	rec := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})
	h := BrokerHandler(Options{Store: st, Reconciler: rec, Secrets: secret.NewMemoryStore(map[string]string{"a2a_bearer_token": "a2a-secret-123"})})
	call := func(body any) string {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer a2a-secret-123")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("a2a status %d: %s", rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	for i, tc := range redacttest.Corpus() {
		t.Run(tc.Name, func(t *testing.T) {
			sent := call(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tasks/send", "params": map[string]any{
				"id": fmt.Sprintf("ext-%d", i), "workload": "llm-agent",
				"message": map[string]any{"parts": []map[string]string{{"text": tc.Text}}},
			}})
			var resp struct {
				Result struct{ ID string } `json:"result"`
			}
			if err := json.Unmarshal([]byte(sent), &resp); err != nil || resp.Result.ID == "" {
				t.Fatalf("tasks/send: %s", sent)
			}
			task, err := st.GetTask(ctx, resp.Result.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid([]byte(task.InputJSON)) {
				t.Fatalf("stored input is not JSON: %q", task.InputJSON)
			}
			if frag, ok := redacttest.Leak(task.InputJSON, tc.Secrets); ok {
				t.Fatalf("stored input_json holds %q", frag)
			}
			msgs, err := st.ListTaskMessages(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range msgs {
				if frag, ok := redacttest.Leak(m.Content, tc.Secrets); ok {
					t.Fatalf("task message holds %q", frag)
				}
			}
			got := call(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tasks/get", "params": map[string]any{"id": task.ID}})
			if frag, ok := redacttest.Leak(got, tc.Secrets); ok {
				t.Fatalf("tasks/get returns %q", frag)
			}
		})
	}
}
