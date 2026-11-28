package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/redact"
)

func postAction(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://sablecourt.local"+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body["error"]
}

func TestActionHandlerRefusals(t *testing.T) {
	gate, st, _, token := setupMCPGateTest(t)
	ctx := context.Background()

	// A lease for a workload that the bundle does not declare.
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "ghost", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "g", AgentCardJSON: "{}"})
	_ = st.CreateTask(ctx, domain.Task{ID: "task-ghost", IdempotencyKey: "idem-ghost", WorkloadName: "ghost", State: domain.TaskStateWorking})
	ghostToken, err := model.MintTaskToken(ctx, st, "task-ghost", "ghost", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	h := gate.ActionHandler()
	cases := []struct {
		name     string
		method   string
		path     string
		token    string
		rawAuth  string
		body     string
		code     int
		errorHas string
	}{
		{"GET refused", http.MethodGet, "/git.open_pr", token, "", "", http.StatusMethodNotAllowed, "POST required"},
		{"no bearer", http.MethodPost, "/git.open_pr", "", "", `{}`, http.StatusUnauthorized, "bearer task token required"},
		{"basic auth", http.MethodPost, "/git.open_pr", "", "Basic abc", `{}`, http.StatusUnauthorized, "bearer task token required"},
		{"foreign token format", http.MethodPost, "/git.open_pr", "abc123", "", `{}`, http.StatusUnauthorized, "invalid task token format"},
		{"unknown token", http.MethodPost, "/git.open_pr", model.TokenPrefix + "unknown", "", `{}`, http.StatusUnauthorized, "invalid, expired, or revoked"},
		{"undeclared workload", http.MethodPost, "/git.open_pr", ghostToken, "", `{}`, http.StatusForbidden, "workload not declared"},
		{"empty action name", http.MethodPost, "/", token, "", `{}`, http.StatusNotFound, "action name required"},
		{"action outside catalog", http.MethodPost, "/git.force_push", token, "", `{}`, http.StatusNotFound, "closed catalog"},
		{"unknown request field", http.MethodPost, "/git.open_pr", token, "", `{"payload":{},"extra":1}`, http.StatusBadRequest, "invalid action request"},
		{"body not JSON", http.MethodPost, "/git.open_pr", token, "", `not json`, http.StatusBadRequest, "invalid action request"},
		{"unknown approval id", http.MethodPost, "/git.open_pr", token, "", `{"approvalId":"apr_missing","payload":{}}`, http.StatusForbidden, "approval_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://sablecourt.local"+tc.path, strings.NewReader(tc.body))
			switch {
			case tc.rawAuth != "":
				req.Header.Set("Authorization", tc.rawAuth)
			case tc.token != "":
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			if got := errorText(t, rec); !strings.Contains(got, tc.errorHas) {
				t.Fatalf("error = %q, want it to contain %q", got, tc.errorHas)
			}
		})
	}

	recs, err := st.ListAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAuditRecord(recs, "data_action_gate", "action:git.force_push", "denied", "unknown_action") {
		t.Fatal("no denied audit record for the action outside the catalog")
	}
}

