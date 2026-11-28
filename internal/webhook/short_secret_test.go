package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// A webhook secret too short to redact is refused before the signature is
// checked, even for a correctly signed request, and the reply does not show it.
func TestGitHubHandlerRefusesShortSecret(t *testing.T) {
	rec, st := newWebhookReconciler(t)
	const short = "short12"
	h := GitHubHandler(GitHubOptions{
		Secrets:        secret.NewMemoryStore(map[string]string{"github_webhook_secret": short}),
		Reconciler:     rec,
		TargetWorkload: "triage-agent",
	})
	payload := `{"action":"opened","repository":{"full_name":"acme/widgets"},"issue":{"number":1,"title":"t","body":"b"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", sign(payload, short))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || strings.Contains(rr.Body.String(), short) {
		t.Fatalf("short secret: %d %s", rr.Code, rr.Body.String())
	}
	if tasks, _ := st.ListTasks(t.Context()); len(tasks) != 0 {
		t.Fatalf("refused request created tasks: %d", len(tasks))
	}
}
