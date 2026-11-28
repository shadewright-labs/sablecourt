package councilproxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// TestCommandProfileAllowsNoProvider checks the proxy profile of a command
// container (issue 134): a CONNECT to any provider host of any harness is
// refused before a lookup or a dial, so the command has no model access.
func TestCommandProfileAllowsNoProvider(t *testing.T) {
	hosts, ok := providerHosts[CommandHarness]
	if !ok || len(hosts) != 0 {
		t.Fatalf("command profile hosts = %v (known %v), want a known profile with none", hosts, ok)
	}
	cfg := Config{Harness: CommandHarness, DaemonListen: "127.0.0.1:7432"}
	if err := cfg.validateCommon(); err != nil {
		t.Fatalf("command profile refused: %v", err)
	}
	var targets []string
	for harness, list := range providerHosts {
		if harness == CommandHarness {
			continue
		}
		for _, host := range list {
			targets = append(targets, net.JoinHostPort(host, "443"))
		}
	}
	targets = append(targets, "example.com:443", "api.anthropic.com:443")
	tunnels := make(chan struct{}, 1)
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			looked, dialed := false, false
			lookup := func(context.Context, string, string) ([]netip.Addr, error) {
				looked = true
				return []netip.Addr{netip.AddrFrom4([4]byte{1, 2, 3, 4})}, nil
			}
			dial := func(context.Context, string, string) (net.Conn, error) { dialed = true; return nil, net.ErrClosed }
			req := httptest.NewRequest(http.MethodConnect, "http://"+target, nil)
			req.URL.Host, req.Host, req.RequestURI = target, target, target
			rec := httptest.NewRecorder()
			providerConnect(rec, req, hosts, tunnels, lookup, dial)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "destination refused") || looked || dialed {
				t.Fatalf("code %d body %q looked %v dialed %v, want a refusal with no lookup or dial", rec.Code, rec.Body.String(), looked, dialed)
			}
		})
	}
}
