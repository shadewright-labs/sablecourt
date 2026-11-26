package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestMCPToolsSendExactRequests(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		args     map[string]any
		route    string
		respBody string
		wantBody string
		wantText string
	}{
		{"list workloads", "sablecourt_list_workloads", map[string]any{}, "GET /api/v1/workloads", `[{"Name":"coder"}]`, "", `[{"Name":"coder"}]`},
		{"budget status", "sablecourt_budget_status", map[string]any{}, "GET /api/v1/budget", `{"CostMicroUSD":5}`, "", `{"CostMicroUSD":5}`},
		{"submit task trims workload and wraps prompt", "sablecourt_submit_task",
			map[string]any{"workload": "  coder ", "prompt": "fix it", "provider": "anthropic", "model": "m1"},
			"POST /api/v1/tasks", `{"ID":"t1"}`,
			`{"workload":"coder","inputJson":"{\"model\":\"m1\",\"prompt\":\"fix it\",\"provider\":\"anthropic\"}"}`, `{"ID":"t1"}`},
		{"submit task without provider", "sablecourt_submit_task",
			map[string]any{"workload": "coder", "prompt": "p"},
			"POST /api/v1/tasks", `{}`, `{"workload":"coder","inputJson":"{\"prompt\":\"p\"}"}`, `{}`},
		{"open conclave trims template and title", "sablecourt_open_conclave",
			map[string]any{"template": " review ", "title": " T ", "decree": " go "},
			"POST /api/v1/conclaves", `{"ID":"c1"}`, `{"template":"review","title":"T","decree":" go "}`, `{"ID":"c1"}`},
		{"conclave decree defaults to all and trims body", "sablecourt_conclave_decree",
			map[string]any{"conclave_id": "c1", "body": "  look  "},
			"POST /api/v1/conclaves/c1/decree", `{"ID":4}`, `{"to":"all","body":"look"}`, `{"ID":4}`},
		{"conclave decree to role", "sablecourt_conclave_decree",
			map[string]any{"conclave_id": "c1", "body": "b", "to": " coder "},
			"POST /api/v1/conclaves/c1/decree", `{}`, `{"to":"coder","body":"b"}`, `{}`},
		{"council say decree to role", "sablecourt_council_say",
			map[string]any{"conclave_id": "c1", "body": "check", "to": "reviewer"},
			"POST /api/v1/conclaves/c1/decree", `{"ID":9}`, `{"to":"reviewer","body":"check"}`, "decree #9 sent to @reviewer"},
		{"council read relic", "sablecourt_council_read",
			map[string]any{"conclave_id": "c1", "relic": "digest"},
			"GET /api/v1/conclaves/c1/relics/digest",
			`{"Name":"digest","AuthorRole":"system","ContentType":"text/markdown","SHA256":"0123456789abcdef","Body":"  verdict  "}`, "",
			"Relic digest (by @system, text/markdown, sha256=0123456789ab):\nverdict\n"},
		{"council read short sha", "sablecourt_council_read",
			map[string]any{"conclave_id": "c1", "relic": "n"},
			"GET /api/v1/conclaves/c1/relics/n", `{"Name":"n","SHA256":"abc","Body":"x"}`, "", "sha256=abc)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newCLIDaemon().on(tc.route, http.StatusOK, tc.respBody)
			cs := connectTestMCP(t, d)
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil || res.IsError {
				t.Fatalf("CallTool: err=%v res=%+v", err, res)
			}
			reqs := d.requests()
			if len(reqs) != 1 || reqs[0].Method+" "+reqs[0].Path != tc.route {
				t.Fatalf("requests = %+v, want one %s", reqs, tc.route)
			}
			if !jsonEqual(t, reqs[0].Body, tc.wantBody) {
				t.Errorf("body = %s, want %s", reqs[0].Body, tc.wantBody)
			}
			if got := mcpText(res); !strings.Contains(got, tc.wantText) {
				t.Errorf("tool text = %q, want it to contain %q", got, tc.wantText)
			}
		})
	}
}

