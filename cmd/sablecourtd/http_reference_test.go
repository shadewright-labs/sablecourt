package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
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
	// upstream records every request the Model Gate forwards. It is the
	// only upstream: a fake on a loopback port.
	upstream *methodRecorder
}

// forwarded is one request that reached the fake upstream.
type forwarded struct {
	method, host string
}

type methodRecorder struct {
	mu sync.Mutex
	// host is the authority of the fake upstream.
	host string
	seen []forwarded
}

func (m *methodRecorder) take() []forwarded {
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

	// Every provider upstream is one fake server on a loopback port that
	// records the method and the authority of each request. The probes
	// reach no real provider: every default upstream is replaced below.
	rec := &methodRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, forwarded{method: r.Method, host: r.Host})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(upstreamURL.Hostname()); ip == nil || !ip.IsLoopback() {
		t.Fatalf("fake upstream %s is not on a loopback address", upstreamURL.Host)
	}
	rec.host = upstreamURL.Host
	upstreams := map[string]model.UpstreamConfig{}
	for name := range model.DefaultUpstreams() {
		upstreams[name] = model.UpstreamConfig{BaseURL: upstream.URL}
	}
	if len(upstreams) == 0 {
		t.Fatal("model.DefaultUpstreams is empty; nothing to replace with the fake")
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
	classPublic = "public"
	// classSelf is a public route whose handler checks its own credential
	// (a webhook signature, the A2A token) instead of the daemon's checks.
	classSelf        = "self-authenticated"
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
	"Inbound integrations (parked)": classSelf,
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

// probe is one request to the daemon handler.
type probe struct {
	method, path, token string
	// header and body, when set, replace the default JSON body "{}".
	header http.Header
	body   string
}

func (d referenceDaemon) do(p probe) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	body := p.body
	if body == "" {
		body = "{}"
	}
	r := httptest.NewRequest(p.method, p.path, strings.NewReader(body)).WithContext(ctx)
	for k, v := range p.header {
		r.Header[k] = v
	}
	if p.token != "" {
		r.Header.Set("Authorization", "Bearer "+p.token)
	}
	w := httptest.NewRecorder()
	d.handler.ServeHTTP(w, r)
	return w
}

func (d referenceDaemon) serve(method, path, token string) *httptest.ResponseRecorder {
	return d.do(probe{method: method, path: path, token: token})
}

func refusedByOperatorAuth(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusUnauthorized && strings.Contains(w.Body.String(), "operator credentials required")
}

// refused reports a refusal for a missing or wrong credential.
func refused(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden
}

// publicOwnRefusal holds the public routes that refuse a request with no
// credential for a reason of their own, with the error text of that
// refusal. POST /login wants a same-origin browser form and refuses
// anything else before it reads a token.
var publicOwnRefusal = map[string]string{
	"POST /login": "same-origin request required",
}

// wrongCredentials are bearer tokens that no route accepts, one of each
// shape the daemon tells apart: operator, task lease and anything else.
var wrongCredentials = []string{
	"op_" + strings.Repeat("0", 64),
	model.TokenPrefix + strings.Repeat("0", 64),
	"wrong-credential",
}

// credentialClass finds the class of a route from how the handler treats
// requests: first without a credential, then with the observer token and
// with the integration token. A route that answers a request with no
// credential is public whatever else it accepts. Only the operator check,
// the observer check, the gate's lease check and the integration token
// check run before a handler refuses these requests, so no probe changes
// state.
func (d referenceDaemon) credentialClass(t *testing.T, route, method, path string) string {
	t.Helper()
	if method == "ANY" {
		method = http.MethodPost
	}
	anon := d.serve(method, path, "")
	switch {
	case refusedByOperatorAuth(anon):
		if method == http.MethodGet && !refusedByOperatorAuth(d.serve(method, path, testObserverToken)) {
			return classObserver
		}
		return classOperator
	case !refused(anon):
		return classPublic
	case publicOwnRefusal[route] != "" && strings.Contains(anon.Body.String(), publicOwnRefusal[route]):
		return classPublic
	case strings.HasPrefix(path, "/gate/"):
		if anon.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a lease: status %d, want 401 from the gate", method, path, anon.Code)
		}
		return classLease
	case !refused(d.serve(method, path, testShadewrightToken)):
		return classIntegration
	}
	return classSelf
}

