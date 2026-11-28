package main

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// Packages without a logger of their own (store.RecordAudit logs failed audit
// writes through slog.Default) write through the daemon's logger, also when
// the caller supplies that logger, and the previous default is restored when
// the daemon returns.
func TestRunDaemonSetsDefaultLoggerToSuppliedLogger(t *testing.T) {
	t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before := slog.Default()
	handler := &testLogHandler{}
	var during slog.Handler
	handler.onListening = func() {
		during = slog.Default().Handler()
		cancel()
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	if err := runDaemon(ctx, daemonConfig{
		dbPath:         filepath.Join(t.TempDir(), "sablecourt.db"),
		listenAddr:     addr,
		runtimeBackend: "k8s",
		logger:         slog.New(handler),
	}); err != nil {
		t.Fatalf("runDaemon = %v", err)
	}
	if during != slog.Handler(handler) {
		t.Errorf("default logger while running = %T, want the supplied logger", during)
	}
	if slog.Default() != before {
		t.Error("runDaemon left its logger as the default")
	}
}
