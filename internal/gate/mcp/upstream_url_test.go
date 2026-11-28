package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// TestUpstreamBridgeRefusesPlainHTTPAtCallTime is the request-time half of the
// rule the loader applies to grants.mcpServers[].url: a server that reaches
// the bridge without the loader (a direct registration, a test double) still
// gets no request, no secret lookup, and an error without the URL.
func TestUpstreamBridgeRefusesPlainHTTPAtCallTime(t *testing.T) {
	cred := "hunter" + "2secret"
	cases := []struct {
		url  string
		want string // "" means the call is sent
	}{
		{"https://mcp.example.test/mcp", ""},
		{"http://127.0.0.1:8931/mcp", ""},
		{"http://[::1]:8931/mcp", ""},
		{"http://mcp.example.test/mcp", `upstream MCP server "up" url: plain http is refused`},
		{"http://u:" + cred + "@mcp.example.test/mcp", `upstream MCP server "up" url: plain http is refused`},
		{"ws://127.0.0.1:8931/mcp", `upstream MCP server "up" url: only https`},
		{"mcp.example.test/mcp", `upstream MCP server "up" url: the URL has no scheme`},
		{"https:///mcp", `upstream MCP server "up" url: the URL has no host`},
	}
	for _, tc := range cases {
		var sent int
		cli := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"result":{}}`)), Request: r}, nil
		})}
		lookups := &countingStore{SecretStore: secret.NewMemoryStore(map[string]string{"up_token": "up-token-" + cred})}
		bridge := NewUpstreamBridge(lookups, cli)
		_, err := bridge.CallTool(context.Background(), domain.UpstreamMCPServer{Name: "up", URL: tc.url, SecretName: "up_token"}, "get", json.RawMessage(`{}`))
		if tc.want == "" {
			if err != nil || sent != 1 {
				t.Errorf("CallTool(%q) = %v after %d requests, want one request", tc.url, err, sent)
			}
			continue
		}
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("CallTool(%q) = %v, want %q", tc.url, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), "mcp.example.test")) {
			t.Errorf("CallTool(%q) error %q carries the URL", tc.url, err)
		}
		if sent != 0 || lookups.gets != 0 {
			t.Errorf("CallTool(%q) sent %d requests and read the secret %d times, want none", tc.url, sent, lookups.gets)
		}
	}
}

// TestUpstreamBridgeRefusesRedirectToPlainHTTP: an https server cannot move
// the call, with its credential header, to plain http.
func TestUpstreamBridgeRefusesRedirectToPlainHTTP(t *testing.T) {
	var hosts []string
	cli := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Scheme+"://"+r.URL.Host)
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"http://mcp.example.test/mcp"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})}
	bridge := NewUpstreamBridge(secret.NewMemoryStore(map[string]string{"up_token": "up-token-value"}), cli)
	_, err := bridge.CallTool(context.Background(), domain.UpstreamMCPServer{Name: "up", URL: "https://mcp.example.test/mcp", SecretName: "up_token"}, "get", nil)
	if err == nil || !strings.Contains(err.Error(), `upstream MCP server "up" url redirect: plain http is refused`) {
		t.Fatalf("CallTool = %v, want a redirect refusal", err)
	}
	if len(hosts) != 1 || hosts[0] != "https://mcp.example.test" {
		t.Fatalf("requests went to %v; the plain http target must never be contacted", hosts)
	}
}

type countingStore struct {
	secret.SecretStore
	gets int
}

func (s *countingStore) Get(ctx context.Context, name string) (string, error) {
	s.gets++
	return s.SecretStore.Get(ctx, name)
}

// TestUpstreamBridgeTransportErrorOmitsURL: a failed call reports the cause
// without the URL, whose user name the http client keeps in its error.
func TestUpstreamBridgeTransportErrorOmitsURL(t *testing.T) {
	cli := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	bridge := NewUpstreamBridge(nil, cli)
	_, err := bridge.CallTool(context.Background(), domain.UpstreamMCPServer{Name: "up", URL: "https://svc-account@mcp.example.test/mcp"}, "get", nil)
	if err == nil || !strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "svc-account") || strings.Contains(err.Error(), "example.test") {
		t.Fatalf("CallTool = %v, want the cause without the URL", err)
	}
}
