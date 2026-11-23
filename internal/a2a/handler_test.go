package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestA2ABrokerSendAndGet(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.Migrate(ctx)

	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{
			"llm-agent": {
				Metadata: domain.ObjectMeta{Name: "llm-agent", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
			},
		},
	}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "llm-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d1", AgentCardJSON: "{}",
	})

	rec := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})
	sec := secret.NewMemoryStore(map[string]string{"a2a_bearer_token": "a2a-secret-123"})
	h := BrokerHandler(Options{Store: st, Reconciler: rec, Secrets: sec})

	// Unauthorized call fails closed.
	req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tasks/send"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rr.Code)
	}

	// tasks/send creates task.
	sendBody := `{"jsonrpc":"2.0","id":1,"method":"tasks/send","params":{"id":"ext-1","workload":"llm-agent","message":{"parts":[{"text":"Summarize status"}]}}}`
	req = httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(sendBody))
	req.Header.Set("Authorization", "Bearer a2a-secret-123")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on tasks/send, got %d: %s", rr.Code, rr.Body.String())
	}

	var sendResp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &sendResp)
	if sendResp.Result.ID == "" {
		t.Fatalf("expected task ID in A2A response: %s", rr.Body.String())
	}

	// tasks/get returns task status.
	getBody := `{"jsonrpc":"2.0","id":2,"method":"tasks/get","params":{"id":"` + sendResp.Result.ID + `"}}`
	req = httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(getBody))
	req.Header.Set("Authorization", "Bearer a2a-secret-123")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "submitted") {
		t.Fatalf("unexpected tasks/get response: code=%d body=%s", rr.Code, rr.Body.String())
	}
}
