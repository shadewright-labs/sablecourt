package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// registeredRoutes builds the daemon handler with every optional part present
// and returns each route it registers as "METHOD /path". A pattern without a
// method serves any method and is returned as "ANY /path". The Model Gate
// mount is expanded into the provider paths it forwards.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
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
	var policies []domain.Policy
	for _, p := range bundle.Policies {
		policies = append(policies, p)
	}
	getBundle := func() *config.Bundle { return bundle }
	breaker := policy.NewCircuitBreaker(3, time.Minute)
	policyEng := policy.NewEngine(policies, breaker)
	approvals := approval.NewQueue(st, nil)
	modelGate := model.New(model.Options{Store: st, Secrets: secret.NewMemoryStore(nil), Bundle: bundle})
	dataGate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: policyEng, Breaker: breaker, Approvals: approvals})
	reconciler := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle, Policy: policyEng})
	broker := conclave.NewBroker(conclave.Options{Store: st, Reconciler: reconciler, GetBundle: getBundle})
	dash, err := ui.NewDashboard(ui.Options{Store: st, Approvals: approvals, Reconciler: reconciler, Conclave: broker, GetBundle: getBundle})
	if err != nil {
		t.Fatalf("NewDashboard: %v", err)
	}

	var patterns []string
	newHandler(st, defaultIntegrationSecrets(), getBundle, func() error { return nil }, modelGate, dataGate, approvals, reconciler,
		controlplane.NewScheduler(reconciler), broker, attach.NewHub(), nil, dash,
		handlerConfig{operatorToken: testOperatorToken, routeSink: func(p string) { patterns = append(patterns, p) }})

	var routes []string
	for _, p := range patterns {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			method, path = "ANY", p
		}
		if path == "/gate/model/" {
			for _, r := range model.Routes() {
				routes = append(routes, method+" /gate/model"+r)
			}
			continue
		}
		routes = append(routes, method+" "+path)
	}
	sort.Strings(routes)
	return routes
}

// documentedRoutes reads the first cell of every table row in the HTTP API
// reference whose table header starts with "Route".
func documentedRoutes(t *testing.T, page string) []string {
	t.Helper()
	f, err := os.Open(page) // #nosec G304 -- fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var routes []string
	inRouteTable := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "|") {
			inRouteTable = false
			continue
		}
		first := strings.TrimSpace(strings.Split(strings.Trim(line, "|"), "|")[0])
		switch {
		case first == "Route":
			inRouteTable = true
		case !inRouteTable || strings.HasPrefix(first, "---"):
		default:
			if !strings.HasPrefix(first, "`") || !strings.HasSuffix(first, "`") || strings.Count(first, "`") != 2 {
				t.Errorf("route cell %q must hold one route in backticks", first)
				continue
			}
			routes = append(routes, strings.Trim(first, "`"))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(routes)
	return routes
}

// TestHTTPReferenceListsEveryRoute fails when the route tables of
// docs/reference/http-api.md differ from the routes the daemon registers:
// a route missing from the page, a route on the page that the daemon does
// not register, a wrong method, or a route listed twice.
func TestHTTPReferenceListsEveryRoute(t *testing.T) {
	code := registeredRoutes(t)
	doc := documentedRoutes(t, filepath.Join("..", "..", "docs", "reference", "http-api.md"))
	if len(code) < 50 {
		t.Fatalf("only %d routes recorded; the recorder is not wired in", len(code))
	}
	count := func(list []string) map[string]int {
		m := map[string]int{}
		for _, r := range list {
			m[r]++
		}
		return m
	}
	inCode, inDoc := count(code), count(doc)
	for r, n := range inDoc {
		if n > 1 {
			t.Errorf("route listed %d times in http-api.md: %s", n, r)
		}
		if inCode[r] == 0 {
			t.Errorf("route in http-api.md but not registered by the daemon: %s", r)
		}
	}
	for r := range inCode {
		if inDoc[r] == 0 {
			t.Errorf("route registered by the daemon but missing from http-api.md: %s", r)
		}
	}
	if t.Failed() {
		got, _ := json.MarshalIndent(code, "", "  ")
		t.Logf("registered routes:\n%s", got)
	}
}
