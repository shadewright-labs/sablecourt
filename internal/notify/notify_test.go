package notify

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotifierFormatsAndRedacts(t *testing.T) {
	var capturedURL, capturedBody string
	cli := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			capturedURL = r.URL.String()
			b, _ := io.ReadAll(r.Body)
			capturedBody = string(b)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}

	sec := secret.NewMemoryStore(map[string]string{
		"notify_webhook_url": "https://discord.com/api/webhooks/123/abc",
	})
	n := New(Options{
		Secrets:    sec,
		PublicURL:  "https://sablecourt.tail.ts.net",
		HTTPClient: cli,
	})

	err := n.Send(context.Background(), Event{
		Type:   "conclave.ward_paused",
		Title:  "Ward tripped on ccl-1",
		Detail: "Budget exceeded with leaked key sk-1234567890abcdefghij123456",
		Path:   "/conclaves/ccl-1",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(capturedURL, "discord.com") {
		t.Fatalf("unexpected URL: %s", capturedURL)
	}
	if strings.Contains(capturedBody, "sk-1234567890") {
		t.Fatalf("expected secret redaction in webhook payload, got %s", capturedBody)
	}
	if !strings.Contains(capturedBody, "https://sablecourt.tail.ts.net/conclaves/ccl-1") {
		t.Fatalf("expected deep link in payload, got %s", capturedBody)
	}
}
