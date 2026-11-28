package main

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

func synthDaemonSecret(parts ...string) string {
	return strings.Join(parts, "")
}

func TestDaemonSecretPreloadAndRegistration(t *testing.T) {
	ctx := context.Background()
	secretPath := filepath.Join(t.TempDir(), "secrets.json")
	fs := secret.NewFileStore(secretPath)

	apiKey := synthDaemonSecret("preload", "_openai_", "99118822")
	mcpKey := synthDaemonSecret("preload", "_mcp_", "88227733")
	customFileKey := synthDaemonSecret("preload", "_custom_", "77336644")
	voiceModel := "whisper-preload-model-name"

	for k, v := range map[string]string{
		"openai_api_key":  apiKey,
		"custom_mcp_auth": mcpKey,
		"extra_file_key":  customFileKey,
		"voice_stt_model": voiceModel,
	} {
		if err := fs.Set(ctx, k, v); err != nil {
			t.Fatalf("FileStore.Set %s: %v", k, err)
		}
	}

	envWebhook := "https://hooks.example.invalid/" + synthDaemonSecret("env", "_hook_", "66445522")
	t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", envWebhook)

	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{
			"w1": {
				Metadata: domain.ObjectMeta{Name: "w1"},
				Spec: domain.WorkloadSpec{
					Grants: domain.GrantSet{
						MCPServers: []domain.UpstreamMCPServer{
							{Name: "srv", URL: "https://mcp.example.invalid", SecretName: "custom_mcp_auth"},
						},
					},
				},
			},
		},
	}

	obs := secret.NewObservingStore(fs, registerDaemonSecret)
	if err := preloadDaemonSecrets(ctx, obs, fs, bundle); err != nil {
		t.Fatalf("preloadDaemonSecrets: %v", err)
	}

	for _, mustRedact := range []string{apiKey, mcpKey, customFileKey, envWebhook} {
		if got := redact.String("x=" + mustRedact); strings.Contains(got, mustRedact) {
			t.Fatalf("preloaded secret was not redacted: %q", got)
		}
	}
	// Non-secret voice settings must remain unredacted.
	if got := redact.String("model=" + voiceModel); got != "model="+voiceModel {
		t.Fatalf("non-secret voice setting was unexpectedly redacted: %q", got)
	}

	// Runtime rotation via ObservingStore.Set registers immediately.
	rotated := synthDaemonSecret("rotated", "_runtime_", "55443322")
	if err := obs.Set(ctx, "openai_api_key", rotated); err != nil {
		t.Fatalf("ObservingStore.Set: %v", err)
	}
	if got := redact.String("k=" + rotated); strings.Contains(got, rotated) {
		t.Fatalf("rotated secret was not redacted: %q", got)
	}
}

func TestRegisterDSNAndOperatorSigningKeyRotation(t *testing.T) {
	pwURL := synthDaemonSecret("pgurl", "_pass_", "44332211")
	pwKV := synthDaemonSecret("pgkv", "_pass_", "33221100")
	if err := registerDSN("postgres://user:" + pwURL + "@127.0.0.1:5432/sablecourt"); err != nil {
		t.Fatal(err)
	}
	if err := registerDSN("host=127.0.0.1 user=svc password=" + pwKV + " dbname=sablecourt"); err != nil {
		t.Fatal(err)
	}

	for _, pw := range []string{pwURL, pwKV} {
		if got := redact.String("err " + pw); strings.Contains(got, pw) {
			t.Fatalf("DSN password was not redacted: %q", got)
		}
	}

	opTok := "op_" + strings.Repeat("c3", 32)
	auth := newOperatorAuth(opTok)
	initialKeyHex := hex.EncodeToString(auth.signKey[:])
	if got := redact.String("k=" + initialKeyHex); strings.Contains(got, initialKeyHex) {
		t.Fatal("initial operator signing key hex was not redacted")
	}

	// Logout rotates signKey and registers the new key material immediately.
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7432/logout", nil)
	req.Header.Set("Origin", "http://127.0.0.1:7432")
	auth.logout(httptest.NewRecorder(), req)
	rotatedKeyHex := hex.EncodeToString(auth.signKey[:])
	if rotatedKeyHex == initialKeyHex {
		t.Fatal("logout did not rotate signKey")
	}
	if got := redact.String("k=" + rotatedKeyHex); strings.Contains(got, rotatedKeyHex) {
		t.Fatal("rotated operator signing key hex was not redacted")
	}
}