// checkCredential sends method to path without a credential and with each
// wrong credential. On a route of a class that needs a credential, every
// request must be refused with 401 or 403; on a public route the request
// with no credential must not be.
func (d referenceDaemon) checkCredential(t *testing.T, route, class, method, path string) {
	t.Helper()
	anon := d.serve(method, path, "")
	if class == classPublic {
		if refused(anon) && (publicOwnRefusal[route] == "" || !strings.Contains(anon.Body.String(), publicOwnRefusal[route])) {
			t.Errorf("%s: listed as public, but %s without a credential is refused with %d: %s", route, method, anon.Code, strings.TrimSpace(anon.Body.String()))
		}
		return
	}
	if !refused(anon) {
		t.Errorf("%s: listed under %s routes, but %s without a credential is answered %d; want 401 or 403", route, class, method, anon.Code)
	}
	for _, tok := range wrongCredentials {
		if w := d.serve(method, path, tok); !refused(w) {
			t.Errorf("%s: listed under %s routes, but %s with the wrong credential %q is answered %d; want 401 or 403", route, class, method, tok, w.Code)
		}
	}
}

// probeMethods stand for "any": the methods of RFC 9110 that a handler can
// serve (CONNECT and TRACE are left out: the router treats CONNECT apart
// and the daemon never serves TRACE) and one extension method.
var probeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions, "PROPFIND",
}

