package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// A webhook dispatch whose task.submit row cannot be written is refused
// with HTTP 500, not 409 (issue 107 row 3).
func TestGitHubWebhookAuditWriteFailureAnswers500(t *testing.T) {
	ctx := context.Background()
	raw, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{Workloads: map[string]domain.Workload{
		"triage-agent": {Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime}, Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent}},
	}}
	faults := audittest.Wrap(raw)
	faults.Fail(audittest.Action("task.submit"))
	rec := controlplane.NewReconciler(controlplane.Options{Store: faults, Bundle: bundle})
	sec := secret.NewMemoryStore(map[string]string{"github_webhook_secret": "top-secret-key"})
	h := GitHubHandler(GitHubOptions{Secrets: sec, Reconciler: rec, TargetWorkload: "triage-agent"})
	payload := `{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":1,"title":"t","body":"b"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", sign(payload, "top-secret-key"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "nothing was changed") {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
}