// A credential shorter than redact.MinSecretLength cannot be held by the exact
// matcher, so the daemon refuses to start with it. The error names the
// setting and never the value.
func TestShortCredentialsRefusedAtLoad(t *testing.T) {
	ctx := context.Background()
	short := "s7" + "x9q" // five bytes, synthetic
	for _, tc := range []struct {
		name, setting string
		load          func() error
	}{
		{"provider key in the file store", "openai_api_key", func() error {
			fs := secret.NewFileStore(filepath.Join(t.TempDir(), "secrets.json"))
			if err := fs.Set(ctx, "openai_api_key", short); err != nil {
				t.Fatal(err)
			}
			return preloadDaemonSecrets(ctx, secret.NewObservingStore(fs, registerDaemonSecret), fs, nil)
		}},
		{"webhook secret in the file store", "github_webhook_secret", func() error {
			fs := secret.NewFileStore(filepath.Join(t.TempDir(), "secrets.json"))
			if err := fs.Set(ctx, "github_webhook_secret", short); err != nil {
				t.Fatal(err)
			}
			return preloadDaemonSecrets(ctx, secret.NewObservingStore(fs, registerDaemonSecret), fs, nil)
		}},
		{"other secret in the file store", "extra_file_key", func() error {
			fs := secret.NewFileStore(filepath.Join(t.TempDir(), "secrets.json"))
			if err := fs.Set(ctx, "extra_file_key", short); err != nil {
				t.Fatal(err)
			}
			return preloadDaemonSecrets(ctx, secret.NewObservingStore(fs, registerDaemonSecret), fs, nil)
		}},
		{"MCP server secret named by the bundle", "short_mcp_auth", func() error {
			mem := secret.NewMemoryStore(map[string]string{"short_mcp_auth": short})
			bundle := &config.Bundle{Workloads: map[string]domain.Workload{"w": {Spec: domain.WorkloadSpec{Grants: domain.GrantSet{
				MCPServers: []domain.UpstreamMCPServer{{Name: "srv", URL: "https://mcp.example.invalid", SecretName: "short_mcp_auth"}},
			}}}}}
			return preloadDaemonSecrets(ctx, mem, nil, bundle)
		}},
		{"kubernetes bearer token from the kubeconfig", "kubernetes bearer token", func() error {
			path := filepath.Join(t.TempDir(), "kubeconfig")
			kc := "apiVersion: v1\nkind: Config\ncurrent-context: x\n" +
				"clusters: [{name: c, cluster: {server: \"https://k8s.example.invalid\"}}]\n" +
				"users: [{name: u, user: {token: \"" + short + "\"}}]\n" +
				"contexts: [{name: x, context: {cluster: c, user: u}}]\n"
			if err := os.WriteFile(path, []byte(kc), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := daemonConfig{runtimeBackend: "k8s", kubeconfigPath: path, k8sNamespace: "default"}
			return wireK8sRuntime(ctx, cfg, map[domain.WorkloadClass]scruntime.Adapter{}, attach.NewHub(), slog.New(slog.DiscardHandler))
		}},
		{"notify webhook from the environment", "SABLECOURT_NOTIFY_WEBHOOK_URL", func() error {
			t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", short)
			return preloadDaemonSecrets(ctx, nil, nil, nil)
		}},
		{"postgres URL password", "postgres DSN password", func() error {
			return registerDSN("postgres://user:" + short + "@127.0.0.1:5432/sablecourt")
		}},
		{"postgres keyword password", "postgres DSN password", func() error {
			return registerDSN("host=127.0.0.1 user=svc password=" + short + " dbname=sablecourt")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.load()
			if err == nil {
				t.Fatal("short credential accepted at load")
			}
			if !strings.Contains(err.Error(), tc.setting) || strings.Contains(err.Error(), short) {
				t.Fatalf("error %q must name %q and not the value", err, tc.setting)
			}
		})
	}
}

// registerDSN must register the password the driver sends, as pgx parses it:
// quoted values with spaces or escapes, spaces around "=", and percent-decoded
// URL passwords. The 8-byte rule applies to that parsed value.
func TestRegisterDSNPasswordAsDriverParses(t *testing.T) {
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "none"))
	for _, tc := range []struct {
		name, dsn, want string
		short           bool
	}{
		{"quoted with spaces", "host=127.0.0.1 user=svc password='" + synthDaemonSecret("correct ", "horse ", "battery9") + "' dbname=x", synthDaemonSecret("correct ", "horse ", "battery9"), false},
		{"quoted, short first word", "host=127.0.0.1 password='" + synthDaemonSecret("ab ", "cdefghij7") + "'", synthDaemonSecret("ab ", "cdefghij7"), false},
		{"escaped quote", `host=127.0.0.1 password='` + synthDaemonSecret(`it\'s `, "a longpass8") + `'`, synthDaemonSecret("it's ", "a longpass8"), false},
		{"escaped backslash", `host=127.0.0.1 password=` + synthDaemonSecret(`back\\`, "slashpw6"), synthDaemonSecret(`back\`, "slashpw6"), false},
		{"spaces around equals", "host=127.0.0.1 password = " + synthDaemonSecret("spacedpw", "_5521") + " dbname=x", synthDaemonSecret("spacedpw", "_5521"), false},
		{"url, percent encoded", "postgres://svc:" + synthDaemonSecret("p%40ss", "%20word", "9917") + "@127.0.0.1:5432/x", synthDaemonSecret("p@ss", " word", "9917"), false},
		{"quoted, short in total", "host=127.0.0.1 password='" + synthDaemonSecret("a b ", "c") + "'", "", true},
		{"url, short after decoding", "postgres://svc:" + synthDaemonSecret("%61%62", "cd") + "@127.0.0.1:5432/x", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := registerDSN(tc.dsn)
			if tc.short {
				var short *redact.ShortSecretError
				if !errors.As(err, &short) {
					t.Fatalf("registerDSN error = %v, want a short secret refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("registerDSN: %v", err)
			}
			if got, want := redact.String("x "+tc.want+" y"), "x "+redact.KnownSecretMarker+" y"; got != want {
				t.Fatalf("redact.String = %q, want %q", got, want)
			}
		})
	}
}

// Each logout swaps in a new signing key. The retired key is gone from the
// daemon, so it is unregistered: the registry does not grow with logouts.
func TestLogoutKeepsRegistryBounded(t *testing.T) {
	auth := newOperatorAuth("op_" + strings.Repeat("d4", 32))
	before := redact.DefaultRegistry().Len()
	for range 200 {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7432/logout", nil)
		req.Header.Set("Origin", "http://127.0.0.1:7432")
		auth.logout(httptest.NewRecorder(), req)
	}
	// One key has up to seven forms; how many are distinct depends on its
	// bytes (hex without letters has one case), so the count can move by a few.
	if grown := redact.DefaultRegistry().Len() - before; grown > 7 {
		t.Fatalf("registry grew by %d entries over 200 logouts, want at most 7", grown)
	}
	current := hex.EncodeToString(auth.signKey[:])
	if got := redact.String("k=" + current); strings.Contains(got, current) {
		t.Fatal("current signing key is not redacted")
	}
}
