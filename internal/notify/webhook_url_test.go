package notify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// TestNotifierRefusesPlainHTTPWebhook: the webhook URL is itself a secret
// (Slack and Discord put the token in the path), so plain http off the
// machine is refused before a request, for the secret and the variable alike,
// and the error names the setting, never the URL.
func TestNotifierRefusesPlainHTTPWebhook(t *testing.T) {
	tok := "T0KEN" + "pathsecret"
	cases := []struct {
		name, secretURL, envURL, want string // want "" means sent
	}{
		{"https secret", "https://hooks.example.test/" + tok, "", ""},
		{"loopback secret", "http://127.0.0.1:8080/ntfy/" + tok, "", ""},
		{"loopback v6 variable", "", "http://[::1]:8080/" + tok, ""},
		{"plain http secret", "http://hooks.example.test/" + tok, "", "notify_webhook_url: plain http is refused"},
		{"plain http variable", "", "http://hooks.example.test/" + tok, "SABLECOURT_NOTIFY_WEBHOOK_URL: plain http is refused"},
		{"other scheme", "ftp://hooks.example.test/" + tok, "", "notify_webhook_url: only https"},
		{"no scheme", "hooks.example.test/" + tok, "", "notify_webhook_url: the URL has no scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", tc.envURL)
			var c captured
			opts := Options{HTTPClient: capturingClient(&c, 200, nil)}
			if tc.secretURL != "" {
				opts.Secrets = secret.NewMemoryStore(map[string]string{"notify_webhook_url": tc.secretURL})
			}
			err := New(opts).Send(context.Background(), Event{Type: "notify.test", Title: "t"})
			if tc.want == "" {
				if err != nil || c.calls != 1 {
					t.Fatalf("Send = %v after %d calls, want one", err, c.calls)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), "example.test") {
				t.Fatalf("Send = %v, want %q without the URL", err, tc.want)
			}
			if c.calls != 0 {
				t.Fatalf("Send made %d requests, want none", c.calls)
			}
		})
	}
}

// TestNotifierTransportErrorOmitsWebhookURL: a failed request reports the
// cause without the URL the http client puts in its error.
func TestNotifierTransportErrorOmitsWebhookURL(t *testing.T) {
	t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", "")
	tok := "T0KEN" + "pathsecret"
	var c captured
	n := New(Options{Secrets: secret.NewMemoryStore(map[string]string{"notify_webhook_url": "https://hooks.example.test/" + tok}), HTTPClient: capturingClient(&c, 0, errors.New("dial refused"))})
	err := n.Send(context.Background(), Event{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "dial refused") || strings.Contains(err.Error(), tok) {
		t.Fatalf("Send = %v, want the cause without the URL", err)
	}
}

func TestNotifierRefusesRedirectToPlainHTTP(t *testing.T) {
	t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", "")
	var calls int
	cli := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://hooks.example.test/in"}}, Body: http.NoBody, Request: r}, nil
	})}
	n := New(Options{Secrets: secret.NewMemoryStore(map[string]string{"notify_webhook_url": "https://hooks.example.test/in"}), HTTPClient: cli})
	err := n.Send(context.Background(), Event{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "redirect: plain http is refused") || calls != 1 {
		t.Fatalf("Send = %v after %d calls, want a redirect refusal after one", err, calls)
	}
}
