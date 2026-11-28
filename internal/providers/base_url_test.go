package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type countingTransport struct{ n int }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n++
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[],"models":[]}`)), Request: r}, nil
}

// TestSyncModelsRefusesPlainHTTPBaseURL: a model list request carries the
// provider key, so a base URL that is plain http off the machine is refused
// before any request; loopback (the local provider's default) still works.
func TestSyncModelsRefusesPlainHTTPBaseURL(t *testing.T) {
	keys := map[string]string{
		"openai_api_key":    runtimeKey("sk-"),
		"anthropic_api_key": runtimeKey("sk-ant-"),
		"gemini_api_key":    runtimeKey("AIza"),
	}
	cases := []struct {
		provider, base, want string // want "" means one request is sent
	}{
		{"openai", "https://api.example.test", ""},
		{"local", "http://127.0.0.1:11434", ""},
		{"local", "http://[::1]:11434", ""},
		{"openai", "http://api.example.test", "openai provider base URL: plain http is refused"},
		{"anthropic", "http://api.example.test", "anthropic provider base URL: plain http is refused"},
		{"gemini", "http://api.example.test", "gemini provider base URL: plain http is refused"},
		{"local", "http://10.0.0.5:11434", "local provider base URL: plain http is refused"},
		{"openai", "ftp://api.example.test", "openai provider base URL: only https"},
	}
	for _, tc := range cases {
		tr := &countingTransport{}
		reg := NewRegistry(Options{
			Secrets: secret.NewMemoryStore(keys),
			Client:  &http.Client{Transport: tr},
			BaseURL: map[string]string{tc.provider: tc.base},
		})
		_, err := reg.SyncModels(context.Background(), tc.provider)
		if tc.want == "" {
			if err != nil || tr.n != 1 {
				t.Errorf("SyncModels(%s, %q) = %v after %d requests, want one request", tc.provider, tc.base, err, tr.n)
			}
			continue
		}
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) || strings.Contains(err.Error(), "example.test") {
			t.Errorf("SyncModels(%s, %q) = %v, want %q and no URL", tc.provider, tc.base, err, tc.want)
		}
		if tr.n != 0 {
			t.Errorf("SyncModels(%s, %q) sent %d requests, want none", tc.provider, tc.base, tr.n)
		}
	}
}

// TestGitHubDeviceFlowRefusesPlainHTTPBaseURL: the device flow returns an
// access token, so its base URL follows the same rule.
func TestGitHubDeviceFlowRefusesPlainHTTPBaseURL(t *testing.T) {
	for _, base := range []string{"http://github.example.test", "ftp://github.example.test", "github.example.test"} {
		tr := &countingTransport{}
		g := &GitHubDeviceFlow{ClientID: "client", BaseURL: base, Client: &http.Client{Transport: tr}}
		_, startErr := g.Start(context.Background(), "")
		// The timeout only bounds the loop when the refusal is missing.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, pollErr := g.PollToken(ctx, "device", time.Millisecond)
		cancel()
		for _, err := range []error{startErr, pollErr} {
			if err == nil || !strings.HasPrefix(err.Error(), "GitHub OAuth base URL: ") || strings.Contains(err.Error(), "example.test") {
				t.Errorf("base %q: err = %v, want a refusal naming the field and not the URL", base, err)
			}
		}
		if tr.n != 0 {
			t.Errorf("base %q: %d requests sent, want none", base, tr.n)
		}
	}
}
