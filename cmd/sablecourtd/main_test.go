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
	dash, err := ui.NewDashboard(ui.Options{Store: st, Approvals: approvals, Reconciler: reconciler})
	if err != nil {
		t.Fatalf("NewDashboard: %v", err)
	}

	h := newHandler(st, secret.NewMemoryStore(nil), func() *config.Bundle { return bundle }, nil, modelGate, dataGate, approvals, reconciler, nil, dash)

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
}
