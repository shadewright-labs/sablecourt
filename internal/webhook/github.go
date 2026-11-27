package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type GitHubOptions struct {
	Secrets        secret.SecretStore
	SecretName     string
	Reconciler     *controlplane.Reconciler
	TargetWorkflow string
	TargetWorkload string
}

// GitHubHandler verifies X-Hub-Signature-256 in constant time (fail-closed)
// and dispatches idempotent tasks or workflows on issues.opened events.
func GitHubHandler(opts GitHubOptions) http.Handler {
	secretName := opts.SecretName
	if secretName == "" {
		secretName = "github_webhook_secret" // #nosec G101 -- GitHubHandler passes this lookup name to Secrets.Get for the signing key.
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		if opts.Secrets == nil || opts.Reconciler == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "webhook handler unavailable"})
			return
		}
		whSecret, err := opts.Secrets.Get(r.Context(), secretName)
		if err != nil || strings.TrimSpace(whSecret) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "webhook secret not configured"})
			return
		}
		redact.Register(whSecret)

		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
			return
		}
		if !VerifyGitHubSignature(body, r.Header.Get("X-Hub-Signature-256"), whSecret) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid webhook signature"})
			return
		}

		eventType := r.Header.Get("X-GitHub-Event")
		if eventType != "issues" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": eventType})
			return
		}

		var payload struct {
			Action     string `json:"action"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			Issue struct {
				Number int    `json:"number"`
				Title  string `json:"title"`
				Body   string `json:"body"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
			return
		}
		if payload.Action != "opened" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "action": payload.Action})
			return
		}

		summary := fmt.Sprintf("GitHub issue %s#%d: %s\n\n%s", payload.Repository.FullName, payload.Issue.Number, payload.Issue.Title, payload.Issue.Body)
		idemKey := fmt.Sprintf("gh:issue:%s:%d", payload.Repository.FullName, payload.Issue.Number)

		var taskID string
		if opts.TargetWorkflow != "" {
			t, err := opts.Reconciler.StartWorkflow(r.Context(), opts.TargetWorkflow, summary)
			if err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			taskID = t.ID
		} else {
			wl := opts.TargetWorkload
			if wl == "" {
				wl = "triage-agent"
			}
			inBytes, _ := json.Marshal(map[string]any{"prompt": summary, "issue": payload.Issue.Number, "repo": payload.Repository.FullName})
			t, err := opts.Reconciler.SubmitTask(r.Context(), wl, idemKey, string(inBytes))
			if err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			taskID = t.ID
		}

		writeJSON(w, http.StatusAccepted, map[string]string{"status": "dispatched", "taskId": taskID})
	})
}

func VerifyGitHubSignature(body []byte, sigHeader, secretKey string) bool {
	if !strings.HasPrefix(sigHeader, "sha256=") {
		return false
	}
	gotHex := strings.TrimPrefix(sigHeader, "sha256=")
	gotBytes, err := hex.DecodeString(gotHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(gotBytes, expected)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var _ = context.Background