func TestActionHandlerFailsClosedWithoutPolicyOrBundle(t *testing.T) {
	gate, st, _, token := setupMCPGateTest(t)
	bundle := gate.snapshot().bundle

	mustReload(t, gate, bundle, nil)
	rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.open_pr", token, `{"payload":{}}`)
	if rec.Code != http.StatusForbidden || errorText(t, rec) != "policy failed closed" {
		t.Fatalf("nil policy: %d %s", rec.Code, rec.Body.String())
	}
	recs, _ := st.ListAudit(context.Background(), 10)
	if !hasAuditRecord(recs, "data_action_gate", "action:git.open_pr", "denied", "policy_engine_unavailable") {
		t.Fatal("no audit record for the missing policy engine")
	}

	mustReload(t, gate, nil, policy.NewEngine(nil, nil))
	rec = postAction(t, gate.ActionHandler(), http.MethodPost, "/git.open_pr", token, `{"payload":{}}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(errorText(t, rec), "no workload bundle") {
		t.Fatalf("nil bundle: %d %s", rec.Code, rec.Body.String())
	}
}

func TestActionHandlerApprovalChecks(t *testing.T) {
	ctx := context.Background()
	gate, st, queue, token := setupMCPGateTest(t)
	h := gate.ActionHandler()
	payload := map[string]string{"branch": "sablecourt/task-1"}

	// A pending approval does not authorize the action.
	pending, err := queue.Request(ctx, "task-1", "triage-agent", "action:git.open_pr", payload, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"approvalId":"` + pending.ID + `","payload":{"branch":"sablecourt/task-1"}}`
	if rec := postAction(t, h, http.MethodPost, "/git.open_pr", token, body); rec.Code != http.StatusForbidden {
		t.Fatalf("pending approval: %d %s", rec.Code, rec.Body.String())
	}

	// An approval for another capability does not authorize this one.
	other, err := queue.Request(ctx, "task-1", "triage-agent", "action:git.merge", payload, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Approve(ctx, other.ID, other.PayloadSHA256, "op"); err != nil {
		t.Fatal(err)
	}
	body = `{"approvalId":"` + other.ID + `","payload":{"branch":"sablecourt/task-1"}}`
	if rec := postAction(t, h, http.MethodPost, "/git.open_pr", token, body); rec.Code != http.StatusForbidden {
		t.Fatalf("approval for another capability: %d %s", rec.Code, rec.Body.String())
	}

	// An approval for another task does not authorize this task.
	_ = st.CreateTask(ctx, domain.Task{ID: "task-2", IdempotencyKey: "idem-2", WorkloadName: "triage-agent", State: domain.TaskStateWorking})
	foreign, err := queue.Request(ctx, "task-2", "triage-agent", "action:git.open_pr", payload, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Approve(ctx, foreign.ID, foreign.PayloadSHA256, "op"); err != nil {
		t.Fatal(err)
	}
	body = `{"approvalId":"` + foreign.ID + `","payload":{"branch":"sablecourt/task-1"}}`
	if rec := postAction(t, h, http.MethodPost, "/git.open_pr", token, body); rec.Code != http.StatusForbidden {
		t.Fatalf("approval for another task: %d %s", rec.Code, rec.Body.String())
	}

	recs, _ := st.ListAudit(ctx, 100)
	if !hasAuditRecord(recs, "data_action_gate", "action:git.open_pr", "denied", "approval_mismatch_or_unapproved") {
		t.Fatal("no audit record for the refused approvals")
	}

	// Without a queue the gate cannot ask, and says so.
	gate.approvals = nil
	if rec := postAction(t, h, http.MethodPost, "/git.open_pr", token, `{"payload":{}}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no queue: %d %s", rec.Code, rec.Body.String())
	}
}

func TestActionHandlerBreakerAndRedaction(t *testing.T) {
	ctx := context.Background()
	gate, st, _, token := setupMCPGateTest(t)

	// Let the workload run git.push directly.
	bundle := gate.snapshot().bundle
	wl := bundle.Workloads["triage-agent"]
	wl.Spec.Grants.Actions = append(wl.Spec.Grants.Actions, "git.push")
	bundle.Workloads["triage-agent"] = wl
	pol := bundle.Policies["default"]
	pol.Spec.Rules = append(pol.Spec.Rules, domain.PolicyRule{Workload: "triage-agent", Capability: "action:git.push", Effect: domain.EffectAllow})
	breaker := policy.NewCircuitBreaker(2, time.Hour)
	gate.breaker = breaker
	mustReload(t, gate, bundle, policy.NewEngine([]domain.Policy{pol}, breaker))

	secretValue := "ghp_" + strings.Repeat("a1B2", 9)
	fail := true
	gate.RegisterAction("git.push", func(context.Context, domain.Workload, string, json.RawMessage) (any, error) {
		if fail {
			return nil, errors.New("remote said: token " + secretValue + " rejected")
		}
		return map[string]string{"log": "pushed with " + secretValue}, nil
	})
	h := gate.ActionHandler()

	rec := postAction(t, h, http.MethodPost, "/git.push", token, `{"payload":{}}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failing action: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretValue) {
		t.Fatalf("error leaked the credential: %s", rec.Body.String())
	}

	// One success resets the count, so two more failures are needed to trip.
	fail = false
	rec = postAction(t, h, http.MethodPost, "/git.push", token, `{"payload":{}}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secretValue) {
		t.Fatalf("success: %d %s", rec.Code, rec.Body.String())
	}
	fail = true
	postAction(t, h, http.MethodPost, "/git.push", token, `{"payload":{}}`)
	if breaker.IsOpen("action:git.push") {
		t.Fatal("breaker open after one failure following a success")
	}
	postAction(t, h, http.MethodPost, "/git.push", token, `{"payload":{}}`)
	rec = postAction(t, h, http.MethodPost, "/git.push", token, `{"payload":{}}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(errorText(t, rec), "circuit_breaker_open") {
		t.Fatalf("tripped breaker: %d %s", rec.Code, rec.Body.String())
	}

	recs, _ := st.ListAudit(ctx, 100)
	if !hasAuditRecord(recs, "data_action_gate", "action:git.push", "error", "") {
		t.Fatal("no error audit record")
	}
	for _, r := range recs {
		if strings.Contains(r.DetailJSON, secretValue) {
			t.Fatalf("audit record leaked the credential: %s", r.DetailJSON)
		}
	}
}

// The decoder error names an unknown request field. A workload must not be able
// to turn that into an echo path for a daemon-held secret it can guess.
func TestActionRefusalRedactsRegisteredSecretInFieldName(t *testing.T) {
	gate, _, _, token := setupMCPGateTest(t)
	secret := "refusal_field_" + strings.Repeat("q7", 12)
	redact.Register(secret)
	rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.open_pr", token, `{"payload":{},"`+secret+`":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("refusal echoed a registered secret: %s", rec.Body.String())
	}
}
