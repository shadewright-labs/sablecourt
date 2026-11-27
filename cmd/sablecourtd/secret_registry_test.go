package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
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
	preloadDaemonSecrets(ctx, obs, fs, bundle)

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
	registerDSN("postgres://user:" + pwURL + "@127.0.0.1:5432/sablecourt")
	registerDSN("host=127.0.0.1 user=svc password=" + pwKV + " dbname=sablecourt")

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
