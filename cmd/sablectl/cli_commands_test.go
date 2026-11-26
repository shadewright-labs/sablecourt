package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// cliRequest is one request the CLI sent to the in-process daemon.
type cliRequest struct {
	Method string
	Path   string // escaped path
	Query  string
	Body   string
}

// cliDaemon records every request and answers from a route table keyed by
// "METHOD /escaped/path". Unknown routes answer 404 so a wrong path fails the test.
type cliDaemon struct {
	mu     sync.Mutex
	reqs   []cliRequest
	routes map[string]func(w http.ResponseWriter, r *http.Request)
}

func newCLIDaemon() *cliDaemon {
	return &cliDaemon{routes: map[string]func(http.ResponseWriter, *http.Request){}}
}

func (d *cliDaemon) on(route string, status int, body string) *cliDaemon {
	d.routes[route] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	return d
}

func (d *cliDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	d.reqs = append(d.reqs, cliRequest{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Body: string(raw)})
	h := d.routes[r.Method+" "+r.URL.EscapedPath()]
	d.mu.Unlock()
	if h == nil {
		http.Error(w, "unexpected route "+r.Method+" "+r.URL.EscapedPath(), http.StatusNotFound)
		return
	}
	h(w, r)
}

func (d *cliDaemon) requests() []cliRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]cliRequest(nil), d.reqs...)
}

