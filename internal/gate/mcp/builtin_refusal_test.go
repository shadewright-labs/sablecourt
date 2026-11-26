package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type failingPROpener struct{ err error }

func (f failingPROpener) OpenPR(context.Context, OpenPRInput) (string, error) { return "", f.err }

func builtinGate(t *testing.T, repoRoot string, opener PROpener) *Gate {
	t.Helper()
	gate, _, _, _ := setupMCPGateTest(t)
	bundle := gate.snapshot().bundle
	bundle.Workloads["exp-sandbox"] = domain.Workload{
		Metadata: domain.ObjectMeta{Name: "exp-sandbox", Zone: domain.ZoneSandbox},
	}
	RegisterBuiltinGitOps(gate, repoRoot, opener)
	return gate
}

func runAction(t *testing.T, g *Gate, name, payload string) (any, error) {
	t.Helper()
	g.mu.RLock()
	fn := g.actions[name]
	g.mu.RUnlock()
	if fn == nil {
		t.Fatalf("action %q not registered", name)
	}
	return fn(context.Background(), domain.Workload{}, "task-1", json.RawMessage(payload))
}

func TestBuiltinCreateBranchRefusals(t *testing.T) {
	repo := initGitRepo(t)
	gate := builtinGate(t, repo, nil)
	noRoot := builtinGate(t, "", nil)

	cases := []struct {
		name    string
		g       *Gate
		payload string
		wantErr string
	}{
		{"unknown field", gate, `{"branch":"sablecourt/a","force":true}`, "invalid action payload"},
		{"empty branch", gate, `{"branch":"  "}`, "protected or empty"},
		{"master", gate, `{"branch":"master"}`, "protected or empty"},
		{"outside prefix", gate, `{"branch":"feature/x"}`, "must start with sablecourt/"},
		{"dot dot", gate, `{"branch":"sablecourt/../main"}`, "illegal characters"},
		{"space", gate, `{"branch":"sablecourt/a b"}`, "illegal characters"},
		{"glob", gate, `{"branch":"sablecourt/a*"}`, "illegal characters"},
		{"no repo root", noRoot, `{"branch":"sablecourt/a"}`, "repo root not configured"},
		{"base as option", gate, `{"branch":"sablecourt/a","base":"--orphan"}`, "invalid base ref"},
		{"base range", gate, `{"branch":"sablecourt/a","base":"main..HEAD"}`, "invalid base ref"},
		{"base with space", gate, `{"branch":"sablecourt/a","base":"ma in"}`, "invalid base ref"},
		{"base that does not exist", gate, `{"branch":"sablecourt/a","base":"no-such-ref"}`, "git branch failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runAction(t, tc.g, "git.create_branch", tc.payload)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("create_branch(%s) = %v, %v; want error containing %q", tc.payload, out, err, tc.wantErr)
			}
		})
	}

	out, err := runAction(t, gate, "git.create_branch", `{"branch":"sablecourt/from-head","base":"HEAD"}`)
	if err != nil {
		t.Fatalf("create_branch with base HEAD: %v", err)
	}
	if m, _ := out.(map[string]string); m["status"] != "created" || m["branch"] != "sablecourt/from-head" {
		t.Fatalf("result = %#v", out)
	}
}

func TestBuiltinOpenPRRefusals(t *testing.T) {
	ok := &stubPROpener{}
	cases := []struct {
		name    string
		opener  PROpener
		payload string
		wantErr string
	}{
		{"head outside prefix", ok, `{"owner":"o","repo":"r","head":"main","base":"main","title":"t"}`, "invalid head branch"},
		{"missing owner", ok, `{"repo":"r","head":"sablecourt/x","base":"main","title":"t"}`, "owner, repo, base, and title are required"},
		{"blank title", ok, `{"owner":"o","repo":"r","head":"sablecourt/x","base":"main","title":" "}`, "required"},
		{"missing base", ok, `{"owner":"o","repo":"r","head":"sablecourt/x","title":"t"}`, "required"},
		{"unknown field", ok, `{"owner":"o","repo":"r","head":"sablecourt/x","base":"main","title":"t","draft":true}`, "invalid action payload"},
		{"no opener", nil, `{"owner":"o","repo":"r","head":"sablecourt/x","base":"main","title":"t"}`, "PR opener not configured"},
		{"opener fails", failingPROpener{errors.New("github down")}, `{"owner":"o","repo":"r","head":"sablecourt/x","base":"main","title":"t"}`, "github down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := builtinGate(t, "", tc.opener)
			if _, err := runAction(t, gate, "git.open_pr", tc.payload); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("open_pr = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if ok.last != (OpenPRInput{}) {
		t.Fatalf("the opener was called for a refused request: %+v", ok.last)
	}
}

func TestBuiltinProposePromotionRefusals(t *testing.T) {
	gate := builtinGate(t, "", nil)
	cases := []struct {
		name    string
		payload string
		wantErr string
	}{
		{"bad branch", `{"workload":"exp-sandbox","branch":"main","reason":"r"}`, "protected"},
		{"unknown workload", `{"workload":"nobody","branch":"sablecourt/p","reason":"r"}`, `workload "nobody" not found`},
		{"already runtime", `{"workload":"triage-agent","branch":"sablecourt/p","reason":"r"}`, "already in zone"},
		{"unknown field", `{"workload":"exp-sandbox","branch":"sablecourt/p","zone":"runtime"}`, "invalid action payload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runAction(t, gate, "sandbox.propose_promotion", tc.payload); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("propose_promotion = %v, want %q", err, tc.wantErr)
			}
		})
	}

	gate.Reload(nil, gate.snapshot().policy)
	if _, err := runAction(t, gate, "sandbox.propose_promotion", `{"workload":"exp-sandbox","branch":"sablecourt/p","reason":"r"}`); err == nil || !strings.Contains(err.Error(), "no manifest bundle") {
		t.Fatalf("propose_promotion without a bundle = %v", err)
	}
}

