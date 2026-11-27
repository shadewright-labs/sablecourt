package main

import (
	"context"
	"net/url"
	"os"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

var knownDaemonSecretNames = []string{
	"openai_api_key",
	"anthropic_api_key",
	"gemini_api_key",
	"local_model_api_key",
	"github_token",
	"github_webhook_secret",
	"a2a_bearer_token",
	"observer_bearer_token",
	"shadewright_bearer_token",
	"notify_webhook_url",
}

// isNonSecretConfigKey reports keys stored in SecretStore for non-sensitive
// local voice settings that must remain readable in status responses.
func isNonSecretConfigKey(name string) bool {
	switch name {
	case "voice_stt_url", "voice_stt_model", "voice_tts_url", "voice_tts_model", "voice_tts_voice":
		return true
	default:
		return false
	}
}

func registerDaemonSecret(name, value string) {
	if isNonSecretConfigKey(name) {
		return
	}
	redact.Register(value)
}

func preloadDaemonSecrets(ctx context.Context, st secret.SecretStore, fileStore *secret.FileStore, bundle *config.Bundle) {
	if envWebhook := strings.TrimSpace(os.Getenv("SABLECOURT_NOTIFY_WEBHOOK_URL")); envWebhook != "" {
		redact.Register(envWebhook)
	}
	if st == nil {
		return
	}
	for _, name := range knownDaemonSecretNames {
		if val, err := st.Get(ctx, name); err == nil {
			registerDaemonSecret(name, val)
		}
	}
	if fileStore != nil {
		if keys, err := fileStore.Keys(ctx); err == nil {
			for _, name := range keys {
				if isNonSecretConfigKey(name) {
					continue
				}
				if val, err := st.Get(ctx, name); err == nil {
					registerDaemonSecret(name, val)
				}
			}
		}
	}
	preloadBundleSecrets(ctx, st, bundle)
}

func preloadBundleSecrets(ctx context.Context, st secret.SecretStore, bundle *config.Bundle) {
	if st == nil || bundle == nil {
		return
	}
	for _, w := range bundle.Workloads {
		for _, srv := range w.Spec.Grants.MCPServers {
			name := strings.TrimSpace(srv.SecretName)
			if name == "" {
				continue
			}
			if val, err := st.Get(ctx, name); err == nil {
				registerDaemonSecret(name, val)
			}
		}
	}
}

func registerDSN(dsn string) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return
	}
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			redact.Register(pw, dsn)
			return
		}
	}
	for field := range strings.FieldsSeq(dsn) {
		k, v, ok := strings.Cut(field, "=")
		if !ok || !strings.EqualFold(k, "password") {
			continue
		}
		v = strings.Trim(v, `"'`)
		if v != "" {
			redact.Register(v, dsn)
		}
	}
}
