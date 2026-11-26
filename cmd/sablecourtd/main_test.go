package main

import (
	"context"
	"encoding/json"
	"io"
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
	"github.com/shadewright-labs/sablecourt/internal/store"
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
	h := newHandler(st, defaultIntegrationSecrets(), func() *config.Bundle { return bundle }, nil, modelGate, dataGate, approvals, reconciler, controlplane.NewScheduler(reconciler), conclaveBroker, attachHub, nil, dash, handlerConfig{operatorToken: testOperatorToken})

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
	h.ServeHTTP(rec, operatorRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"workload":"triage-agent","idempotencyKey":"smoke-1","inputJson":"{}"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/tasks expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created domain.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode created task: %v (%s)", err, rec.Body.String())
	}

	// 4. GET /tasks/{id} detail page and GET /api/v1/tasks/{id}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(http.MethodGet, "/tasks/"+created.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("task detail page failed: code=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/tasks/"+created.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("GET /api/v1/tasks/{id} failed: code=%d", rec.Code)
	}

	// 5. POST /api/v1/tasks/{id}/cancel
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(http.MethodPost, "/api/v1/tasks/"+created.ID+"/cancel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel task failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6. GET /api/v1/budget
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/budget", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/budget failed: code=%d", rec.Code)
	}

	// 7. Web UI renders workloads and cancelled task
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "triage-agent") {
		t.Fatalf("Web UI failed: code=%d", rec.Code)
	}

	// 8. Conclave API open + Decree + Web UI /conclaves
	rec = doBearerPOST(h, "/api/v1/conclaves", testOperatorToken,
		map[string]string{"template": "feature-conclave", "title": "Smoke conclave", "decree": "Begin triage"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/conclaves expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var sess domain.ConclaveSession
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil || sess.ID == "" {
		t.Fatalf("decode conclave session: %v (%s)", err, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(http.MethodGet, "/conclaves/"+sess.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Smoke conclave") {
		t.Fatalf("GET /conclaves/{id} failed: code=%d", rec.Code)
	}
}

func operatorRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+testOperatorToken)
	return req
}

func TestOperatorAuthRequiredMiddleware(t *testing.T) {
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux := http.NewServeMux()
	for _, pattern := range []string{
		"GET /healthz", "GET /.well-known/agent-card.json", "/gate/model/",
		"POST /api/v1/hooks/github", "POST /a2a", "GET /api/v1/workloads", "POST /api/v1/tasks",
		"GET /api/v1/observer/snapshot", "GET /api/v1/tasks/{id}/scry",
	} {
		mux.Handle(pattern, okHandler)
	}

	hEmpty := newOperatorAuth("").requireOperatorAuth(mux, mux, nil)
	rec := httptest.NewRecorder()
	hEmpty.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when token missing from store, got %d", rec.Code)
	}

	// 2. Configured token allows public/self-authenticated routes and enforces bearer on operator routes.
	auth := newOperatorAuth(testOperatorToken)
	h := auth.requireOperatorAuth(mux, mux, nil)

	for _, openPath := range []string{"/healthz", "/.well-known/agent-card.json", "/gate/model/v1/chat/completions", "/api/v1/hooks/github", "/a2a"} {
		method := http.MethodGet
		if openPath == "/api/v1/hooks/github" || openPath == "/a2a" {
			method = http.MethodPost
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, openPath, nil))
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
	reqOK.Header.Set("Authorization", "Bearer "+testOperatorToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid operator token, got %d", rec.Code)
	}

	// 3. Read-only observer_bearer_token can access observer snapshot and scry, but is rejected on operator routes.
	secWithObs := secret.NewMemoryStore(map[string]string{
		"operator_bearer_token": testOperatorToken,
		"observer_bearer_token": "obs-readonly-456",
	})
	hObs := auth.requireOperatorAuth(mux, mux, secWithObs)

	for _, allowed := range []string{"/api/v1/observer/snapshot", "/api/v1/tasks/t1/scry"} {
		req := httptest.NewRequest(http.MethodGet, allowed, nil)
		req.Header.Set("Authorization", "Bearer obs-readonly-456")
		rec = httptest.NewRecorder()
		hObs.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected observer token to access %s, got %d", allowed, rec.Code)
		}
	}
	reqDenied := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", nil)
	reqDenied.Header.Set("Authorization", "Bearer obs-readonly-456")
	rec = httptest.NewRecorder()
	hObs.ServeHTTP(rec, reqDenied)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected observer token to be denied on POST /api/v1/tasks, got %d", rec.Code)
	}
}

