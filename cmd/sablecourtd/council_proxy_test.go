package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		{"removed preflight", "HEAD", "/gate/_preflight", 403},
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

	_, lock, err := initDaemonState(stateDir)
	if err != nil {
		t.Fatalf("initDaemonState: %v", err)
	}
	entries, err := os.ReadDir(gatesDir)
	if err != nil || len(entries) != 0 {
		_ = lock.Close()
		t.Fatalf("expected stale gate sockets swept on daemon start, err=%v entries=%v", err, entries)
	}

	// While the first daemon holds the lock, a second initDaemonState must fail before sweeping.
	liveLaunch := filepath.Join(gatesDir, "g-live5678")
	if err := os.MkdirAll(liveLaunch, 0o700); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	liveSock := filepath.Join(liveLaunch, "gate.sock")
	if err := os.WriteFile(liveSock, []byte("live"), 0o600); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if _, _, err := initDaemonState(stateDir); err == nil {
		_ = lock.Close()
		t.Fatal("second initDaemonState succeeded while state directory was locked")
	}
	if _, err := os.Lstat(liveSock); err != nil {
		_ = lock.Close()
		t.Fatalf("locked initDaemonState swept live socket: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("release state lock: %v", err)
	}

	if err := os.Chmod(gatesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := initDaemonState(stateDir); err == nil {
		t.Fatal("initDaemonState accepted unsafe 0755 gates directory")
	}
}

func TestSecondDaemonFailsBeforeSweepingLiveSockets(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "sc-lock-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("SABLECOURT_STATE_DIR", stateDir)

	reserveLoopback := func(t *testing.T) string {
		t.Helper()
		ln, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}
	addr1 := reserveLoopback(t)
	addr2 := reserveLoopback(t)

	ctx1, cancel1 := context.WithCancel(t.Context())
	done1 := make(chan error, 1)
	go func() {
		defer close(done1)
		done1 <- runDaemon(ctx1, daemonConfig{
			dbPath:         filepath.Join(t.TempDir(), "d1.db"),
			listenAddr:     addr1,
			mode:           "sanctum",
			runtimeBackend: "none",
		})
	}()
	t.Cleanup(func() {
		cancel1()
		for range done1 {
		}
	})

	healthURL := "http://" + addr1 + "/healthz"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := client.Get(healthURL)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("first daemon did not become healthy in time")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Simulate an active per-launch socket owned by the running first daemon.
	gatesDir := councilproxy.SocketRoot(stateDir)
	liveDir := filepath.Join(gatesDir, "g-live1234")
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gatesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	liveSock := filepath.Join(liveDir, "gate.sock")
	if err := os.WriteFile(liveSock, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A second daemon targeting the same state directory must fail before sweeping,
	// whether it targets the same port or a different port.
	for _, secondAddr := range []string{addr1, addr2} {
		ctx2, cancel2 := context.WithTimeout(t.Context(), 2*time.Second)
		err := runDaemon(ctx2, daemonConfig{
			dbPath:         filepath.Join(t.TempDir(), "d2.db"),
			listenAddr:     secondAddr,
			mode:           "sanctum",
			runtimeBackend: "none",
		})
		cancel2()
		if err == nil {
			t.Fatalf("second daemon on %s succeeded while first daemon held state directory", secondAddr)
		}
		if _, statErr := os.Lstat(liveSock); statErr != nil {
			t.Fatalf("second daemon on %s swept live launch socket before failing: %v (daemon err: %v)", secondAddr, statErr, err)
		}
	}

	// Once the first daemon stops, a fresh daemon startup acquires the lock and sweeps stale sockets.
	cancel1()
	if err := <-done1; err != nil {
		t.Fatalf("first daemon shutdown: %v", err)
	}
	_, lock3, err := initDaemonState(stateDir)
	if err != nil {
		t.Fatalf("restart daemon state after lock release: %v", err)
	}
	_ = lock3.Close()
	if _, err := os.Lstat(liveSock); !os.IsNotExist(err) {
		t.Fatalf("expected stale socket swept after first daemon exited, got %v", err)
	}
}