// runCLI runs sablectl with the given arguments against handler and returns stdout, stderr and the error.
func runCLI(t *testing.T, handler http.Handler, stdin string, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("SABLECOURT_OPERATOR_TOKEN", "")
	root := newRootCmdWithTransport(&handlerRoundTripper{handler: handler})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--api", "http://unix"}, args...))
	err := root.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func jsonEqual(t *testing.T, got, want string) bool {
	t.Helper()
	if want == "" {
		return true
	}
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Errorf("request body %q is not JSON: %v", got, err)
		return false
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON %q: %v", want, err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	return bytes.Equal(gb, wb)
}

func TestMutatingCommandsSendExactRequest(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		stdin     string
		route     string
		respBody  string
		wantQuery string
		wantBody  string
		wantOut   []string
	}{
		{
			name:     "approve escapes id and pins digest",
			args:     []string{"approval", "approve", "apr/1", "--digest", "abc123"},
			route:    "POST /api/v1/approvals/apr%2F1/approve",
			respBody: `{}`,
			wantBody: `{"expectedSha256":"abc123"}`,
			wantOut:  []string{"approved apr/1"},
		},
		{
			name:     "approve without digest sends empty digest",
			args:     []string{"approval", "approve", "apr1"},
			route:    "POST /api/v1/approvals/apr1/approve",
			respBody: `{}`,
			wantBody: `{"expectedSha256":""}`,
			wantOut:  []string{"approved apr1"},
		},
		{
			name:     "deny sends reason",
			args:     []string{"approval", "deny", "apr1", "--reason", "too risky"},
			route:    "POST /api/v1/approvals/apr1/deny",
			respBody: `{}`,
			wantBody: `{"reason":"too risky"}`,
			wantOut:  []string{"denied apr1"},
		},
		{
			name:     "device revoke",
			args:     []string{"device", "revoke", "dev 1"},
			route:    "POST /api/v1/devices/dev%201/revoke",
			respBody: `{}`,
			wantBody: `{}`,
			wantOut:  []string{"revoked device dev 1"},
		},
		{
			name:     "pair prints code uri and expiry",
			args:     []string{"pair"},
			route:    "POST /api/v1/devices/pair/start",
			respBody: `{"code":"K7Q2P9","uri":"sablecourt://pair?code=K7Q2P9","expiresAt":"2026-11-26T10:00:00Z"}`,
			wantBody: `{}`,
			wantOut:  []string{"pairing code: K7Q2P9", "URI: sablecourt://pair?code=K7Q2P9", "Expires: 2026-11-26T10:00:00Z"},
		},
		{
			name:     "task cancel",
			args:     []string{"task", "cancel", "t/1"},
			route:    "POST /api/v1/tasks/t%2F1/cancel",
			respBody: `{}`,
			wantBody: `{}`,
			wantOut:  []string{"cancelled task t/1"},
		},
		{
			name:     "task suspend",
			args:     []string{"task", "suspend", "t1"},
			route:    "POST /api/v1/tasks/t1/suspend",
			respBody: `{}`,
			wantBody: `{}`,
			wantOut:  []string{"suspended task t1"},
		},
		{
			name:     "task resume",
			args:     []string{"task", "resume", "t1"},
			route:    "POST /api/v1/tasks/t1/resume",
			respBody: `{}`,
			wantBody: `{}`,
			wantOut:  []string{"resumed task t1"},
		},
		{
			name:     "task retry names the new task",
			args:     []string{"task", "retry", "t1"},
			route:    "POST /api/v1/tasks/t1/retry",
			respBody: `{"ID":"t2","WorkloadName":"coder"}`,
			wantBody: `{}`,
			wantOut:  []string{"retried t1 as t2 (coder)"},
		},
		{
			name:      "task retry with handoff",
			args:      []string{"task", "retry", "t1", "--handoff"},
			route:     "POST /api/v1/tasks/t1/retry",
			respBody:  `{"ID":"t3","WorkloadName":"coder"}`,
			wantQuery: "handoff=true",
			wantBody:  `{}`,
			wantOut:   []string{"retried t1 as t3 (coder) with failure-handoff.md"},
		},
		{
			name:     "task retry without task in response",
			args:     []string{"task", "retry", "t1"},
			route:    "POST /api/v1/tasks/t1/retry",
			respBody: `{"status":"queued"}`,
			wantBody: `{}`,
			wantOut:  []string{"retried task t1"},
		},
		{
			name:     "task submit with JSON input provider and model",
			args:     []string{"task", "submit", "coder", "--input", `{"prompt":"fix it"}`, "--provider", "anthropic", "--model", "m1", "--idempotency-key", "k1"},
			route:    "POST /api/v1/tasks",
			respBody: `{"ID":"t9","WorkloadName":"coder"}`,
			wantBody: `{"workload":"coder","idempotencyKey":"k1","inputJson":"{\"model\":\"m1\",\"prompt\":\"fix it\",\"provider\":\"anthropic\"}"}`,
			wantOut:  []string{"submitted task t9 for coder"},
		},
		{
			name:     "task submit wraps plain text as prompt",
			args:     []string{"task", "submit", "coder", "--input", "fix the build"},
			route:    "POST /api/v1/tasks",
			respBody: `{"ID":"t10","WorkloadName":"coder"}`,
			wantBody: `{"workload":"coder","idempotencyKey":"","inputJson":"{\"prompt\":\"fix the build\"}"}`,
			wantOut:  []string{"submitted task t10 for coder"},
		},
		{
			name:     "apply reloads manifests",
			args:     []string{"apply"},
			route:    "POST /api/v1/reload",
			respBody: `{}`,
			wantBody: `{}`,
			wantOut:  []string{"manifests reloaded"},
		},
		{
			name:     "promote prints branch and next step",
			args:     []string{"promote", "coder"},
			route:    "POST /api/v1/promote/coder",
			respBody: `{"branch":"promote/coder","next":"open a pull request"}`,
			wantBody: `{}`,
			wantOut:  []string{"created promotion branch promote/coder", "open a pull request"},
		},
		{
			name:     "provider sync counts models",
			args:     []string{"provider", "sync", "openai"},
			route:    "POST /api/v1/providers/openai/sync",
			respBody: `[{"id":"a"},{"id":"b"}]`,
			wantBody: `{}`,
			wantOut:  []string{"synced 2 models from openai"},
		},
		{
			name:     "conclave open",
			args:     []string{"conclave", "open", "review", "--title", "Review X", "--decree", "start"},
			route:    "POST /api/v1/conclaves",
			respBody: `{"ID":"c1","Title":"Review X"}`,
			wantBody: `{"template":"review","title":"Review X","decree":"start"}`,
			wantOut:  []string{"convened conclave c1 (Review X)"},
		},
		{
			name:     "conclave decree defaults to all",
			args:     []string{"conclave", "decree", "c1", "focus on tests"},
			route:    "POST /api/v1/conclaves/c1/decree",
			respBody: `{}`,
			wantBody: `{"to":"all","body":"focus on tests"}`,
			wantOut:  []string{"decree issued to @all in c1"},
		},
		{
			name:     "relic put from flag",
			args:     []string{"conclave", "relic", "put", "c1", "plan", "--body", "step one", "--type", "text/markdown"},
			route:    "POST /api/v1/conclaves/c1/relics",
			respBody: `{}`,
			wantBody: `{"name":"plan","contentType":"text/markdown","body":"step one"}`,
			wantOut:  []string{"stored relic plan in c1"},
		},
		{
			name:     "relic put from stdin",
			args:     []string{"conclave", "relic", "put", "c1", "notes"},
			stdin:    "from stdin\n",
			route:    "POST /api/v1/conclaves/c1/relics",
			respBody: `{}`,
			wantBody: `{"name":"notes","contentType":"text/plain","body":"from stdin\n"}`,
			wantOut:  []string{"stored relic notes in c1"},
		},
		{
			name:     "memory set",
			args:     []string{"memory", "set", "ns", "k", "v"},
			route:    "POST /api/v1/memory",
			respBody: `{}`,
			wantBody: `{"namespace":"ns","key":"k","value":"v"}`,
			wantOut:  []string{"saved memory ns/k"},
		},
		{
			name:    "memory delete escapes both parts",
			args:    []string{"memory", "delete", "n/s", "k y"},
			route:   "DELETE /api/v1/memory/n%2Fs/k%20y",
			wantOut: []string{"deleted memory n/s/k y"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newCLIDaemon().on(tc.route, http.StatusOK, tc.respBody)
			out, _, err := runCLI(t, d, tc.stdin, tc.args...)
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}
			reqs := d.requests()
			if len(reqs) != 1 {
				t.Fatalf("sent %d requests, want 1: %+v", len(reqs), reqs)
			}
			got := reqs[0]
			if got.Method+" "+got.Path != tc.route {
				t.Errorf("request = %s %s, want %s", got.Method, got.Path, tc.route)
			}
			if got.Query != tc.wantQuery {
				t.Errorf("query = %q, want %q", got.Query, tc.wantQuery)
			}
			if !jsonEqual(t, got.Body, tc.wantBody) {
				t.Errorf("body = %s, want %s", got.Body, tc.wantBody)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out, w) {
					t.Errorf("output %q does not contain %q", out, w)
				}
			}
		})
	}
}

