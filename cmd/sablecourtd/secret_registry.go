package main

import (
	"context"
	"errors"
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

// registerDaemonSecret registers a value read from the secret store under
// name, or refuses it when it is too short to redact (the error names the
// setting, never the value).
func registerDaemonSecret(name, value string) error {
	if isNonSecretConfigKey(name) {
		return nil
	}
	return redact.RegisterSetting(name, value)
}

// preloadDaemonSecrets registers every credential the daemon can load before
// it serves anything. A credential shorter than redact.MinSecretLength stops
// the start with an error naming its setting.
func preloadDaemonSecrets(ctx context.Context, st secret.SecretStore, fileStore *secret.FileStore, bundle *config.Bundle) error {
	var errs []error
	if err := redact.RegisterSetting("SABLECOURT_NOTIFY_WEBHOOK_URL", os.Getenv("SABLECOURT_NOTIFY_WEBHOOK_URL")); err != nil {
		errs = append(errs, err)
	}
	if st == nil {
		return errors.Join(errs...)
	}
	keep := func(err error) {
		var short *redact.ShortSecretError
		if errors.As(err, &short) {
			errs = append(errs, err)
		}
	}
	for _, name := range knownDaemonSecretNames {
		_, err := st.Get(ctx, name)
		keep(err)
	}
	if fileStore != nil {
		if keys, err := fileStore.Keys(ctx); err == nil {
			for _, name := range keys {
				if isNonSecretConfigKey(name) {
					continue
				}
				_, err := st.Get(ctx, name)
				keep(err)
			}
		}
	}
	keep(preloadBundleSecrets(ctx, st, bundle))
	return errors.Join(errs...)
}

// preloadBundleSecrets registers the MCP server secrets a bundle names. The
// store passed in registers what it reads (an ObservingStore); a plain store
// is registered here.
func preloadBundleSecrets(ctx context.Context, st secret.SecretStore, bundle *config.Bundle) error {
	if st == nil || bundle == nil {
		return nil
	}
	var errs []error
	for _, w := range bundle.Workloads {
		for _, srv := range w.Spec.Grants.MCPServers {
			name := strings.TrimSpace(srv.SecretName)
			if name == "" {
				continue
			}
			val, err := st.Get(ctx, name)
			if err == nil {
				err = registerDaemonSecret(name, val)
			}
			var short *redact.ShortSecretError
			if errors.As(err, &short) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// registerDSN registers the password of a PostgreSQL DSN and the DSN itself,
// or refuses a password too short to redact.
func registerDSN(dsn string) error {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil
	}
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			if err := redact.RegisterSetting("postgres DSN password", pw); err != nil {
				return err
			}
			redact.Register(dsn)
			return nil
		}
	}
	for field := range strings.FieldsSeq(dsn) {
		k, v, ok := strings.Cut(field, "=")
		if !ok || !strings.EqualFold(k, "password") {
			continue
		}
		v = strings.Trim(v, `"'`)
		if v == "" {
			continue
		}
		if err := redact.RegisterSetting("postgres DSN password", v); err != nil {
			return err
		}
		redact.Register(dsn)
	}
	return nil
}