func TestMCPCouncilReadFormatsSinceAndDigest(t *testing.T) {
	detail := `{"conclave":{"ID":"c1","Title":"Q","State":"active","TemplateName":"council","WardReason":"limit"},` +
		`"messages":[{"ID":1,"FromRole":"claude","ToRole":"all","Kind":"proposal","Body":"old"},{"ID":2,"Round":1,"FromRole":"codex","ToRole":"all","Kind":"review","Body":" new "}],` +
		`"relics":[{"Name":"notes","AuthorRole":"claude","ContentType":"text/plain","SHA256":"` + strings.Repeat("f", 64) + `"}]}`
	d := newCLIDaemon().on("GET /api/v1/conclaves/c1", http.StatusOK, detail)
	cs := connectTestMCP(t, d)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "sablecourt_council_read", Arguments: map[string]any{"conclave_id": "c1", "since_id": 1}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool: err=%v res=%+v", err, res)
	}
	got := mcpText(res)
	for _, w := range []string{`Council c1 ("Q") state=active template=council`, `Ward: "limit"`, `#2 round=1 @codex -> @all (review): "new"`, "- notes (by @claude, text/plain, sha256=ffffffffffff)", "Digest: no digest recorded"} {
		if !strings.Contains(got, w) {
			t.Errorf("text %q missing %q", got, w)
		}
	}
	if strings.Contains(got, "old") {
		t.Errorf("message at or below since_id was included: %q", got)
	}

	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "sablecourt_council_read", Arguments: map[string]any{"conclave_id": "c1", "since_id": 5}})
	if err != nil || res.IsError || !strings.Contains(mcpText(res), "(none)") {
		t.Fatalf("since past all messages: err=%v text=%q", err, mcpText(res))
	}
}

func TestMCPCouncilSayCloseStates(t *testing.T) {
	cases := []struct {
		name      string
		args      map[string]any
		wantState string
		wantText  string
	}{
		{"close defaults to completed", map[string]any{"conclave_id": "c1", "close": true}, `{"state":"completed"}`, "council c1 closed (completed)"},
		{"state alone closes", map[string]any{"conclave_id": "c1", "state": "cancelled"}, `{"state":"cancelled"}`, "council c1 closed (cancelled)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newCLIDaemon().
				on("POST /api/v1/conclaves/c1/close", http.StatusOK, `{}`).
				on("GET /api/v1/tasks", http.StatusOK, `[]`)
			cs := connectTestMCP(t, d)
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "sablecourt_council_say", Arguments: tc.args})
			if err != nil || res.IsError {
				t.Fatalf("CallTool: err=%v res=%+v", err, res)
			}
			if got := mcpText(res); got != tc.wantText {
				t.Errorf("text = %q, want %q", got, tc.wantText)
			}
			reqs := d.requests()
			if len(reqs) == 0 || reqs[0].Path != "/api/v1/conclaves/c1/close" || !jsonEqual(t, reqs[0].Body, tc.wantState) {
				t.Fatalf("first request = %+v, want close with %s", reqs, tc.wantState)
			}
		})
	}
}

func TestMCPToolRefusalsSendNothing(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args any
		want string
	}{
		{"submit missing workload", "sablecourt_submit_task", map[string]any{"workload": " ", "prompt": "p"}, "workload is required"},
		{"submit missing prompt", "sablecourt_submit_task", map[string]any{"workload": "w", "prompt": " "}, "prompt is required"},
		{"submit wrong type", "sablecourt_submit_task", map[string]any{"workload": 1, "prompt": "p"}, ""},
		{"open missing template", "sablecourt_open_conclave", map[string]any{"template": " ", "title": "t"}, "template is required"},
		{"open missing title", "sablecourt_open_conclave", map[string]any{"template": "x", "title": ""}, "title is required"},
		{"decree oversized body", "sablecourt_conclave_decree", map[string]any{"conclave_id": "c1", "body": strings.Repeat("a", (64<<10)+1)}, "body exceeds 64 KiB"},
		{"decree control char id", "sablecourt_conclave_decree", map[string]any{"conclave_id": "c\x001", "body": "b"}, "invalid conclave_id"},
		{"inspect task control char", "sablecourt_inspect_task", map[string]any{"task_id": "t\n1"}, "invalid task_id"},
		{"inspect conclave wrong type", "sablecourt_inspect_conclave", map[string]any{"conclave_id": true}, ""},
		{"council read relic control char", "sablecourt_council_read", map[string]any{"conclave_id": "c1", "relic": "a\x7fb"}, "invalid relic"},
		{"council read wrong type", "sablecourt_council_read", map[string]any{"conclave_id": 3}, ""},
		{"say state with close false", "sablecourt_council_say", map[string]any{"conclave_id": "c1", "close": false, "state": "cancelled"}, "state cannot be set when close is false"},
		{"say body and state", "sablecourt_council_say", map[string]any{"conclave_id": "c1", "body": "x", "state": "completed"}, "body cannot be combined with close or state"},
		{"say close false and no body", "sablecourt_council_say", map[string]any{"conclave_id": "c1", "close": false}, "body or close is required"},
		{"say wrong type", "sablecourt_council_say", map[string]any{"conclave_id": []int{1}}, ""},
		{"convene repo with control char", "sablecourt_council_convene", map[string]any{"question": "q", "repo": "repo\x1bpath"}, "invalid repo path"},
		{"convene wrong type", "sablecourt_council_convene", map[string]any{"question": 7}, ""},
		{"open wrong type", "sablecourt_open_conclave", map[string]any{"template": 1, "title": "t"}, ""},
		{"decree wrong type", "sablecourt_conclave_decree", map[string]any{"conclave_id": 1, "body": "b"}, ""},
		{"arguments over 256 KiB", "sablecourt_submit_task", map[string]any{"workload": "w", "prompt": strings.Repeat("p", 257<<10)}, "tool arguments exceed 256 KiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newCLIDaemon()
			cs := connectTestMCP(t, d)
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err == nil && (res == nil || !res.IsError) {
				t.Fatalf("want refusal, got success: %+v", res)
			}
			if tc.want != "" {
				msg := ""
				if err != nil {
					msg = err.Error()
				} else {
					msg = mcpText(res)
				}
				if !strings.Contains(msg, tc.want) {
					t.Errorf("refusal %q does not contain %q", msg, tc.want)
				}
			}
			if n := len(d.requests()); n != 0 {
				t.Fatalf("sent %d daemon requests on refused input", n)
			}
		})
	}
}