func TestDaemonErrorStatusIsReturnedAndNothingPrinted(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		route  string
		status int
		body   string
		want   string
	}{
		{"approve refused by daemon", []string{"approval", "approve", "a1", "--digest", "bad"}, "POST /api/v1/approvals/a1/approve", http.StatusConflict, `{"error":"digest mismatch"}` + "\n", `HTTP 409: {"error":"digest mismatch"}`},
		{"deny on decided approval", []string{"approval", "deny", "a1"}, "POST /api/v1/approvals/a1/deny", http.StatusGone, `{"error":"already decided"}`, "HTTP 410"},
		{"revoke unknown device", []string{"device", "revoke", "d1"}, "POST /api/v1/devices/d1/revoke", http.StatusNotFound, `{"error":"no such device"}`, "HTTP 404"},
		{"pair unauthorized", []string{"pair"}, "POST /api/v1/devices/pair/start", http.StatusUnauthorized, `{"error":"operator credential required"}`, "HTTP 401: {\"error\":\"operator credential required\"}"},
		{"submit over budget", []string{"task", "submit", "coder"}, "POST /api/v1/tasks", http.StatusForbidden, `{"error":"budget exhausted"}`, "HTTP 403"},
		{"cancel", []string{"task", "cancel", "t1"}, "POST /api/v1/tasks/t1/cancel", http.StatusConflict, "x", "HTTP 409: x"},
		{"suspend", []string{"task", "suspend", "t1"}, "POST /api/v1/tasks/t1/suspend", http.StatusConflict, "x", "HTTP 409"},
		{"resume", []string{"task", "resume", "t1"}, "POST /api/v1/tasks/t1/resume", http.StatusConflict, "x", "HTTP 409"},
		{"retry", []string{"task", "retry", "t1"}, "POST /api/v1/tasks/t1/retry", http.StatusConflict, "x", "HTTP 409"},
		{"apply", []string{"apply"}, "POST /api/v1/reload", http.StatusUnprocessableEntity, "bad manifest", "HTTP 422: bad manifest"},
		{"promote", []string{"promote", "w"}, "POST /api/v1/promote/w", http.StatusForbidden, "sandbox only", "HTTP 403"},
		{"provider sync", []string{"provider", "sync", "p"}, "POST /api/v1/providers/p/sync", http.StatusBadGateway, "upstream", "HTTP 502"},
		{"conclave open", []string{"conclave", "open", "t"}, "POST /api/v1/conclaves", http.StatusBadRequest, "no template", "HTTP 400"},
		{"conclave decree", []string{"conclave", "decree", "c1", "m"}, "POST /api/v1/conclaves/c1/decree", http.StatusConflict, "closed", "HTTP 409"},
		{"relic put", []string{"conclave", "relic", "put", "c1", "n", "--body", "b"}, "POST /api/v1/conclaves/c1/relics", http.StatusForbidden, "denied", "HTTP 403"},
		{"memory set", []string{"memory", "set", "a", "b", "c"}, "POST /api/v1/memory", http.StatusBadRequest, "bad", "HTTP 400"},
		{"memory delete", []string{"memory", "delete", "a", "b"}, "DELETE /api/v1/memory/a/b", http.StatusNotFound, " missing \n", "HTTP 404: missing"},
		{"workload list", []string{"workload", "list"}, "GET /api/v1/workloads", http.StatusInternalServerError, "boom", "HTTP 500: boom"},
		{"task list", []string{"task", "list"}, "GET /api/v1/tasks", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"task inspect", []string{"task", "inspect", "t1"}, "GET /api/v1/tasks/t1", http.StatusNotFound, "no task", "HTTP 404"},
		{"budget status", []string{"budget", "status"}, "GET /api/v1/budget", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"audit tail", []string{"audit", "tail"}, "GET /api/v1/audit", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"audit verify broken chain", []string{"audit", "verify"}, "GET /api/v1/audit/verify", http.StatusConflict, `{"valid":false,"verifiedRows":3,"error":"row 4 hash mismatch"}`, "row 4 hash mismatch"},
		{"audit export", []string{"audit", "export"}, "GET /api/v1/audit/export", http.StatusInternalServerError, " store down ", "HTTP 500: store down"},
		{"approval list", []string{"approval", "list"}, "GET /api/v1/approvals", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"provider list", []string{"provider", "list"}, "GET /api/v1/providers", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"device list", []string{"device", "list"}, "GET /api/v1/devices", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"conclave list", []string{"conclave", "list"}, "GET /api/v1/conclaves", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"conclave inspect", []string{"conclave", "inspect", "c1"}, "GET /api/v1/conclaves/c1", http.StatusNotFound, "none", "HTTP 404"},
		{"relic get", []string{"conclave", "relic", "get", "c1", "n"}, "GET /api/v1/conclaves/c1/relics/n", http.StatusNotFound, "none", "HTTP 404"},
		{"voice status", []string{"voice", "status"}, "GET /api/v1/voice/status", http.StatusServiceUnavailable, "off", "HTTP 503"},
		{"voice speak", []string{"voice", "speak", "hi"}, "POST /api/v1/voice/speak", http.StatusServiceUnavailable, "off", "HTTP 503"},
		{"memory list", []string{"memory", "list"}, "GET /api/v1/memory", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"task diff", []string{"task", "diff", "t1"}, "GET /api/v1/tasks/t1", http.StatusNotFound, "none", "HTTP 404"},
		{"task scry", []string{"task", "scry", "t1"}, "GET /api/v1/tasks/t1/scry", http.StatusForbidden, " not yours \n", "HTTP 403: not yours"},
		{"takeover refused", []string{"task", "takeover", "t1"}, "POST /api/v1/tasks/t1/takeover", http.StatusConflict, "held by another operator", "HTTP 409: held by another operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newCLIDaemon().on(tc.route, tc.status, tc.body)
			out, _, err := runCLI(t, d, "", tc.args...)
			if err == nil {
				t.Fatalf("want error containing %q, got success with output %q", tc.want, out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(out, "✓") || strings.Contains(out, "valid (") {
				t.Errorf("success output printed despite error: %q", out)
			}
			reqs := d.requests()
			if len(reqs) == 0 || reqs[0].Method+" "+reqs[0].Path != tc.route {
				t.Errorf("requests = %+v, want first %s", reqs, tc.route)
			}
			if tc.name == "takeover refused" && len(reqs) != 1 {
				t.Errorf("takeover refusal sent %d requests, want no detach or stream: %+v", len(reqs), reqs)
			}
		})
	}
}

