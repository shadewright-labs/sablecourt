package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
)

func TestCouncilProxyAuthority(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1).String()
	authority := net.JoinHostPort("sablecourt-council-gate", "7432")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		name, method, target string
		want                 int
	}{
		{"mcp", "POST", "/gate/mcp", 204},
		{"messages", "GET", "/gate/conclave/room/messages?since=0", 204},
		{"relic", "GET", "/gate/conclave/room/relics/artifact", 204},
		{"task", "GET", "/gate/task/messages", 204},
		{"action", "POST", "/gate/actions/git.create_branch", 204},
		{"operator", "GET", "/api/v1/tasks", 403},
		{"login", "POST", "/login", 403},
		{"model", "POST", "/gate/model/openai/v1/responses", 403},
		{"unknown gate", "GET", "/gate/unknown", 403},
		{"wrong method", "PUT", "/gate/task/messages", 403},
		{"noncanonical", "GET", "/gate/task/../task/messages", 403},
		{"escaped slash", "GET", "/gate/conclave/room%2Fother/messages", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, extraHosts := range [][]string{nil, {authority}} {
				cfg := daemonConfig{listenAddr: net.JoinHostPort(ip, "7432"), allowedHosts: extraHosts}
				h, err := cfg.hostHandler(handler, false)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(tc.method, tc.target, nil)
				r.Host = authority
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Errorf("extra hosts %v: got %d, want %d", extraHosts, w.Code, tc.want)
				}
			}
		})
	}
}

func TestCouncilProxyRequiresLoopbackDaemon(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		allowed    bool
	}{
		{"loopback", net.IPv6loopback.String(), true},
		{"unspecified", net.IPv6unspecified.String(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := daemonConfig{listenAddr: net.JoinHostPort(tc.host, "7432")}
			h, err := cfg.hostHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), false)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "/gate/task/messages", nil)
			r.Host = net.JoinHostPort("sablecourt-council-gate", "7432")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if (w.Code == http.StatusNoContent) != tc.allowed {
				t.Fatalf("unexpected status %d", w.Code)
			}
		})
	}
}

func TestCouncilProxyPreflightOnDaemonHostHandler(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1).String()
	listen := net.JoinHostPort(ip, "7432")
	authority, err := councilproxy.GateAuthority(listen)
	if err != nil {
		t.Fatal(err)
	}
	const launchID = "sablecourt-council-host-test"
	var secret [32]byte
	for i := range secret {
		secret[i] = byte(i + 7)
	}
	councilproxy.DefaultRegistry.Register(authority, launchID, secret)
	t.Cleanup(func() {
		councilproxy.DefaultRegistry.Unregister(authority, launchID)
	})

	cfg := daemonConfig{listenAddr: listen}
	tcpHandler, err := cfg.hostHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("preflight must not reach inner mux")
		w.WriteHeader(http.StatusOK)
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	unixHandler, err := cfg.hostHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), true)
	if err != nil {
		t.Fatal(err)
	}

	challenge := strings.Repeat("c", 32)
	wantProof := councilproxy.ComputeGateProof(secret, authority, launchID, challenge)

	for _, tc := range []struct {
		name      string
		h         http.Handler
		method    string
		launch    string
		challenge string
		want      int
		wantProof string
	}{
		{"registered launch over tcp", tcpHandler, http.MethodHead, launchID, challenge, http.StatusNoContent, wantProof},
		{"unregistered launch", tcpHandler, http.MethodHead, "unknown-launch", challenge, http.StatusForbidden, ""},
		{"get method refused", tcpHandler, http.MethodGet, launchID, challenge, http.StatusForbidden, ""},
		{"operator unix socket refused", unixHandler, http.MethodHead, launchID, challenge, http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, councilproxy.PreflightPath, nil)
			req.Host = authority
			req.Header.Set(councilproxy.LaunchHeader, tc.launch)
			req.Header.Set(councilproxy.ChallengeHeader, tc.challenge)
			rec := httptest.NewRecorder()
			tc.h.ServeHTTP(rec, req)
			if rec.Code != tc.want || rec.Header().Get(councilproxy.ProofHeader) != tc.wantProof || (tc.want == http.StatusNoContent && tc.wantProof == "") {
				t.Fatalf("status=%d proof=%q, want status=%d proof=%q", rec.Code, rec.Header().Get(councilproxy.ProofHeader), tc.want, tc.wantProof)
			}
		})
	}
}

func TestInitDaemonStateSweepsStaleCouncilGateSockets(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "sc-state-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	gatesDir := councilproxy.SocketRoot(stateDir)
	staleLaunch := filepath.Join(gatesDir, "g-stale1234")
	if err := os.MkdirAll(staleLaunch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gatesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleLaunch, "gate.sock"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := initDaemonState(stateDir); err != nil {
		t.Fatalf("initDaemonState: %v", err)
	}
	entries, err := os.ReadDir(gatesDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected stale gate sockets swept on daemon start, err=%v entries=%v", err, entries)
	}

	if err := os.Chmod(gatesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := initDaemonState(stateDir); err == nil {
		t.Fatal("initDaemonState accepted unsafe 0755 gates directory")
	}
}
