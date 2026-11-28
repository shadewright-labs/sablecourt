package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// A credential too short to redact is refused before any request is sent.
func TestShortCredentialsRefusedBeforeUse(t *testing.T) {
	ctx := context.Background()
	const short = "short12"
	noCall := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request sent with a refused credential")
		return nil, errors.New("unreachable")
	})}

	gh := &GitHubPROpener{Secrets: secret.NewMemoryStore(map[string]string{"github_token": short}), Client: noCall}
	in := OpenPRInput{Owner: "acme", Repo: "widgets", Head: "x", Base: "main", Title: "t"}
	var shortErr *redact.ShortSecretError
	if _, err := gh.OpenPR(ctx, in); !errors.As(err, &shortErr) {
		t.Fatalf("OpenPR with a short token = %v", err)
	}

	bridge := NewUpstreamBridge(secret.NewMemoryStore(map[string]string{"linear_api_key": short}), noCall)
	_, err := bridge.CallTool(ctx, domain.UpstreamMCPServer{
		Name: "linear", URL: "https://mcp.linear.example/mcp", SecretName: "linear_api_key", AllowedTools: []string{"list_issues"},
	}, "list_issues", json.RawMessage(`{}`))
	if !errors.As(err, &shortErr) {
		t.Fatalf("CallTool with a short key = %v", err)
	}
}
