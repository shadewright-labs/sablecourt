package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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

// referenceDaemon is the daemon handler with every optional part present,
// the routes it registers and what the probes below need.
type referenceDaemon struct {
	handler http.Handler
	// routes holds each registered route as "METHOD /path". A pattern
	// without a method is "ANY /path". The Model Gate mount is expanded into
	// the provider paths it forwards.
	routes []string
	// leaseToken is an active task lease of a declared workload that holds
	// a Model Gate grant for every provider.
	leaseToken string
	// upstream records the method of every request the Model Gate forwards.
	upstream *methodRecorder
}

type methodRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (m *methodRecorder) take() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.seen
	m.seen = nil
	return out
}

func newReferenceDaemon(t *testing.T) referenceDaemon {
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

	// Every provider upstream is a local server that records the method.
	rec := &methodRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, r.Method)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(upstream.Close)
	upstreams := map[string]model.UpstreamConfig{}
	for name := range model.DefaultUpstreams() {
		upstreams[name] = model.UpstreamConfig{BaseURL: upstream.URL}
	}

	// A working task of a declared workload, with a lease.
	const workload = "triage-agent"
	if _, ok := bundle.Workloads[workload]; !ok {
		t.Fatalf("examples/local-demo.yaml no longer declares %s", workload)
	}
	now := time.Now().UTC()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: workload, Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "reference-task", IdempotencyKey: "reference-task", WorkloadName: workload, State: domain.TaskStateWorking, InputJSON: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	token, err := model.MintTaskToken(ctx, st, "reference-task", workload, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	var policies []domain.Policy
	for _, p := range bundle.Policies {
		policies = append(policies, p)
	}
	getBundle := func() *config.Bundle { return bundle }
	breaker := policy.NewCircuitBreaker(3, time.Minute)
	policyEng := policy.NewEngine(policies, breaker)
	approvals := approval.NewQueue(st, nil)
	modelGate := model.New(model.Options{Store: st, Secrets: secret.NewMemoryStore(nil), Bundle: bundle, Upstreams: upstreams})
	dataGate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: policyEng, Breaker: breaker, Approvals: approvals})
	reconciler := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle, Policy: policyEng})
	broker := conclave.NewBroker(conclave.Options{Store: st, Reconciler: reconciler, GetBundle: getBundle})
	dash, err := ui.NewDashboard(ui.Options{Store: st, Approvals: approvals, Reconciler: reconciler, Conclave: broker, GetBundle: getBundle})
	if err != nil {
		t.Fatalf("NewDashboard: %v", err)
	}

	var patterns []string
	h := newHandler(st, defaultIntegrationSecrets(), getBundle, func() error { return nil }, modelGate, dataGate, approvals, reconciler,
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
	return referenceDaemon{handler: h, routes: routes, leaseToken: token, upstream: rec}
}

// Credential classes, one per "## " section of the page that holds a route
// table.
const (
	classPublic      = "public"
	classOperator    = "operator"
	classObserver    = "observer"
	classIntegration = "integration"
	classLease       = "task lease"
)

var sectionClass = map[string]string{
	"Public routes":                 classPublic,
	"Operator routes":               classOperator,
	"Observer routes":               classObserver,
	"Integration routes":            classIntegration,
	"Task-lease routes":             classLease,
	"Inbound integrations (parked)": classPublic,
}

// observerNote in the purpose of an operator route says that the observer
// credential also reaches it.
const observerNote = "Observer credential accepted."

type docRoute struct {
	class, purpose string
}

// documentedRoutes reads every table row of the HTTP API reference whose
// table header starts with "Route", with the credential class of the "## "
// section it sits in.
func documentedRoutes(t *testing.T, page string) (map[string]docRoute, []string) {
	t.Helper()
	f, err := os.Open(page) // #nosec G304 -- fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	routes := map[string]docRoute{}
	var order []string
	section := ""
	inRouteTable := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if s, ok := strings.CutPrefix(line, "## "); ok {
			section = s
		}
		if !strings.HasPrefix(line, "|") {
			inRouteTable = false
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		first := cells[0]
		switch {
		case first == "Route":
			inRouteTable = true
			if _, ok := sectionClass[section]; !ok {
				t.Errorf("route table under %q, which names no credential class", section)
			}
		case !inRouteTable || strings.HasPrefix(first, "---"):
		default:
			if !strings.HasPrefix(first, "`") || !strings.HasSuffix(first, "`") || strings.Count(first, "`") != 2 {
				t.Errorf("route cell %q must hold one route in backticks", first)
				continue
			}
			r := strings.Trim(first, "`")
			if _, dup := routes[r]; dup {
				t.Errorf("route listed twice in http-api.md: %s", r)
			}
			purpose := ""
			if len(cells) > 1 {
				purpose = cells[1]
			}
			routes[r] = docRoute{class: sectionClass[section], purpose: purpose}
			order = append(order, r)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return routes, order
}

// probePath turns a route pattern into a concrete path: a wildcard segment
// becomes "probe", {$} is dropped and a subtree pattern gets one more
// segment.
func probePath(pattern string) string {
	p := strings.ReplaceAll(pattern, "{$}", "")
	p = regexp.MustCompile(`\{[a-zA-Z]+\}`).ReplaceAllString(p, "probe")
	if strings.HasSuffix(p, "/") && p != "/" {
		p += "probe"
	}
	return p
}

func (d referenceDaemon) serve(method, path, token string) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(method, path, strings.NewReader("{}")).WithContext(ctx)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	d.handler.ServeHTTP(w, r)
	return w
}

func refusedByOperatorAuth(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusUnauthorized && strings.Contains(w.Body.String(), "operator credentials required")
}

// credentialClass finds the class of a route from how the handler treats
// requests: without a credential, with the observer token and with the
// integration token. Only the operator check, the observer check and the
// integration token check run before a handler refuses these requests, so
// no probe changes state.
func (d referenceDaemon) credentialClass(t *testing.T, method, path string) string {
	t.Helper()
	if method == "ANY" {
		method = http.MethodPost
	}
	if method == http.MethodPost {
		w := d.serve(method, path, testShadewrightToken)
		if !refusedByOperatorAuth(w) && !strings.Contains(w.Body.String(), "only valid on integration routes") {
			return classIntegration
		}
	}
	anon := d.serve(method, path, "")
	if refusedByOperatorAuth(anon) {
		if method == http.MethodGet && !refusedByOperatorAuth(d.serve(method, path, testObserverToken)) {
			return classObserver
		}
		return classOperator
	}
	if strings.HasPrefix(path, "/gate/") {
		if anon.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a lease: status %d, want 401 from the gate", method, path, anon.Code)
		}
		return classLease
	}
	return classPublic
}

var probeMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// acceptedMethods sends each probe method with the task lease to a route
// registered without a method and returns those not answered with 405.
// For the Model Gate it also requires the upstream to see the same method.
func (d referenceDaemon) acceptedMethods(t *testing.T, path string) string {
	t.Helper()
	var accepted []string
	for _, m := range probeMethods {
		w := d.serve(m, path, d.leaseToken)
		forwarded := d.upstream.take()
		if w.Code == http.StatusMethodNotAllowed {
			continue
		}
		accepted = append(accepted, m)
		if strings.HasPrefix(path, "/gate/model/") && (len(forwarded) != 1 || forwarded[0] != m) {
			t.Errorf("%s %s: status %d, upstream saw %v, want the method forwarded once", m, path, w.Code, forwarded)
		}
	}
	if len(accepted) == len(probeMethods) {
		return "any"
	}
	return "`" + strings.Join(accepted, "`, `") + "`"
}

var methodsNote = regexp.MustCompile(`Methods: ([^.]+)\.`)

// TestHTTPReferenceListsEveryRoute fails when the route tables of
// docs/reference/http-api.md differ from the routes the daemon registers:
// a route missing from the page, a route on the page that the daemon does
// not register, a wrong method, a route listed twice, a route in the table
// of another credential class than the one the handler applies, or, for a
// pattern registered without a method, a "Methods:" note that differs from
// the methods the handler behind it accepts.
func TestHTTPReferenceListsEveryRoute(t *testing.T) {
	d := newReferenceDaemon(t)
	code := d.routes
	doc, order := documentedRoutes(t, filepath.Join("..", "..", "docs", "reference", "http-api.md"))
	if len(code) < 50 {
		t.Fatalf("only %d routes recorded; the recorder is not wired in", len(code))
	}
	inCode := map[string]bool{}
	for _, r := range code {
		inCode[r] = true
		if _, ok := doc[r]; !ok {
			t.Errorf("route registered by the daemon but missing from http-api.md: %s", r)
		}
	}
	for _, r := range order {
		if !inCode[r] {
			t.Errorf("route in http-api.md but not registered by the daemon: %s", r)
		}
	}
	for _, r := range code {
		row, ok := doc[r]
		if !ok {
			continue
		}
		method, pattern, _ := strings.Cut(r, " ")
		path := probePath(pattern)
		got := d.credentialClass(t, method, path)
		switch {
		case got == classObserver && row.class == classOperator:
			if !strings.Contains(row.purpose, observerNote) {
				t.Errorf("%s: the observer credential reaches it; say %q or move it to Observer routes", r, observerNote)
			}
		case got != row.class:
			t.Errorf("%s: listed under %s routes, the handler treats it as %s", r, row.class, got)
		case strings.Contains(row.purpose, observerNote):
			t.Errorf("%s: says %q, but the observer credential does not reach it", r, observerNote)
		}
		m := methodsNote.FindStringSubmatch(row.purpose)
		if method != "ANY" {
			if m != nil {
				t.Errorf("%s: a \"Methods:\" note belongs only on a route registered without a method", r)
			}
			continue
		}
		want := d.acceptedMethods(t, path)
		switch {
		case m == nil:
			t.Errorf("%s: registered without a method; add \"Methods: %s.\" to the purpose", r, want)
		case m[1] != want:
			t.Errorf("%s: the page says \"Methods: %s.\", the handler accepts %s", r, m[1], want)
		}
	}
	if t.Failed() {
		got, _ := json.MarshalIndent(code, "", "  ")
		t.Logf("registered routes:\n%s", got)
	}
}
