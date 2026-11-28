package main

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
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

// slog.SetDefault also points the standard log package at the daemon's
// handler and clears its flags. Both come back when the daemon returns, so a
// later log.Print does not reach the daemon's handler.
func TestRunDaemonRestoresStandardLogOutput(t *testing.T) {
	t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	savedOut, savedFlags := log.Writer(), log.Flags()
	t.Cleanup(func() { log.SetOutput(savedOut); log.SetFlags(savedFlags) })
	var buf bytes.Buffer
	const flags = log.Ldate | log.Lmicroseconds
	log.SetOutput(&buf)
	log.SetFlags(flags)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler := &testLogHandler{onListening: cancel}
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
	const marker = "after-daemon-marker"
	log.Print(marker)
	if got := log.Flags(); got != flags {
		t.Errorf("log flags after return = %d, want %d", got, flags)
	}
	if !strings.Contains(buf.String(), marker) {
		t.Errorf("log output after return did not reach the earlier writer: %q", buf.String())
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	for _, e := range handler.entries {
		if strings.Contains(e.msg, marker) {
			t.Errorf("log output after return reached the daemon's handler: %+v", e)
		}
	}
}