// mcpInitialize is a well-formed MCP initialize request.
const mcpInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"reference-probe","version":"0"}}}`

// methodAccepted sends method with the task lease to a route registered
// without a method. It returns true when the handler accepted the method,
// false when it refused the method with 405, and fails the test on any
// other answer, which is a refusal for another reason that says nothing
// about the method.
//
// What counts as accepted differs by handler, because no probe may run a
// real model call or a real action:
//   - Model Gate: the fake upstream saw exactly this one request with the
//     same method. This is a full acceptance.
//   - MCP: a well-formed initialize request is answered 200. A method the
//     SDK refuses is answered 405, so a 400 or 415 here is a probe defect.
//   - Actions: the request passed the method check and the lease check and
//     reached the catalog lookup, which refuses the unknown action name
//     "probe" with 404. The probe cannot show that a real action runs for a
//     method, because that needs an approval and makes a change.
//   - Any other handler: a 2xx answer.
func (d referenceDaemon) methodAccepted(t *testing.T, route, path, method string) bool {
	t.Helper()
	p := probe{method: method, path: path, token: d.leaseToken}
	if path == "/gate/mcp" {
		p.header = http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
		p.body = mcpInitialize
	}
	w := d.do(p)
	seen := d.upstream.take()
	if w.Code == http.StatusMethodNotAllowed {
		if len(seen) != 0 {
			t.Errorf("%s %s: refused with 405, but the upstream saw %v", route, method, seen)
		}
		return false
	}
	body := strings.TrimSpace(w.Body.String())
	switch {
	case strings.HasPrefix(path, "/gate/model/"):
		if w.Code == http.StatusOK && len(seen) == 1 && seen[0].method == method && seen[0].host == d.upstream.host {
			return true
		}
		t.Errorf("%s %s: status %d, the fake upstream at %s saw %v; want the method forwarded once to the fake", route, method, w.Code, d.upstream.host, seen)
	case path == "/gate/mcp":
		if w.Code == http.StatusOK {
			return true
		}
		t.Errorf("%s %s: status %d (%s); want 200 for an initialize request or 405", route, method, w.Code, body)
	case strings.HasPrefix(path, "/gate/actions/"):
		if w.Code == http.StatusNotFound && strings.Contains(body, "action not in closed catalog") {
			return true
		}
		t.Errorf("%s %s: status %d (%s); want 404 from the catalog lookup or 405", route, method, w.Code, body)
	default:
		if w.Code >= 200 && w.Code < 300 {
			return true
		}
		t.Errorf("%s %s: status %d (%s) is neither an acceptance nor a 405; teach methodAccepted how this handler accepts a request", route, method, w.Code, body)
	}
	if len(seen) != 0 {
		t.Errorf("%s %s: the upstream saw %v", route, method, seen)
	}
	return false
}

// acceptedMethods returns the probe methods a route registered without a
// method accepts, and its "Methods:" note: "any" when it accepts every
// probe method, else the methods in backticks.
func (d referenceDaemon) acceptedMethods(t *testing.T, route, path string) ([]string, string) {
	t.Helper()
	var accepted []string
	for _, m := range probeMethods {
		if d.methodAccepted(t, route, path, m) {
			accepted = append(accepted, m)
		}
	}
	if len(accepted) == len(probeMethods) {
		return accepted, "any"
	}
	return accepted, "`" + strings.Join(accepted, "`, `") + "`"
}

var methodsNote = regexp.MustCompile(`Methods: ([^.]+)\.`)

// handlerEnv lists the environment variables that the handlers, the Model
// Gate's client and the libraries behind them read while serving. The test
// clears them so that its result does not depend on the caller's shell.
var handlerEnv = []string{
	// The MCP SDK changes how the stateless handler answers.
	"MCPGODEBUG",
	"GODEBUG",
	// The voice routes.
	"SABLECOURT_STT_URL", "SABLECOURT_STT_MODEL", "SABLECOURT_TTS_URL", "SABLECOURT_TTS_MODEL", "SABLECOURT_TTS_VOICE",
	// The notify test route.
	"SABLECOURT_PUBLIC_URL", "SABLECOURT_NOTIFY_WEBHOOK_URL",
	"SABLECOURT_STATE_DIR",
	"DOCKER_HOST",
	// Outbound HTTP, including the Model Gate's requests to the fake.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
}

// mcpDebugEnv is read by the MCP SDK once, when the test binary starts, so
// clearing it with t.Setenv comes too late.
const mcpDebugEnv = "MCPGODEBUG"

// rerunWithoutMCPDebug runs TestHTTPReferenceListsEveryRoute again in a new
// test process whose environment lacks MCPGODEBUG, and reports its result.
func rerunWithoutMCPDebug(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHTTPReferenceListsEveryRoute$", "-test.count=1") // #nosec G204 -- this test binary with fixed arguments
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, mcpDebugEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run again without %s: %v\n%s", mcpDebugEnv, err, out)
	}
}

// TestHTTPReferenceListsEveryRoute fails when the route tables of
// docs/reference/http-api.md differ from the routes the daemon registers:
// a route missing from the page, a route on the page that the daemon does
// not register, a wrong method, a route listed twice, a route in the table
// of another credential class than the one the handler applies, a route in
// a table that needs a credential that answers a request with no credential
// or with a wrong one, a public route that refuses a request with no
// credential, or, for a pattern registered without a method, a "Methods:"
// note that differs from the methods the handler behind it accepts.
//
// The probes write audit and budget rows only to a temporary SQLite store
// and reach only the fake loopback upstream.
func TestHTTPReferenceListsEveryRoute(t *testing.T) {
	if v := os.Getenv(mcpDebugEnv); v != "" {
		rerunWithoutMCPDebug(t)
		return
	}
	for _, k := range handlerEnv {
		t.Setenv(k, "")
	}
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
		got := d.credentialClass(t, r, method, path)
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
			d.checkCredential(t, r, row.class, method, path)
			continue
		}
		accepted, want := d.acceptedMethods(t, r, path)
		switch {
		case m == nil:
			t.Errorf("%s: registered without a method; add \"Methods: %s.\" to the purpose", r, want)
		case m[1] != want:
			t.Errorf("%s: the page says \"Methods: %s.\", the handler accepts %s", r, m[1], want)
		}
		if len(accepted) == 0 {
			t.Errorf("%s: accepts none of %v", r, probeMethods)
		}
		for _, am := range accepted {
			d.checkCredential(t, r, row.class, am, path)
		}
		if seen := d.upstream.take(); len(seen) != 0 {
			t.Errorf("%s: a request without a valid lease reached the upstream: %v", r, seen)
		}
	}
	if t.Failed() {
		got, _ := json.MarshalIndent(code, "", "  ")
		t.Logf("registered routes:\n%s", got)
	}
}