func TestBuiltinListWorkloadsTool(t *testing.T) {
	gate := builtinGate(t, "", nil)
	gate.mu.RLock()
	def := gate.tools["sablecourt.list_workloads"]
	gate.mu.RUnlock()
	out, err := def.Handler(context.Background(), domain.Workload{}, "task-1", nil)
	if err != nil {
		t.Fatalf("list_workloads: %v", err)
	}
	var items []domain.WorkloadRecord
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(items) != 1 || items[0].Name != "triage-agent" {
		t.Fatalf("workloads = %+v", items)
	}
	if string(def.InputSchema) != `{"type":"object"}` {
		t.Fatalf("default input schema = %s", def.InputSchema)
	}
}

func TestGitHubPROpener(t *testing.T) {
	ctx := context.Background()
	in := OpenPRInput{Owner: "acme", Repo: "widgets", Head: "sablecourt/x", Base: "main", Title: "Fix", Body: "b"}
	tokenValue := "ghp_" + strings.Repeat("Zx9", 12)

	t.Run("no secret store", func(t *testing.T) {
		if _, err := (&GitHubPROpener{}).OpenPR(ctx, in); err == nil || !strings.Contains(err.Error(), "secret store unavailable") {
			t.Fatalf("OpenPR = %v", err)
		}
	})

	t.Run("no token", func(t *testing.T) {
		gh := &GitHubPROpener{Secrets: secret.NewMemoryStore(nil)}
		if _, err := gh.OpenPR(ctx, in); err == nil || !strings.Contains(err.Error(), "load github_token") {
			t.Fatalf("OpenPR = %v", err)
		}
	})

	cases := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr string
	}{
		{"created", http.StatusCreated, `{"html_url":"https://github.example/acme/widgets/pull/3"}`, "https://github.example/acme/widgets/pull/3", ""},
		{"refused", http.StatusUnprocessableEntity, `{"message":"exists"}`, "", "HTTP 422"},
		{"redirect is not success", http.StatusMovedPermanently, ``, "", "HTTP 301"},
		{"garbage body", http.StatusCreated, `not json`, "", "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotReq *http.Request
			var gotBody map[string]string
			gh := &GitHubPROpener{
				Secrets: secret.NewMemoryStore(map[string]string{"github_token": tokenValue}),
				APIURL:  "https://api.github.example/",
				Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					gotReq = r
					_ = json.NewDecoder(r.Body).Decode(&gotBody)
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})},
			}
			got, err := gh.OpenPR(ctx, in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("OpenPR = %q, %v; want %q", got, err, tc.wantErr)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("OpenPR = %q, %v; want %q", got, err, tc.want)
			}
			if gotReq.Method != http.MethodPost || gotReq.URL.String() != "https://api.github.example/repos/acme/widgets/pulls" {
				t.Fatalf("request = %s %s", gotReq.Method, gotReq.URL)
			}
			if gotReq.Header.Get("Authorization") != "Bearer "+tokenValue {
				t.Fatal("token not sent as a bearer header")
			}
			if gotBody["head"] != "sablecourt/x" || gotBody["base"] != "main" || gotBody["title"] != "Fix" {
				t.Fatalf("body = %v", gotBody)
			}
		})
	}

	t.Run("transport error", func(t *testing.T) {
		gh := &GitHubPROpener{
			Secrets: secret.NewMemoryStore(map[string]string{"github_token": tokenValue}),
			Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" {
					t.Errorf("default API host = %q", r.URL.Host)
				}
				return nil, errors.New("dial refused")
			})},
		}
		if _, err := gh.OpenPR(ctx, in); err == nil || !strings.Contains(err.Error(), "dial refused") {
			t.Fatalf("OpenPR = %v", err)
		}
	})
}

