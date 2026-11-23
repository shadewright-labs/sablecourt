package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
	"github.com/shadewright-labs/sablecourt/internal/ui"
)

func TestDaemonSmokeWithLocalDemoManifests(t *testing.T) {
	ctx := context.Background()
	bundle, err := config.LoadPath(filepath.Join("..", "..", "examples", "local-demo.yaml"))
	if err != nil {
		t.Fatalf("load examples/local-demo.yaml: %v", err)
	}

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, w := range bundle.Workloads {
		cardBytes, _ := json.Marshal(w.Spec.AgentCard)
		_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name:           w.Metadata.Name,
			Zone:           w.Metadata.Zone,
			Class:          w.Spec.Class,
			ManifestDigest: bundle.ManifestDigest,
			AgentCardJSON:  string(cardBytes),
		})
	}

	var policies []domain.Policy
	for _, p := range bundle.Policies {
		policies = append(policies, p)
	}
	breaker := policy.NewCircuitBreaker(3, time.Minute)
	policyEng := policy.NewEngine(policies, breaker)
	approvals := approval.NewQueue(st, nil)
	modelGate := model.New(model.Options{Store: st, Secrets: secret.NewMemoryStore(nil), Bundle: bundle})
	dataGate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: policyEng, Breaker: breaker, Approvals: approvals})
	mcpgate.RegisterBuiltinGitOps(dataGate, t.TempDir(), nil)
	reconciler := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle, Policy: policyEng})
	conclaveBroker := conclave.NewBroker(conclave.Options{Store: st, Reconciler: reconciler, GetBundle: func() *config.Bundle { return bundle }})
	dash, err := ui.NewDashboard(ui.Options{Store: st, Approvals: approvals, Reconciler: reconciler, Conclave: conclaveBroker, GetBundle: func() *config.Bundle { return bundle }})
	if err != nil {
		t.Fatalf("NewDashboard: %v", err)
	}

	attachHub := attach.NewHub()
	h := newHandler(st, secret.NewMemoryStore(nil), func() *config.Bundle { return bundle }, nil, modelGate, dataGate, approvals, reconciler, controlplane.NewScheduler(reconciler), conclaveBroker, attachHub, nil, dash)

	// 1. /healthz
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz expected 200, got %d", rec.Code)
	}

	// 2. /.well-known/agent-card.json excludes sandbox workload
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("agent-card expected 200, got %d", rec.Code)
	}
	cardBody := rec.Body.String()
	if !strings.Contains(cardBody, "triage-agent") || strings.Contains(cardBody, "experimental-sandbox") {
		t.Fatalf("unexpected agent card body: %s", cardBody)
	}

	// 3. POST /api/v1/tasks
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"workload":"triage-agent","idempotencyKey":"smoke-1","inputJson":"{}"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/tasks expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created domain.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode created task: %v (%s)", err, rec.Body.String())
	}

	// 4. GET /tasks/{id} detail page and GET /api/v1/tasks/{id}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks/"+created.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("task detail page failed: code=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+created.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("GET /api/v1/tasks/{id} failed: code=%d", rec.Code)
	}

	// 5. POST /api/v1/tasks/{id}/cancel
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+created.ID+"/cancel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel task failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6. GET /api/v1/budget
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/budget", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/budget failed: code=%d", rec.Code)
	}

	// 7. Web UI renders workloads and cancelled task
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "triage-agent") {
		t.Fatalf("Web UI failed: code=%d", rec.Code)
	}

	// 8. Conclave API open + Decree + Web UI /conclaves
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/conclaves", strings.NewReader(`{"template":"feature-conclave","title":"Smoke conclave","decree":"Begin triage"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/conclaves expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var sess domain.ConclaveSession
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil || sess.ID == "" {
		t.Fatalf("decode conclave session: %v (%s)", err, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conclaves/"+sess.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Smoke conclave") {
		t.Fatalf("GET /conclaves/{id} failed: code=%d", rec.Code)
	}
}

func TestOperatorAuthRequiredMiddleware(t *testing.T) {
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 1. Fail closed when operator_bearer_token is missing.
	emptyStore := secret.NewMemoryStore(nil)
	hEmpty := requireOperatorAuth(okHandler, emptyStore)
	rec := httptest.NewRecorder()
	hEmpty.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when token missing from store, got %d", rec.Code)
	}

	// 2. Configured token allows public/self-authenticated routes and enforces bearer on operator routes.
	sec := secret.NewMemoryStore(map[string]string{"operator_bearer_token": "op-secret-123"})
	h := requireOperatorAuth(okHandler, sec)

	for _, openPath := range []string{"/healthz", "/.well-known/agent-card.json", "/gate/model/v1/chat/completions", "/api/v1/hooks/github", "/a2a"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, openPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected %s to bypass operator auth, got %d", openPath, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without Authorization header, got %d", rec.Code)
	}

	reqWrong := httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
	reqWrong.Header.Set("Authorization", "Bearer wrong-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqWrong)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", rec.Code)
	}

	reqOK := httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
	reqOK.Header.Set("Authorization", "Bearer op-secret-123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid operator token, got %d", rec.Code)
	}
}
