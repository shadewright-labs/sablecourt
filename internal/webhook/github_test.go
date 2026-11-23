package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

func sign(body, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubWebhookVerifiesHMACAndDispatchesIdempotently(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.Migrate(ctx)

	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{
			"triage-agent": {
				Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
			},
		},
	}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d1", AgentCardJSON: "{}",
	})

	rec := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})
	sec := secret.NewMemoryStore(map[string]string{"github_webhook_secret": "top-secret-key"})
	h := GitHubHandler(GitHubOptions{Secrets: sec, Reconciler: rec, TargetWorkload: "triage-agent"})

	payload := `{"action":"opened","repository":{"full_name":"shadewright-labs/sablecourt"},"issue":{"number":12,"title":"Bug","body":"Details"}}`

	// 1. Bad signature fails closed with 401.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad signature, got %d", rr.Code)
	}

	// 2. Valid signature dispatches task (202).
	req = httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", sign(payload, "top-secret-key"))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for valid webhook, got %d: %s", rr.Code, rr.Body.String())
	}

	// 3. Replaying same issue returns 409 conflict (idempotent).
	req = httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", sign(payload, "top-secret-key"))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate webhook replay, got %d", rr.Code)
	}
}
