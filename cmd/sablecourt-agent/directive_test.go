package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestParseMentionTarget(t *testing.T) {
	cases := []struct {
		reply, target, body string
	}{
		{"@reviewer: please check", "reviewer", "please check"},
		{"  @coder, done  ", "coder", "done"},
		{"@lead", "lead", "@lead"},
		{"@ hello", "operator", "@ hello"},
		{"@: x", "operator", "@: x"},
		{"plain answer", "operator", "plain answer"},
		{"mail me at x@y", "operator", "mail me at x@y"},
	}
	for _, tc := range cases {
		target, body := parseMentionTarget(tc.reply)
		if target != tc.target || body != tc.body {
			t.Errorf("parseMentionTarget(%q) = %q, %q; want %q, %q", tc.reply, target, body, tc.target, tc.body)
		}
	}
}

func TestExecuteDirectiveAction(t *testing.T) {
	var got struct {
		path, auth, body string
	}
	useFakeGate(t, func(r *http.Request) (*http.Response, error) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		got.body = string(raw)
		return gateResponse(http.StatusAccepted, ` {"status":"approval_required"} `), nil
	})
	out := executeDirective(context.Background(), nil, "https://gate.example", "lease-1", toolCallDirective{Action: "git.open_pr", Args: map[string]any{"head": "sablecourt/x"}})
	if out != `HTTP 202: {"status":"approval_required"}` {
		t.Fatalf("output = %q", out)
	}
	var sent map[string]map[string]string
	if err := json.Unmarshal([]byte(got.body), &sent); err != nil || sent["payload"]["head"] != "sablecourt/x" {
		t.Fatalf("body = %q, %v", got.body, err)
	}
	if got.path != "/gate/actions/git.open_pr" || got.auth != "Bearer lease-1" {
		t.Fatalf("request = %s with %q", got.path, got.auth)
	}

	useFakeGate(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("gate down") })
	if out := executeDirective(context.Background(), nil, "https://gate.example", "lease-1", toolCallDirective{Action: "x"}); !strings.HasPrefix(out, "error: ") || !strings.Contains(out, "gate down") {
		t.Fatalf("transport failure output = %q", out)
	}
}

func TestExecuteDirectiveRefusals(t *testing.T) {
	useFakeGate(t, func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", r.URL)
		return gateResponse(http.StatusOK, ""), nil
	})
	cases := []struct {
		name string
		dir  toolCallDirective
		want string
	}{
		{"tool without session", toolCallDirective{MCPTool: "memory.get"}, "error: MCP session unavailable"},
		{"action without gate", toolCallDirective{Action: "git.open_pr"}, "error: unsupported directive"},
		{"empty directive", toolCallDirective{}, "error: unsupported directive"},
	}
	for _, tc := range cases {
		if out := executeDirective(context.Background(), nil, "", "t", tc.dir); out != tc.want {
			t.Errorf("%s: output = %q, want %q", tc.name, out, tc.want)
		}
	}
}

func TestExecuteDirectiveMCPTool(t *testing.T) {
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "gate", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "memory.get", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]string
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "value of " + args["key"]}, &mcp.TextContent{Text: "second"}}}, nil
	})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close() //nolint:errcheck // test cleanup
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close() //nolint:errcheck // test cleanup

	if out := executeDirective(ctx, cs, "", "t", toolCallDirective{MCPTool: "memory.get", Args: map[string]any{"key": "k1"}}); out != "value of k1\nsecond" {
		t.Fatalf("output = %q", out)
	}
	if out := executeDirective(ctx, cs, "", "t", toolCallDirective{MCPTool: "no.such_tool"}); !strings.HasPrefix(out, "error: ") {
		t.Fatalf("unknown tool output = %q", out)
	}
}
