package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
)

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
	staleLaunch := filepath.Join(gatesDir, "g-0123456789abcdef")
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
	liveLaunch := filepath.Join(gatesDir, "g-fedcba9876543210")
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

func TestInitDaemonStateKeepsFilesThatAreNotLaunchDirectories(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "sc-keep-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	gatesDir := councilproxy.SocketRoot(stateDir)
	if err := os.Mkdir(gatesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(gatesDir, "g-0123456789abcdef")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	// An operator pointed --db at a file under the socket root.
	dbFile := filepath.Join(gatesDir, "sablecourt.db")
	if err := os.WriteFile(dbFile, []byte("task state"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, lock, err := initDaemonState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if data, err := os.ReadFile(dbFile); err != nil || string(data) != "task state" {
		t.Fatalf("startup sweep damaged the database file: %q %v", data, err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale launch directory survived the sweep: %v", err)
	}
}

func TestDaemonServerReleasesIdleConnections(t *testing.T) {
	srv := newDaemonServer(http.NotFoundHandler())
	if srv.IdleTimeout <= 0 || srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("idle=%v header=%v, want both finite", srv.IdleTimeout, srv.ReadHeaderTimeout)
	}
	// A write timeout would cut event streams that are still active.
	if srv.WriteTimeout != 0 {
		t.Fatalf("write timeout %v would cut active streams", srv.WriteTimeout)
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
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for healthy := false; !healthy; {
		res, err := client.Get(healthURL)
		if err == nil {
			_ = res.Body.Close()
			healthy = res.StatusCode == http.StatusOK
		}
		if healthy {
			break
		}
		select {
		case err := <-done1:
			t.Fatalf("first daemon exited before it became healthy: %v", err)
		case <-timeout.C:
			t.Fatal("first daemon did not become healthy in time")
		case <-tick.C:
		}
	}

	// Simulate an active per-launch socket owned by the running first daemon.
	gatesDir := councilproxy.SocketRoot(stateDir)
	liveDir := filepath.Join(gatesDir, "g-00112233445566aa")
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