func TestArgumentValidationSendsNoRequest(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"approve without id", []string{"approval", "approve"}, "accepts 1 arg(s)"},
		{"approve with two ids", []string{"approval", "approve", "a", "b"}, "accepts 1 arg(s)"},
		{"deny without id", []string{"approval", "deny"}, "accepts 1 arg(s)"},
		{"revoke without id", []string{"device", "revoke"}, "accepts 1 arg(s)"},
		{"cancel without id", []string{"task", "cancel"}, "accepts 1 arg(s)"},
		{"submit without workload", []string{"task", "submit"}, "accepts 1 arg(s)"},
		{"decree without message", []string{"conclave", "decree", "c1"}, "accepts 2 arg(s)"},
		{"memory set missing value", []string{"memory", "set", "ns", "k"}, "accepts 3 arg(s)"},
		{"memory delete missing key", []string{"memory", "delete", "ns"}, "accepts 2 arg(s)"},
		{"relic get missing name", []string{"conclave", "relic", "get", "c1"}, "accepts 2 arg(s)"},
		{"unknown digest flag spelling", []string{"approval", "approve", "a1", "--sha", "x"}, "unknown flag"},
		{"invalid api url", []string{"--api", "daemon-without-scheme", "approval", "approve", "a1"}, "invalid daemon API URL"},
		{"api url with user info", []string{"--api", "http://user:pass@example.com", "approval", "deny", "a1"}, "invalid daemon API URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newCLIDaemon()
			_, _, err := runCLI(t, d, "", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if n := len(d.requests()); n != 0 {
				t.Fatalf("sent %d requests on invalid input, want 0", n)
			}
		})
	}
}

func TestRemotePlaintextDaemonRefusesExplicitCredential(t *testing.T) {
	d := newCLIDaemon().on("POST /api/v1/approvals/a1/approve", http.StatusOK, `{}`)
	root := newRootCmdWithTransport(&handlerRoundTripper{handler: d})
	t.Setenv("SABLECOURT_OPERATOR_TOKEN", "op_"+strings.Repeat("d", 64))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--api", "http://daemon.invalid", "approval", "approve", "a1"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "operator credentials require HTTPS or a local daemon") {
		t.Fatalf("error = %v, want HTTPS refusal", err)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("credential request reached the daemon %d times", n)
	}
}

