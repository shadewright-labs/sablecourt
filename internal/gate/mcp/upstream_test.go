package mcpgate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpstreamBridgeInjectsSecretAndRedactsResponse(t *testing.T) {
	var gotAuth string
	var gotTool string
	cli := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotAuth = r.Header.Get("Authorization")
			var req struct {
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotTool = req.Params.Name
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"issue":"LIN-42","note":"leaked ghp_1234567890abcdefghijklmnop1234567890"}}`)),
			}, nil
		}),
	}

	bridge := NewUpstreamBridge(secret.NewMemoryStore(map[string]string{"linear_api_key": "lin_secret_xyz"}), cli)
	out, err := bridge.CallTool(context.Background(), domain.UpstreamMCPServer{
		Name:         "linear",
		URL:          "https://mcp.linear.example/mcp",
		SecretName:   "linear_api_key",
		AllowedTools: []string{"list_issues"},
	}, "list_issues", json.RawMessage(`{"team":"ENG"}`))
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if gotAuth != "Bearer lin_secret_xyz" {
		t.Fatalf("expected injected secret header, got %q", gotAuth)
	}
	if gotTool != "list_issues" {
		t.Fatalf("expected tool list_issues, got %q", gotTool)
	}
	if strings.Contains(out, "ghp_123456") {
		t.Fatalf("expected secret in upstream MCP response to be redacted, got %q", out)
	}
	if !strings.Contains(out, "LIN-42") {
		t.Fatalf("expected issue ID in output, got %q", out)
	}
}
