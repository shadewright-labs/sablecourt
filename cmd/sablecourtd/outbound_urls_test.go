package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunDaemonRefusesPlainHTTPOutboundURLs covers every outbound URL the
// daemon takes from a flag or the environment: plain http to a host that is
// not loopback stops the start before any state is created, and the error
// names the flag or variable but never the URL or its credentials.
func TestRunDaemonRefusesPlainHTTPOutboundURLs(t *testing.T) {
	cred := "hunter" + "2secret"
	for _, tc := range []struct {
		name  string
		cfg   daemonConfig
		env   map[string]string
		field string
	}{
		{name: "gateway flag", cfg: daemonConfig{modelGatewayURL: "http://example.test"}, field: "--model-gateway-url"},
		{name: "gateway flag with credentials", cfg: daemonConfig{modelGatewayURL: "http://gw:" + cred + "@example.test:3000"}, field: "--model-gateway-url"},
		{name: "gateway flag other scheme", cfg: daemonConfig{modelGatewayURL: "ftp://example.test"}, field: "--model-gateway-url"},
		{name: "gateway flag without scheme", cfg: daemonConfig{modelGatewayURL: "example.test:3000"}, field: "--model-gateway-url"},
		{name: "gateway variable", env: map[string]string{"SABLECOURT_MODEL_GATEWAY_URL": "http://example.test"}, field: "SABLECOURT_MODEL_GATEWAY_URL"},
		{name: "manifests repository", cfg: daemonConfig{manifestsGitRepo: "http://user:" + cred + "@example.test/m.git"}, field: "--manifests-git-repo"},
		{name: "manifests repository git protocol", cfg: daemonConfig{manifestsGitRepo: "git://example.test/m.git"}, field: "--manifests-git-repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
			t.Setenv("SABLECOURT_MODEL_GATEWAY_URL", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			dbDir := t.TempDir()
			cfg := tc.cfg
			cfg.dbPath = filepath.Join(dbDir, "sablecourt.db")
			cfg.runtimeBackend = "k8s"
			cfg.listenAddr = "127.0.0.1:0"
			err := runDaemon(t.Context(), cfg)
			if err == nil || !strings.HasPrefix(err.Error(), tc.field+": ") {
				t.Fatalf("runDaemon = %v, want an error naming %s", err, tc.field)
			}
			if strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), "example.test") {
				t.Fatalf("runDaemon error %q carries the URL", err)
			}
			if _, err := os.Lstat(cfg.dbPath); !os.IsNotExist(err) {
				t.Fatalf("refused start created the database: %v", err)
			}
		})
	}
}

// TestCheckOutboundURLsAcceptsLoopbackAndHTTPS is the other half: the
// addresses the refusal must leave alone.
func TestCheckOutboundURLsAcceptsLoopbackAndHTTPS(t *testing.T) {
	for _, cfg := range []daemonConfig{
		{},
		{modelGatewayURL: "http://127.0.0.1:3000"},
		{modelGatewayURL: "http://[::1]:3000"},
		{modelGatewayURL: "http://localhost:3000"},
		{modelGatewayURL: "https://gateway.example.test"},
		{manifestsGitRepo: "https://git.example.test/org/m.git"},
		{manifestsGitRepo: "git@git.example.test:org/m.git"},
		{manifestsGitRepo: "/srv/manifests"},
	} {
		if err := checkOutboundURLs(cfg, "--model-gateway-url"); err != nil {
			t.Errorf("checkOutboundURLs(%+v) = %v, want accepted", cfg, err)
		}
	}
}
