package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type Event struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Path   string `json:"path,omitempty"`
}

type Options struct {
	Secrets    secret.SecretStore
	PublicURL  string
	HTTPClient *http.Client
}

// Notifier delivers redacted operator alerts to Slack, Discord, ntfy, or a generic JSON webhook.
type Notifier struct {
	secrets   secret.SecretStore
	publicURL string
	client    *http.Client
}

func New(opts Options) *Notifier {
	cli := opts.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 8 * time.Second}
	}
	pub := strings.TrimRight(strings.TrimSpace(opts.PublicURL), "/")
	if pub == "" {
		pub = strings.TrimRight(strings.TrimSpace(os.Getenv("SABLECOURT_PUBLIC_URL")), "/")
	}
	return &Notifier{
		secrets:   opts.Secrets,
		publicURL: pub,
		client:    cli,
	}
}

// webhookURL returns the configured webhook URL, or "" when none is set. A URL
// too short to redact is refused with an error naming its setting, and no
// webhook is sent.
func (n *Notifier) webhookURL(ctx context.Context) (string, error) {
	if n.secrets != nil {
		u, err := n.secrets.Get(ctx, "notify_webhook_url")
		if short := (*redact.ShortSecretError)(nil); errors.As(err, &short) {
			return "", err
		}
		if err == nil && strings.TrimSpace(u) != "" {
			if err := redact.RegisterSetting("notify_webhook_url", u); err != nil {
				return "", err
			}
			return strings.TrimSpace(u), nil
		}
	}
	u := strings.TrimSpace(os.Getenv("SABLECOURT_NOTIFY_WEBHOOK_URL"))
	if err := redact.RegisterSetting("SABLECOURT_NOTIFY_WEBHOOK_URL", u); err != nil {
		return "", err
	}
	return u, nil
}

// Send dispatches a redacted notification if notify_webhook_url is configured.
// Returns nil when no webhook URL is configured so local setups stay silent,
// and an error naming the setting when the URL is too short to redact.
func (n *Notifier) Send(ctx context.Context, ev Event) error {
	if n == nil {
		return nil
	}
	target, err := n.webhookURL(ctx)
	if err != nil {
		return err
	}
	if target == "" {
		return nil
	}
	title := redact.String(strings.TrimSpace(ev.Title))
	detail := redact.String(strings.TrimSpace(ev.Detail))
	link := ""
	if n.publicURL != "" && ev.Path != "" {
		link = n.publicURL + ev.Path
	}

	var req *http.Request
	lower := strings.ToLower(target)

	switch {
	case strings.Contains(lower, "discord.com/api/webhooks") || strings.Contains(lower, "discordapp.com/api/webhooks"):
		text := fmt.Sprintf("**[Sablecourt] %s**\n%s", title, detail)
		if link != "" {
			text += "\n" + link
		}
		raw, _ := json.Marshal(map[string]string{"content": text})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}

	case strings.Contains(lower, "hooks.slack.com/"):
		text := fmt.Sprintf("*[Sablecourt] %s*\n%s", title, detail)
		if link != "" {
			text += "\n" + link
		}
		raw, _ := json.Marshal(map[string]string{"text": text})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}

	case strings.Contains(lower, "ntfy.sh/") || strings.Contains(lower, "/ntfy/"):
		body := detail
		if link != "" {
			body += "\n" + link
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
		if err == nil {
			req.Header.Set("Title", "Sablecourt: "+title)
			if link != "" {
				req.Header.Set("Click", link)
			}
		}

	default:
		payload := map[string]string{
			"source": "sablecourt",
			"type":   ev.Type,
			"title":  title,
			"detail": detail,
			"url":    link,
		}
		raw, _ := json.Marshal(payload)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	if err != nil {
		return err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("notify webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