func TestMCPDaemonErrorsBecomeToolErrors(t *testing.T) {
	cases := []struct {
		tool  string
		args  map[string]any
		route string
	}{
		{"sablecourt_list_workloads", map[string]any{}, "GET /api/v1/workloads"},
		{"sablecourt_submit_task", map[string]any{"workload": "w", "prompt": "p"}, "POST /api/v1/tasks"},
		{"sablecourt_inspect_task", map[string]any{"task_id": "t1"}, "GET /api/v1/tasks/t1"},
		{"sablecourt_open_conclave", map[string]any{"template": "x", "title": "t"}, "POST /api/v1/conclaves"},
		{"sablecourt_conclave_decree", map[string]any{"conclave_id": "c1", "body": "b"}, "POST /api/v1/conclaves/c1/decree"},
		{"sablecourt_inspect_conclave", map[string]any{"conclave_id": "c1"}, "GET /api/v1/conclaves/c1"},
		{"sablecourt_budget_status", map[string]any{}, "GET /api/v1/budget"},
		{"sablecourt_council_read", map[string]any{"conclave_id": "c1"}, "GET /api/v1/conclaves/c1"},
		{"sablecourt_council_read", map[string]any{"conclave_id": "c1", "relic": "r"}, "GET /api/v1/conclaves/c1/relics/r"},
		{"sablecourt_council_say", map[string]any{"conclave_id": "c1", "body": "b"}, "POST /api/v1/conclaves/c1/decree"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.route, func(t *testing.T) {
			d := newCLIDaemon().on(tc.route, http.StatusForbidden, `{"error":"policy denied"}`)
			cs := connectTestMCP(t, d)
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool transport error: %v", err)
			}
			if !res.IsError || mcpText(res) != `HTTP 403: {"error":"policy denied"}` {
				t.Fatalf("result IsError=%v text=%q, want the daemon status as a tool error", res.IsError, mcpText(res))
			}
		})
	}
}

func TestMCPCouncilReadRejectsMalformedDaemonJSON(t *testing.T) {
	for _, args := range []map[string]any{
		{"conclave_id": "c1"},
		{"conclave_id": "c1", "relic": "r"},
	} {
		d := newCLIDaemon().
			on("GET /api/v1/conclaves/c1", http.StatusOK, "<html>").
			on("GET /api/v1/conclaves/c1/relics/r", http.StatusOK, "<html>")
		cs := connectTestMCP(t, d)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "sablecourt_council_read", Arguments: args})
		if err != nil || !res.IsError || !strings.Contains(mcpText(res), "invalid character") {
			t.Fatalf("args %v: err=%v res=%+v", args, err, res)
		}
	}
}

func TestDecodeMCPArgs(t *testing.T) {
	var dst struct {
		A string `json:"a"`
	}
	cases := []struct {
		raw  string
		want string
	}{
		{"", "tool arguments object is required"},
		{"  null ", "tool arguments object is required"},
		{"[1,2]", "tool arguments must be a JSON object"},
		{`"str"`, "tool arguments must be a JSON object"},
		{"{bad", "invalid character"},
		{`{"a":"ok"}`, ""},
	}
	for _, tc := range cases {
		err := decodeMCPArgs([]byte(tc.raw), &dst)
		if tc.want == "" {
			if err != nil || dst.A != "ok" {
				t.Errorf("%q: err=%v dst=%+v", tc.raw, err, dst)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err=%v, want %q", tc.raw, err, tc.want)
		}
	}
	over := []byte(`{"a":"` + strings.Repeat("x", maxMCPArgsBytes) + `"}`)
	if err := decodeMCPArgs(over, &dst); err == nil || err.Error() != "tool arguments exceed 256 KiB" {
		t.Errorf("oversized args: err=%v", err)
	}
}
