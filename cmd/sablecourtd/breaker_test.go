package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

const breakerManifest = `apiVersion: sablecourt.io/v1alpha1
kind: Policy
metadata:
  name: breaker-policy
spec:
  defaultEffect: deny
  rules:
    - workload: pr-agent
      capability: action:git.open_pr
      effect: allow
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: pr-agent
  zone: runtime
spec:
  class: long-lived-agent
  runtime:
    type: docker
    image: alpine:3.21
  agentCard:
    description: Opens pull requests.
  grants:
    actions:
      - git.open_pr
`

// The daemon's policy engine and action gate must share one breaker that
// survives a manifest reload: three failed git.open_pr calls open it, and the
// fourth call is refused by policy even after a reload in between.
func TestDaemonBreakerSharedByPolicyAndGateAcrossReload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	manifests := filepath.Join(dir, "sablecourt.yaml")
	if err := os.WriteFile(manifests, []byte(breakerManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(dir, "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	loader := newPolicyLoader(st, manifests)
	bundle, pol, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "task-b", IdempotencyKey: "b", WorkloadName: "pr-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	token, err := model.MintTaskToken(ctx, st, "task-b", "pr-agent", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	gate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: pol})
	calls := 0
	gate.RegisterAction("git.open_pr", func(context.Context, domain.Workload, string, json.RawMessage) (any, error) {
		calls++
		return nil, errors.New("github unavailable")
	})
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(`{"payload":{}}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		gate.ActionHandler().ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < 2; i++ {
		if rec := call(); rec.Code != http.StatusBadGateway {
			t.Fatalf("call %d: want 502, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	bundle, pol, err = loader.Load(ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	gate.Reload(bundle, pol)
	if rec := call(); rec.Code != http.StatusBadGateway {
		t.Fatalf("call 3: want 502, got %d: %s", rec.Code, rec.Body.String())
	}

	rec := call()
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "circuit_breaker_open") {
		t.Fatalf("call 4: want 403 circuit_breaker_open, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls != 3 {
		t.Fatalf("action ran %d times, want 3", calls)
	}
}