func toolText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v", res.Content)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type %T", res.Content[0])
	}
	return tc.Text
}

func TestInvokeToolDecisions(t *testing.T) {
	ctx := context.Background()
	gate, st, _, _ := setupMCPGateTest(t)
	bundle := gate.snapshot().bundle
	wl := bundle.Workloads["triage-agent"]
	wl.Spec.Grants.MCPTools = append(wl.Spec.Grants.MCPTools, "github.comment", "memory.put")
	pol := bundle.Policies["default"]
	pol.Spec.Rules = append(pol.Spec.Rules,
		domain.PolicyRule{Workload: "triage-agent", Capability: "mcp:github.comment", Effect: domain.EffectApprove},
		domain.PolicyRule{Workload: "triage-agent", Capability: "mcp:memory.put", Effect: domain.EffectAllow},
	)
	gate.Reload(bundle, policy.NewEngine([]domain.Policy{pol}, nil))
	lease := domain.Lease{TaskID: "task-1", WorkloadName: "triage-agent"}

	called := 0
	commentDef := ToolDef{Name: "github.comment", Handler: func(context.Context, domain.Workload, string, json.RawMessage) (string, error) {
		called++
		return "posted", nil
	}}

	// Approval required: the handler does not run, an approval is queued and the task waits for input.
	res, err := gate.invokeTool(ctx, lease, wl, commentDef, json.RawMessage(`{"body":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	var pending map[string]string
	if err := json.Unmarshal([]byte(toolText(t, res)), &pending); err != nil {
		t.Fatalf("decode %q: %v", toolText(t, res), err)
	}
	if res.IsError || pending["status"] != "approval_required" || pending["approval_id"] == "" || called != 0 {
		t.Fatalf("approve result = %+v (called %d)", pending, called)
	}
	apr, err := st.GetApproval(ctx, pending["approval_id"])
	if err != nil || apr.Capability != "mcp:github.comment" || apr.PayloadCanonicalJSON != `{"body":"hi"}` {
		t.Fatalf("stored approval = %+v, %v", apr, err)
	}
	task, err := st.GetTask(ctx, "task-1")
	if err != nil || task.State != domain.TaskStateInputRequired {
		t.Fatalf("task = %+v, %v", task, err)
	}

	// Without a queue the request fails closed.
	gate.approvals = nil
	res, _ = gate.invokeTool(ctx, lease, wl, commentDef, nil)
	if !res.IsError || !strings.Contains(toolText(t, res), "approval queue unavailable") || called != 0 {
		t.Fatalf("no queue result = %+v", res)
	}

	// A refusal from the handler is audited as denied, other errors as error, both redacted.
	secretValue := "ghp_" + strings.Repeat("q7W", 12)
	refusing := ToolDef{Name: "memory.put", Handler: func(context.Context, domain.Workload, string, json.RawMessage) (string, error) {
		return "", fmt.Errorf("%w: reserved namespace near %s", errToolRefused, secretValue)
	}}
	res, _ = gate.invokeTool(ctx, lease, wl, refusing, nil)
	if !res.IsError || strings.Contains(toolText(t, res), secretValue) {
		t.Fatalf("refusal result = %q", toolText(t, res))
	}
	failing := ToolDef{Name: "memory.put", Handler: func(context.Context, domain.Workload, string, json.RawMessage) (string, error) {
		return "", errors.New("disk full")
	}}
	res, _ = gate.invokeTool(ctx, lease, wl, failing, nil)
	if !res.IsError || toolText(t, res) != "disk full" {
		t.Fatalf("error result = %q", toolText(t, res))
	}
	recs, _ := st.ListAudit(ctx, 50)
	if !hasAuditRecord(recs, "data_action_gate", "mcp:memory.put", "denied", "refused") {
		t.Fatal("no denied audit record for the refusal")
	}
	if !hasAuditRecord(recs, "data_action_gate", "mcp:memory.put", "error", "disk full") {
		t.Fatal("no error audit record")
	}

	// No policy engine: fail closed and audit it.
	gate.Reload(bundle, nil)
	res, _ = gate.invokeTool(ctx, lease, wl, commentDef, nil)
	if !res.IsError || toolText(t, res) != "policy failed closed" {
		t.Fatalf("nil policy result = %q", toolText(t, res))
	}
	recs, _ = st.ListAudit(ctx, 5)
	if !hasAuditRecord(recs, "data_action_gate", "mcp:github.comment", "denied", "policy_engine_unavailable") {
		t.Fatal("no audit record for the missing policy engine")
	}
}

func TestRegisterUpstreamToolsSkipsIncompleteServers(t *testing.T) {
	gate := New(Options{Bundle: &config.Bundle{}, Approvals: approval.NewQueue(nil, nil), Now: func() time.Time { return time.Unix(0, 0) }})
	gate.RegisterUpstreamTools([]domain.UpstreamMCPServer{{Name: "x", URL: "https://x.example", AllowedTools: []string{"t"}}}, nil)
	if len(gate.tools) != 0 {
		t.Fatalf("tools registered without a bridge: %v", gate.tools)
	}

	var calledTool string
	bridge := NewUpstreamBridge(nil, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		calledTool = body.Params.Name
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"result":{"ok":true}}`))}, nil
	})})
	gate.RegisterUpstreamTools([]domain.UpstreamMCPServer{
		{Name: "", URL: "https://a.example", AllowedTools: []string{"t"}},
		{Name: "b", URL: " ", AllowedTools: []string{"t"}},
		{Name: "linear", URL: "https://linear.example/mcp", AllowedTools: []string{" list ", ""}},
	}, bridge)
	if len(gate.tools) != 1 {
		t.Fatalf("tools = %v, want only linear.list", gate.tools)
	}
	def, ok := gate.tools["linear.list"]
	if !ok {
		t.Fatalf("tools = %v", gate.tools)
	}
	out, err := def.Handler(context.Background(), domain.Workload{}, "t", nil)
	if err != nil || out != `{"ok":true}` || calledTool != "list" {
		t.Fatalf("handler = %q, %v (tool %q)", out, err, calledTool)
	}
}

