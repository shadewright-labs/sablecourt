package webhook

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

func newWebhookReconciler(t *testing.T) (*controlplane.Reconciler, *sqlite.SQLiteStore) {
	t.Helper()
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
		"triage-agent": {
			Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime},
			Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
		},
	}}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d1", AgentCardJSON: "{}"})
	return controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle}), st
}

func TestGitHubHandlerRefusals(t *testing.T) {
	rec, st := newWebhookReconciler(t)
	key := "k-" + strings.Repeat("x", 20)
	secrets := secret.NewMemoryStore(map[string]string{"github_webhook_secret": key, "blank": "  "})
	opened := `{"action":"opened","repository":{"full_name":"acme/widgets"},"issue":{"number":7,"title":"T","body":"B"}}`

	cases := []struct {
		name     string
		opts     GitHubOptions
		method   string
		event    string
		body     string
		sig      string
		code     int
		bodyHas  string
		dispatch bool
	}{
		{"GET", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodGet, "issues", opened, sign(opened, key), http.StatusMethodNotAllowed, "POST required", false},
		{"no secret store", GitHubOptions{Reconciler: rec}, http.MethodPost, "issues", opened, sign(opened, key), http.StatusServiceUnavailable, "unavailable", false},
		{"no reconciler", GitHubOptions{Secrets: secrets}, http.MethodPost, "issues", opened, sign(opened, key), http.StatusServiceUnavailable, "unavailable", false},
		{"secret missing", GitHubOptions{Secrets: secrets, Reconciler: rec, SecretName: "absent"}, http.MethodPost, "issues", opened, sign(opened, key), http.StatusUnauthorized, "not configured", false},
		{"secret blank", GitHubOptions{Secrets: secrets, Reconciler: rec, SecretName: "blank"}, http.MethodPost, "issues", opened, sign(opened, "  "), http.StatusUnauthorized, "not configured", false},
		{"no signature", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "issues", opened, "", http.StatusUnauthorized, "invalid webhook signature", false},
		{"sha1 signature", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "issues", opened, "sha1=" + strings.TrimPrefix(sign(opened, key), "sha256="), http.StatusUnauthorized, "invalid webhook signature", false},
		{"signature not hex", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "issues", opened, "sha256=zz", http.StatusUnauthorized, "invalid webhook signature", false},
		{"signed with another key", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "issues", opened, sign(opened, "other"), http.StatusUnauthorized, "invalid webhook signature", false},
		{"signature of another body", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "issues", opened, sign(opened+" ", key), http.StatusUnauthorized, "invalid webhook signature", false},
		{"other event ignored", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "push", opened, sign(opened, key), http.StatusOK, `"ignored"`, false},
		{"bad JSON", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "issues", `{"action":`, sign(`{"action":`, key), http.StatusBadRequest, "invalid JSON payload", false},
		{"closed ignored", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "issues", `{"action":"closed"}`, sign(`{"action":"closed"}`, key), http.StatusOK, `"closed"`, false},
		{"unknown workload", GitHubOptions{Secrets: secrets, Reconciler: rec, TargetWorkload: "ghost"}, http.MethodPost, "issues", opened, sign(opened, key), http.StatusConflict, "ghost", false},
		{"unknown workflow", GitHubOptions{Secrets: secrets, Reconciler: rec, TargetWorkflow: "nope"}, http.MethodPost, "issues", opened, sign(opened, key), http.StatusConflict, "nope", false},
		{"default workload", GitHubOptions{Secrets: secrets, Reconciler: rec}, http.MethodPost, "issues", opened, sign(opened, key), http.StatusAccepted, "dispatched", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := st.ListTasks(context.Background())
			req := httptest.NewRequest(tc.method, "/api/v1/hooks/github", strings.NewReader(tc.body))
			req.Header.Set("X-GitHub-Event", tc.event)
			if tc.sig != "" {
				req.Header.Set("X-Hub-Signature-256", tc.sig)
			}
			rr := httptest.NewRecorder()
			GitHubHandler(tc.opts).ServeHTTP(rr, req)
			if rr.Code != tc.code || !strings.Contains(rr.Body.String(), tc.bodyHas) {
				t.Fatalf("got %d %s, want %d containing %q", rr.Code, rr.Body.String(), tc.code, tc.bodyHas)
			}
			after, _ := st.ListTasks(context.Background())
			if dispatched := len(after) > len(before); dispatched != tc.dispatch {
				t.Fatalf("task created = %v, want %v", dispatched, tc.dispatch)
			}
			if tc.dispatch {
				task := after[0]
				for _, x := range after {
					if x.IdempotencyKey == "gh:issue:acme/widgets:7" {
						task = x
					}
				}
				var in map[string]any
				_ = json.Unmarshal([]byte(task.InputJSON), &in)
				if task.WorkloadName != "triage-agent" || task.IdempotencyKey != "gh:issue:acme/widgets:7" || in["repo"] != "acme/widgets" {
					t.Fatalf("task = %+v", task)
				}
			}
		})
	}
}

func TestVerifyGitHubSignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	good := sign(string(body), "key")
	cases := []struct {
		name string
		sig  string
		key  string
		want bool
	}{
		{"valid", good, "key", true},
		{"upper case hex", "sha256=" + strings.ToUpper(strings.TrimPrefix(good, "sha256=")), "key", true},
		{"empty", "", "key", false},
		{"prefix only", "sha256=", "key", false},
		{"truncated", good[:len(good)-2], "key", false},
		{"wrong key", good, "other", false},
		{"odd hex", good + "0", "key", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyGitHubSignature(body, tc.sig, tc.key); got != tc.want {
				t.Fatalf("VerifyGitHubSignature(%q) = %v, want %v", tc.sig, got, tc.want)
			}
		})
	}
}
