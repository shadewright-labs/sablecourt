package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type captured struct {
	url    string
	header http.Header
	body   string
	calls  int
}

func capturingClient(c *captured, status int, err error) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		c.calls++
		c.url = r.URL.String()
		c.header = r.Header
		b, _ := io.ReadAll(r.Body)
		c.body = string(b)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
}

func TestNotifierTargetFormats(t *testing.T) {
	leaked := "sk-" + strings.Repeat("a1", 14)
	ev := Event{Type: "approval.requested", Title: " Approve " + leaked + " ", Detail: "detail " + leaked, Path: "/approvals/a1"}

	cases := []struct {
		name   string
		target string
		public string
		check  func(t *testing.T, c *captured)
	}{
		{"slack", "https://hooks.slack.com/services/T/B/X", "https://sc.example/", func(t *testing.T, c *captured) {
			var m map[string]string
			if err := json.Unmarshal([]byte(c.body), &m); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(m["text"], "*[Sablecourt] Approve ") || !strings.HasSuffix(m["text"], "\nhttps://sc.example/approvals/a1") {
				t.Fatalf("slack text = %q", m["text"])
			}
			if c.header.Get("Content-Type") != "application/json" {
				t.Fatalf("content type = %q", c.header.Get("Content-Type"))
			}
		}},
		{"discordapp without link", "https://discordapp.com/api/webhooks/1/x", "", func(t *testing.T, c *captured) {
			var m map[string]string
			_ = json.Unmarshal([]byte(c.body), &m)
			if !strings.HasPrefix(m["content"], "**[Sablecourt] Approve ") || strings.Contains(m["content"], "/approvals/a1") {
				t.Fatalf("discord content = %q", m["content"])
			}
		}},
		{"ntfy", "https://ntfy.sh/sablecourt-alerts", "https://sc.example", func(t *testing.T, c *captured) {
			if !strings.HasPrefix(c.header.Get("Title"), "Sablecourt: Approve ") || c.header.Get("Click") != "https://sc.example/approvals/a1" {
				t.Fatalf("ntfy headers = %v", c.header)
			}
			if !strings.HasPrefix(c.body, "detail ") || !strings.HasSuffix(c.body, "\nhttps://sc.example/approvals/a1") {
				t.Fatalf("ntfy body = %q", c.body)
			}
		}},
		{"self hosted ntfy without link", "https://push.example/ntfy/topic", "", func(t *testing.T, c *captured) {
			if c.header.Get("Click") != "" || strings.Contains(c.body, "\n") {
				t.Fatalf("ntfy without a public URL sent a link: %v %q", c.header, c.body)
			}
		}},
		{"generic JSON", "https://hooks.example/in", "https://sc.example", func(t *testing.T, c *captured) {
			var m map[string]string
			if err := json.Unmarshal([]byte(c.body), &m); err != nil {
				t.Fatal(err)
			}
			if m["source"] != "sablecourt" || m["type"] != "approval.requested" || m["url"] != "https://sc.example/approvals/a1" || !strings.HasPrefix(m["title"], "Approve ") {
				t.Fatalf("payload = %v", m)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SABLECOURT_PUBLIC_URL", "")
			var c captured
			n := New(Options{
				Secrets:    secret.NewMemoryStore(map[string]string{"notify_webhook_url": " " + tc.target + " "}),
				PublicURL:  tc.public,
				HTTPClient: capturingClient(&c, http.StatusNoContent, nil),
			})
			if err := n.Send(context.Background(), ev); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if c.url != tc.target {
				t.Fatalf("url = %q", c.url)
			}
			if strings.Contains(c.body, leaked) || strings.Contains(c.header.Get("Title"), leaked) {
				t.Fatalf("credential reached the webhook: %q %v", c.body, c.header)
			}
			tc.check(t, &c)
		})
	}
}

func TestNotifierConfigurationAndFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("nil notifier and no target are silent", func(t *testing.T) {
		var n *Notifier
		if err := n.Send(ctx, Event{Title: "x"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", "")
		var c captured
		if err := New(Options{HTTPClient: capturingClient(&c, 200, nil)}).Send(ctx, Event{Title: "x"}); err != nil || c.calls != 0 {
			t.Fatalf("Send without a target = %v, calls %d", err, c.calls)
		}
	})

	t.Run("environment fallbacks", func(t *testing.T) {
		t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", "https://hooks.example/env")
		t.Setenv("SABLECOURT_PUBLIC_URL", "https://env.example/")
		var c captured
		n := New(Options{Secrets: secret.NewMemoryStore(map[string]string{"notify_webhook_url": "  "}), HTTPClient: capturingClient(&c, 200, nil)})
		if err := n.Send(ctx, Event{Title: "t", Path: "/p"}); err != nil {
			t.Fatal(err)
		}
		if c.url != "https://hooks.example/env" || !strings.Contains(c.body, `"url":"https://env.example/p"`) {
			t.Fatalf("url %q body %q", c.url, c.body)
		}
	})

	t.Run("HTTP error status", func(t *testing.T) {
		var c captured
		n := New(Options{Secrets: secret.NewMemoryStore(map[string]string{"notify_webhook_url": "https://hooks.example/x"}), HTTPClient: capturingClient(&c, http.StatusForbidden, nil)})
		if err := n.Send(ctx, Event{Title: "t"}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("Send = %v", err)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		var c captured
		n := New(Options{Secrets: secret.NewMemoryStore(map[string]string{"notify_webhook_url": "https://hooks.example/x"}), HTTPClient: capturingClient(&c, 0, errors.New("dial"))})
		if err := n.Send(ctx, Event{Title: "t"}); err == nil || !strings.Contains(err.Error(), "dial") {
			t.Fatalf("Send = %v", err)
		}
	})

	t.Run("unparsable target", func(t *testing.T) {
		var c captured
		n := New(Options{Secrets: secret.NewMemoryStore(map[string]string{"notify_webhook_url": "http://[::1"}), HTTPClient: capturingClient(&c, 200, nil)})
		if err := n.Send(ctx, Event{Title: "t"}); err == nil || c.calls != 0 {
			t.Fatalf("Send = %v, calls %d", err, c.calls)
		}
	})
}