func TestUpstreamBridgeRefusals(t *testing.T) {
	ctx := context.Background()
	respond := func(status int, body string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
	}
	leaked := "ghp_" + strings.Repeat("m3N", 12)
	srv := domain.UpstreamMCPServer{Name: "s", URL: "https://mcp.example/mcp"}
	withSecret := srv
	withSecret.SecretName = "api_key"

	cases := []struct {
		name    string
		bridge  *UpstreamBridge
		srv     domain.UpstreamMCPServer
		want    string
		wantErr string
	}{
		{"file URL", NewUpstreamBridge(nil, respond(200, `{}`)), domain.UpstreamMCPServer{URL: "file:///etc/passwd"}, "", "invalid upstream MCP server URL"},
		{"no host", NewUpstreamBridge(nil, respond(200, `{}`)), domain.UpstreamMCPServer{URL: "https:///x"}, "", "invalid upstream MCP server URL"},
		{"secret without store", NewUpstreamBridge(nil, respond(200, `{}`)), withSecret, "", "secret store unavailable"},
		{"missing secret", NewUpstreamBridge(secret.NewMemoryStore(nil), respond(200, `{}`)), withSecret, "", `secret "api_key" unavailable`},
		{"blank secret", NewUpstreamBridge(secret.NewMemoryStore(map[string]string{"api_key": "  "}), respond(200, `{}`)), withSecret, "", `secret "api_key" unavailable`},
		{"upstream 500 is redacted", NewUpstreamBridge(nil, respond(500, "boom "+leaked)), srv, "", "HTTP 500"},
		{"rpc error is redacted", NewUpstreamBridge(nil, respond(200, `{"error":{"code":-32601,"message":"no tool `+leaked+`"}}`)), srv, "", "upstream MCP error -32601"},
		{"plain text body", NewUpstreamBridge(nil, respond(200, "plain "+leaked)), srv, "plain [REDACTED", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.bridge.CallTool(ctx, tc.srv, "t", json.RawMessage(`not json`))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("CallTool = %q, %v; want %q", out, err, tc.wantErr)
				}
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("error leaked the credential: %v", err)
				}
				return
			}
			if err != nil || !strings.HasPrefix(out, tc.want) || strings.Contains(out, leaked) {
				t.Fatalf("CallTool = %q, %v; want prefix %q", out, err, tc.want)
			}
		})
	}

	t.Run("custom header without prefix", func(t *testing.T) {
		var got http.Header
		b := NewUpstreamBridge(secret.NewMemoryStore(map[string]string{"api_key": " v1 "}), &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r.Header
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"result":1}`))}, nil
		})})
		s := withSecret
		s.AuthHeader = "X-Api-Key"
		if _, err := b.CallTool(ctx, s, "t", nil); err != nil {
			t.Fatal(err)
		}
		if got.Get("X-Api-Key") != "v1" || got.Get("Authorization") != "" {
			t.Fatalf("headers = %v", got)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		b := NewUpstreamBridge(nil, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("refused")
		})})
		if _, err := b.CallTool(ctx, srv, "t", nil); err == nil || !strings.Contains(err.Error(), "upstream MCP request failed") {
			t.Fatalf("CallTool = %v", err)
		}
	})
}