func TestSecurityHardeningCSRFAndHeaders(t *testing.T) {
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := securityHeadersMiddleware(guardOperatorRequest(okHandler))

	// 1. Security headers present on GET.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing security headers: %+v", rec.Header())
	}

	// 2. Cross-site Sec-Fetch-Site POST blocked.
	reqXS := httptest.NewRequest(http.MethodPost, "/ui/tasks", nil)
	reqXS.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqXS)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-site POST, got %d", rec.Code)
	}

	// 3. Mismatched Origin POST blocked.
	reqBadOrigin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7432/api/v1/tasks", nil)
	reqBadOrigin.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqBadOrigin)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for mismatched Origin POST, got %d", rec.Code)
	}

	// 4. Same-origin POST allowed.
	reqSame := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7432/api/v1/tasks", nil)
	reqSame.Header.Set("Origin", "http://127.0.0.1:7432")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqSame)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for same-origin POST, got %d", rec.Code)
	}
}

func TestReservedMemoryRoutes(t *testing.T) {
	st, _, _, h, _ := newIntegrationTestEnv(t, domain.ConclaveWards{}, defaultIntegrationSecrets())
	const ns = "learning:global"
	for _, tc := range []struct {
		name, method, path, body string
		form, read               bool
	}{
		{"api search", "GET", "/api/v1/memory", "", false, true},
		{"api exact search", "GET", "/api/v1/memory?namespace=learning:global", "", false, true},
		{"ui search", "GET", "/memory", "", false, true},
		{"api put", "POST", "/api/v1/memory", `{"namespace":"LEARNING:Global","key":"write","value":"changed"}`, false, false},
		{"api delete", "DELETE", "/api/v1/memory/learning:global/key", "", false, false},
		{"api publish", "POST", "/api/v1/learnings", `{"scope":"global","key":"write","rule":"changed","author":"spoof"}`, false, false},
		{"ui put", "POST", "/ui/memory/set", "scope=learning:global&key=write&value=changed", true, false},
		{"ui delete", "POST", "/ui/memory/delete", "scope=learning:global&key=key", true, false},
		{"ui publish", "POST", "/ui/memory/learning", "scope=global&key=write&rule=changed", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.PutMemory(t.Context(), domain.MemoryEntry{Namespace: ns, Key: "key", Value: "private-legacy-value"}); err != nil {
				t.Fatal(err)
			}
			req := operatorRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.form {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if tc.read {
				if rec.Code != 200 || strings.Contains(rec.Body.String(), "private-legacy-value") {
					t.Errorf("learning visible or status %d", rec.Code)
				}
				return
			}
			if rec.Code < 400 && !(rec.Code == 303 && strings.Contains(rec.Header().Get("Location"), "err=")) {
				t.Errorf("write returned %d", rec.Code)
			}
			if _, err := st.GetMemory(t.Context(), ns, "key"); err != nil {
				t.Errorf("original removed: %v", err)
			}
		})
	}
}

func TestLearningOperatorBoundary(t *testing.T) {
	auth := newOperatorAuth(testOperatorToken)
	mux := http.NewServeMux()
	probe := func(w http.ResponseWriter, r *http.Request) {
		id, _ := store.LearningOperator(r.Context())
		_, _ = io.WriteString(w, id)
	}
	mux.HandleFunc("GET /memory", probe)
	mux.HandleFunc("GET /healthz", probe)
	mux.HandleFunc("GET /api/v1/observer/snapshot", probe)
	h := auth.requireOperatorAuth(mux, mux, defaultIntegrationSecrets())
	for _, tc := range []struct {
		name, path, token, want string
		cookie                  bool
	}{
		{"bearer", "/memory", testOperatorToken, "operator", false},
		{"cookie", "/memory", "", "operator", true},
		{"observer", "/api/v1/observer/snapshot", testObserverToken, "", false},
		{"public", "/healthz", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: operatorCookie, Value: auth.sessionValue(req.Host, time.Now().Add(time.Minute).Unix())})
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 || rec.Body.String() != tc.want {
				t.Errorf("status %d, identity %q, want %q", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