func TestJSONFlagPrintsRawDaemonBody(t *testing.T) {
	cases := []struct {
		args  []string
		route string
	}{
		{[]string{"approval", "approve", "a1"}, "POST /api/v1/approvals/a1/approve"},
		{[]string{"approval", "deny", "a1"}, "POST /api/v1/approvals/a1/deny"},
		{[]string{"approval", "list"}, "GET /api/v1/approvals"},
		{[]string{"device", "revoke", "d1"}, "POST /api/v1/devices/d1/revoke"},
		{[]string{"device", "list"}, "GET /api/v1/devices"},
		{[]string{"pair"}, "POST /api/v1/devices/pair/start"},
		{[]string{"task", "submit", "w"}, "POST /api/v1/tasks"},
		{[]string{"task", "list"}, "GET /api/v1/tasks"},
		{[]string{"task", "inspect", "t1"}, "GET /api/v1/tasks/t1"},
		{[]string{"task", "cancel", "t1"}, "POST /api/v1/tasks/t1/cancel"},
		{[]string{"task", "suspend", "t1"}, "POST /api/v1/tasks/t1/suspend"},
		{[]string{"task", "resume", "t1"}, "POST /api/v1/tasks/t1/resume"},
		{[]string{"task", "retry", "t1"}, "POST /api/v1/tasks/t1/retry"},
		{[]string{"workload", "list"}, "GET /api/v1/workloads"},
		{[]string{"budget", "status"}, "GET /api/v1/budget"},
		{[]string{"apply"}, "POST /api/v1/reload"},
		{[]string{"audit", "tail"}, "GET /api/v1/audit"},
		{[]string{"audit", "verify"}, "GET /api/v1/audit/verify"},
		{[]string{"provider", "list"}, "GET /api/v1/providers"},
		{[]string{"provider", "sync", "p"}, "POST /api/v1/providers/p/sync"},
		{[]string{"promote", "w"}, "POST /api/v1/promote/w"},
		{[]string{"conclave", "list"}, "GET /api/v1/conclaves"},
		{[]string{"conclave", "open", "t"}, "POST /api/v1/conclaves"},
		{[]string{"conclave", "decree", "c1", "m"}, "POST /api/v1/conclaves/c1/decree"},
		{[]string{"conclave", "inspect", "c1"}, "GET /api/v1/conclaves/c1"},
		{[]string{"conclave", "relic", "put", "c1", "n", "--body", "b"}, "POST /api/v1/conclaves/c1/relics"},
		{[]string{"conclave", "relic", "get", "c1", "n"}, "GET /api/v1/conclaves/c1/relics/n"},
		{[]string{"memory", "list"}, "GET /api/v1/memory"},
		{[]string{"memory", "set", "a", "b", "c"}, "POST /api/v1/memory"},
		{[]string{"voice", "status"}, "GET /api/v1/voice/status"},
	}
	const marker = `{"raw":"daemon-body-7f3a"}`
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			d := newCLIDaemon().on(tc.route, http.StatusOK, marker)
			out, _, err := runCLI(t, d, "", append([]string{"--json"}, tc.args...)...)
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}
			if out != marker {
				t.Fatalf("output = %q, want raw body %q", out, marker)
			}
		})
	}
}

func TestListCommandsRenderDaemonRecords(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	cases := []struct {
		name    string
		args    []string
		route   string
		query   string
		body    string
		wantOut []string
	}{
		{"workloads", []string{"workload", "list"}, "GET /api/v1/workloads", "",
			`[{"Name":"coder","Zone":"runtime","Class":"coding-agent","ManifestDigest":"` + sha + `"},{"Name":"lab","Zone":"sandbox","Class":"job","ManifestDigest":"short"}]`,
			[]string{"coder", "runtime", "coding-agent", sha[:12], "lab", "sandbox", "short"}},
		{"no workloads", []string{"workload", "list"}, "GET /api/v1/workloads", "", `[]`, []string{"No workloads registered."}},
		{"tasks", []string{"task", "list"}, "GET /api/v1/tasks", "",
			`[{"ID":"t1","WorkloadName":"coder","State":"working","ParentTaskID":"t0","LastHeartbeatAt":"2026-11-26T08:09:10Z"},{"ID":"t2","WorkloadName":"coder","State":"failed"},{"ID":"t3","WorkloadName":"w","State":"submitted"},{"ID":"t4","WorkloadName":"w","State":"cancelled"}]`,
			[]string{"t1", "working", "t0", "08:09:10Z", "t2", "failed", "none", "submitted", "cancelled"}},
		{"no tasks", []string{"task", "list"}, "GET /api/v1/tasks", "", `[]`, []string{"No tasks submitted."}},
		{"task inspect", []string{"task", "inspect", "t1"}, "GET /api/v1/tasks/t1", "",
			`{"task":{"ID":"t1","WorkloadName":"coder","State":"completed","InputJSON":"{\"prompt\":\"x\"}"},"events":[{"FromState":"submitted","ToState":"working","Reason":"picked up","CreatedAt":"2026-11-26T08:00:00Z"}],"budget":{"InputTokens":10,"OutputTokens":5,"CostMicroUSD":1500000}}`,
			[]string{"t1", "coder", "completed", "$1.5000 (10 in / 5 out)", "submitted -> working", "picked up"}},
		{"budget", []string{"budget", "status"}, "GET /api/v1/budget", "",
			`{"InputTokens":1234,"OutputTokens":56,"CostMicroUSD":2500000}`,
			[]string{"1234", "56", "$2.5000"}},
		{"approvals", []string{"approval", "list"}, "GET /api/v1/approvals", "status=pending",
			`[{"ID":"apr1","WorkloadName":"coder","Capability":"git:push","PayloadSHA256":"` + sha + `","ExpiresAt":"2026-11-26T09:30:00Z"}]`,
			[]string{"apr1", "coder", "git:push", sha[:13] + "...", "09:30:00Z"}},
		{"no approvals", []string{"approval", "list"}, "GET /api/v1/approvals", "status=pending", `[]`, []string{"No pending approvals."}},
		{"audit", []string{"audit", "tail"}, "GET /api/v1/audit", "",
			`[{"Category":"policy","WorkloadName":"coder","Action":"git:push","Outcome":"denied","CreatedAt":"2026-11-26T07:00:00Z"},{"Category":"approval","Action":"approve","Outcome":"pending"},{"Category":"task","Action":"run","Outcome":"ok"}]`,
			[]string{"policy", "coder", "git:push", "denied", "pending", "ok", "-"}},
		{"no audit", []string{"audit", "tail"}, "GET /api/v1/audit", "", `[]`, []string{"No audit events recorded."}},
		{"audit verify", []string{"audit", "verify"}, "GET /api/v1/audit/verify", "", `{"valid":true,"verifiedRows":42}`, []string{"audit hash chain valid (42 rows verified)"}},
		{"providers", []string{"provider", "list"}, "GET /api/v1/providers", "",
			`[{"id":"openai","displayName":"OpenAI","kind":"api","connected":true,"models":[{"id":"m1"},{"id":"m2"}],"detail":"` + strings.Repeat("d", 60) + `"},{"id":"local","displayName":"Local","kind":"ollama","connected":false}]`,
			[]string{"OpenAI", "connected", "2", "...", "Local", "not connected"}},
		{"no providers", []string{"provider", "list"}, "GET /api/v1/providers", "", `[]`, []string{"No providers configured."}},
		{"devices", []string{"device", "list"}, "GET /api/v1/devices", "",
			`[{"ID":"d1","Platform":"ios","Name":"phone"},{"ID":"d2","Platform":"cli","Name":"laptop","RevokedAt":"2026-11-25T00:00:00Z"}]`,
			[]string{"d1", "ios", "active", "phone", "d2", "revoked", "laptop"}},
		{"conclaves", []string{"conclave", "list"}, "GET /api/v1/conclaves", "",
			`[{"ID":"c1","State":"active","TemplateName":"review","Title":"Review X"}]`,
			[]string{"c1", "active", "review", "Review X"}},
		{"conclave inspect", []string{"conclave", "inspect", "c1"}, "GET /api/v1/conclaves/c1", "",
			`{"conclave":{"ID":"c1","Title":"T","State":"paused","TemplateName":"review","WardReason":"ping-pong limit"},"messages":[{"ID":3,"FromRole":"coder","ToRole":"reviewer","Kind":"proposal","Body":"diff ready"}],"relics":[{"Name":"plan","AuthorRole":"coder","ContentType":"text/plain","SHA256":"` + sha + `"}]}`,
			[]string{"state=paused", "ward: ping-pong limit", "#3 @coder -> @reviewer (proposal): diff ready", "plan (by @coder, text/plain, sha256=" + sha[:12] + ")"}},
		{"relic get prints body", []string{"conclave", "relic", "get", "c1", "plan"}, "GET /api/v1/conclaves/c1/relics/plan", "",
			`{"Name":"plan","Body":"step one\nstep two"}`, []string{"step one\nstep two\n"}},
		{"memory list", []string{"memory", "list", "--namespace", "team a", "--q", "x&y"}, "GET /api/v1/memory", "namespace=team+a&q=x%26y",
			`[{"Namespace":"team a","Key":"style","Value":"tabs"}]`, []string{"team a/style  tabs"}},
		{"voice status", []string{"voice", "status"}, "GET /api/v1/voice/status", "", `{"ttsBackend":"say"}`, []string{`{"ttsBackend":"say"}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newCLIDaemon().on(tc.route, http.StatusOK, tc.body)
			out, _, err := runCLI(t, d, "", tc.args...)
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}
			reqs := d.requests()
			if len(reqs) != 1 || reqs[0].Query != tc.query {
				t.Fatalf("requests = %+v, want one with query %q", reqs, tc.query)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out, w) {
					t.Errorf("output %q does not contain %q", out, w)
				}
			}
		})
	}
}

func TestListCommandsRejectMalformedJSON(t *testing.T) {
	cases := []struct {
		args  []string
		route string
	}{
		{[]string{"workload", "list"}, "GET /api/v1/workloads"},
		{[]string{"task", "list"}, "GET /api/v1/tasks"},
		{[]string{"task", "inspect", "t1"}, "GET /api/v1/tasks/t1"},
		{[]string{"task", "submit", "w"}, "POST /api/v1/tasks"},
		{[]string{"budget", "status"}, "GET /api/v1/budget"},
		{[]string{"audit", "tail"}, "GET /api/v1/audit"},
		{[]string{"approval", "list"}, "GET /api/v1/approvals"},
		{[]string{"provider", "list"}, "GET /api/v1/providers"},
		{[]string{"provider", "sync", "p"}, "POST /api/v1/providers/p/sync"},
		{[]string{"device", "list"}, "GET /api/v1/devices"},
		{[]string{"conclave", "list"}, "GET /api/v1/conclaves"},
		{[]string{"conclave", "inspect", "c1"}, "GET /api/v1/conclaves/c1"},
		{[]string{"conclave", "relic", "get", "c1", "n"}, "GET /api/v1/conclaves/c1/relics/n"},
		{[]string{"memory", "list"}, "GET /api/v1/memory"},
		{[]string{"task", "diff", "t1"}, "GET /api/v1/tasks/t1"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			d := newCLIDaemon().on(tc.route, http.StatusOK, `<html>proxy login</html>`)
			_, _, err := runCLI(t, d, "", tc.args...)
			if err == nil || !strings.Contains(err.Error(), "invalid character") {
				t.Fatalf("error = %v, want a JSON decode error", err)
			}
		})
	}
}

func TestSecretCommandsWithFileStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	value := "sk-" + strings.Repeat("Zx9", 12)
	run := func(stdin string, args ...string) (string, string, error) {
		return runCLI(t, newCLIDaemon(), stdin, append([]string{"--secret-file", path}, args...)...)
	}

	out, _, err := run("", "secret", "check", "github_token")
	if err != nil || !strings.Contains(out, "missing github_token") {
		t.Fatalf("check before set: out=%q err=%v", out, err)
	}
	if _, _, err := run("   \n", "secret", "set", "github_token"); err == nil || err.Error() != "secret value cannot be empty" {
		t.Fatalf("empty stdin value: err=%v, want empty-value refusal", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("refused set still wrote the secret file: %v", err)
	}
	out, stderr, err := run(value+"\n", "secret", "set", "github_token")
	if err != nil || !strings.Contains(out, `stored secret "github_token"`) || !strings.Contains(stderr, "Enter value for github_token") {
		t.Fatalf("set from stdin: out=%q stderr=%q err=%v", out, stderr, err)
	}
	if strings.Contains(out+stderr, value) {
		t.Fatal("secret value was echoed to the terminal")
	}
	if got, err := secretFileValue(path, "github_token"); err != nil || got != value {
		t.Fatalf("stored value mismatch: err=%v", err)
	}
	if _, _, err := run("", "secret", "set", "other", "--value", "  v2  "); err != nil {
		t.Fatalf("set from flag: %v", err)
	}
	if got, _ := secretFileValue(path, "other"); got != "v2" {
		t.Fatalf("flag value not trimmed: %q", got)
	}
	out, _, err = run("", "secret", "check", "github_token")
	if err != nil || !strings.Contains(out, "present github_token") || strings.Contains(out, value) {
		t.Fatalf("check after set: out=%q err=%v", out, err)
	}
	out, _, err = run("", "secret", "delete", "github_token")
	if err != nil || !strings.Contains(out, `deleted secret "github_token"`) {
		t.Fatalf("delete: out=%q err=%v", out, err)
	}
	if _, _, err := run("", "secret", "delete", "github_token"); err == nil {
		t.Fatal("deleting a missing secret succeeded")
	}
	out, _, _ = run("", "secret", "check", "github_token")
	if !strings.Contains(out, "missing github_token") {
		t.Fatalf("check after delete: %q", out)
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, newCLIDaemon(), "", "--secret-file", bad, "secret", "check", "x"); err == nil {
		t.Fatal("check on a corrupt secret file reported a result instead of an error")
	}
}

func secretFileValue(path, name string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	return m[name], nil
}

func TestValidateCommand(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "sablecourt.yaml")
	t.Chdir(dir)
	t.Setenv("PATH", t.TempDir())
	if _, _, err := runCLI(t, newCLIDaemon(), "", "init", "--out", manifest); err != nil {
		t.Fatalf("init: %v", err)
	}
	out, _, err := runCLI(t, newCLIDaemon(), "", "validate", manifest)
	if err != nil || !strings.Contains(out, "valid workloads=2 policies=1 budgets=1") {
		t.Fatalf("validate: out=%q err=%v", out, err)
	}
	out, _, err = runCLI(t, newCLIDaemon(), "", "--json", "validate", manifest)
	if err != nil {
		t.Fatalf("validate --json: %v", err)
	}
	var res struct {
		Valid     bool   `json:"valid"`
		Workloads int    `json:"workloads"`
		Digest    string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Valid || res.Workloads != 2 || len(res.Digest) < 12 {
		t.Fatalf("validate --json = %q (err %v)", out, err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: sablecourt.io/v1alpha1\nkind: Nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, newCLIDaemon(), "", "validate", bad); err == nil {
		t.Fatal("validate accepted an unknown kind")
	}
}

func TestVoiceSpeakWritesAudioFile(t *testing.T) {
	audio := "RIFF\x00\x01fake-wave"
	d := newCLIDaemon()
	d.routes["POST /api/v1/voice/speak"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, audio)
	}
	outPath := filepath.Join(t.TempDir(), "reply.wav")
	out, _, err := runCLI(t, d, "", "voice", "speak", "hello", "--voice", "Zosia", "--out", outPath)
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if !jsonEqual(t, d.requests()[0].Body, `{"text":"hello","voice":"Zosia"}`) {
		t.Errorf("speak body = %s", d.requests()[0].Body)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil || string(raw) != audio {
		t.Fatalf("audio file = %q, %v", raw, err)
	}
	info, _ := os.Stat(outPath)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("audio file mode = %o, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(out, "wrote 15 bytes to "+outPath) {
		t.Errorf("output = %q", out)
	}
}

func TestVersionCommand(t *testing.T) {
	out, _, err := runCLI(t, newCLIDaemon(), "", "version")
	if err != nil || !strings.Contains(out, "sablectl "+version) {
		t.Fatalf("version: out=%q err=%v", out, err)
	}
}

func TestConclaveCloseCommand(t *testing.T) {
	t.Run("closes as completed", func(t *testing.T) {
		d := newCLIDaemon().
			on("POST /api/v1/conclaves/c%2F1/close", http.StatusOK, `{"status":"closed"}`).
			on("GET /api/v1/tasks", http.StatusOK, `[]`)
		out, _, err := runCLI(t, d, "", "conclave", "close", "c/1")
		if err != nil || !strings.Contains(out, "closed conclave c/1") {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if reqs := d.requests(); !jsonEqual(t, reqs[0].Body, `{"state":"completed"}`) {
			t.Fatalf("close body = %s", reqs[0].Body)
		}
	})
	t.Run("json prints close response", func(t *testing.T) {
		d := newCLIDaemon().
			on("POST /api/v1/conclaves/c1/close", http.StatusOK, `{"status":"closed"}`).
			on("GET /api/v1/tasks", http.StatusOK, `[]`)
		out, _, err := runCLI(t, d, "", "--json", "conclave", "close", "c1")
		if err != nil || out != `{"status":"closed"}` {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("close refused", func(t *testing.T) {
		d := newCLIDaemon().
			on("POST /api/v1/conclaves/c1/close", http.StatusConflict, "already closed").
			on("GET /api/v1/tasks", http.StatusOK, `[]`)
		out, _, err := runCLI(t, d, "", "conclave", "close", "c1")
		if err == nil || !strings.Contains(err.Error(), "close council: HTTP 409: already closed") || strings.Contains(out, "closed conclave") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
}

func TestUnixSocketDaemonGetsLocalOperatorCredential(t *testing.T) {
	sockDir, err := os.MkdirTemp("", "scsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	var (
		mu      sync.Mutex
		gotAuth string
		gotPath string
	)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.EscapedPath()
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	credential := "op_" + strings.Repeat("e", 64)
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "operator.token"), []byte(credential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SABLECOURT_STATE_DIR", state)
	t.Setenv("SABLECOURT_OPERATOR_TOKEN", "")

	root := newRootCmdWithTransport(&handlerRoundTripper{handler: newCLIDaemon()})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--api", "https://daemon.invalid", "--socket", sock, "approval", "approve", "a1", "--digest", "d"})
	if err := root.Execute(); err != nil {
		t.Fatalf("approve over socket: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/api/v1/approvals/a1/approve" || gotAuth != "Bearer "+credential {
		t.Fatalf("socket request path=%q auth set=%t", gotPath, gotAuth == "Bearer "+credential)
	}
}

func TestLocalDaemonWithUnreadableCredentialFailsClosed(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SABLECOURT_STATE_DIR", state)
	t.Setenv("SABLECOURT_OPERATOR_TOKEN", "")
	d := newCLIDaemon()
	root := newRootCmdWithTransport(&handlerRoundTripper{handler: d})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--api", "http://localhost:7432", "approval", "approve", "a1"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "read operator credential") {
		t.Fatalf("err = %v, want credential read failure", err)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("request without credential reached the daemon %d times", n)
	}
}
