package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
)

func TestDaemonServerReadTimeoutIsFiniteAndLeavesWritesAlone(t *testing.T) {
	srv := newDaemonServer(http.NotFoundHandler())
	if srv.ReadTimeout <= 0 {
		t.Fatalf("ReadTimeout = %v, want a finite timeout", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want none so streaming responses are not cut", srv.WriteTimeout)
	}
}

func TestDaemonReadTimeoutFreesSlotHeldByStalledBody(t *testing.T) {
	old := daemonReadTimeout
	daemonReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { daemonReadTimeout = old })

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatal(err)
	}
	// One slot: a stalled rejected request holds it until the timeout fires.
	srv := newDaemonServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected", http.StatusForbidden)
	}))
	go func() { _ = srv.Serve(councilproxy.LimitListener(ln, 1)) }()
	t.Cleanup(func() { _ = srv.Close() })

	stalled, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	_, err = io.WriteString(stalled, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabcde")
	if err != nil {
		t.Fatal(err)
	}

	// The slot must come back after the read timeout, not stay held indefinitely.
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(600 * time.Millisecond))
		_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		_ = conn.Close()
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot still held by a stalled body after the read timeout: %v", err)
		}
	}
}

func TestDaemonReadTimeoutDoesNotCutStreamingResponse(t *testing.T) {
	old := daemonReadTimeout
	daemonReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { daemonReadTimeout = old })

	const chunks = 12
	ctxErr := make(chan error, 1)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			if _, err := fmt.Fprintf(w, "chunk %d\n", i); err != nil {
				ctxErr <- err
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				ctxErr <- r.Context().Err()
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		ctxErr <- nil
	}))
	ts.Config = newDaemonServer(ts.Config.Handler)
	ts.Start()
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("streaming response cut: %v", err)
	}
	if got := strings.Count(string(body), "chunk "); got != chunks {
		t.Fatalf("received %d of %d chunks", got, chunks)
	}
	if err := <-ctxErr; err != nil {
		t.Fatalf("handler saw the stream cut: %v", err)
	}
}

func TestCouncilGateVirtualHostIsNotOnTheNetworkMux(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1).String()
	authority := net.JoinHostPort(councilproxy.Authority, "7432")
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	for _, unixSocket := range []bool{false, true} {
		for _, extra := range [][]string{nil, {authority}} {
			cfg := daemonConfig{listenAddr: net.JoinHostPort(ip, "7432"), allowedHosts: extra}
			h, err := cfg.hostHandler(next, unixSocket)
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"/gate/mcp", "/gate/task/messages", "/api/v1/tasks", "/healthz"} {
				r := httptest.NewRequest("POST", target, strings.NewReader("body"))
				r.Host = authority
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden || reached {
					t.Errorf("unix=%v extra=%v %s: status %d, reached=%v; the council host must not be served on a listener", unixSocket, extra, target, w.Code, reached)
				}
			}
		}
	}
}

func TestCouncilGateHandlerDispatchesInProcess(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1).String()
	authority := net.JoinHostPort(councilproxy.Authority, "7432")
	cfg := daemonConfig{listenAddr: net.JoinHostPort(ip, "7432")}
	h, err := cfg.councilGateHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, target, host string
		want                       int
	}{
		{"mcp", "POST", "/gate/mcp", authority, 204},
		{"messages", "GET", "/gate/conclave/room/messages?since=0", authority, 204},
		{"task", "GET", "/gate/task/messages", authority, 204},
		{"action", "POST", "/gate/actions/git.create_branch", authority, 204},
		{"operator", "GET", "/api/v1/tasks", authority, 403},
		{"login", "POST", "/login", authority, 403},
		{"model", "POST", "/gate/model/openai/v1/responses", authority, 403},
		{"unknown gate", "GET", "/gate/unknown", authority, 403},
		{"wrong method", "PUT", "/gate/task/messages", authority, 403},
		{"noncanonical", "GET", "/gate/task/../task/messages", authority, 403},
		{"escaped slash", "GET", "/gate/conclave/room%2Fother/messages", authority, 403},
		{"removed preflight", "HEAD", "/gate/_preflight", authority, 403},
		{"wrong authority", "POST", "/gate/mcp", net.JoinHostPort("localhost", "7432"), 403},
		{"wrong port", "POST", "/gate/mcp", net.JoinHostPort(councilproxy.Authority, "7433"), 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, nil)
			r.Host = tc.host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestCouncilGateHandlerRequiresLoopbackDaemon(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		wantErr    bool
	}{
		{"loopback", net.IPv6loopback.String(), false},
		{"unspecified", net.IPv6unspecified.String(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := daemonConfig{listenAddr: net.JoinHostPort(tc.host, "7432")}
			_, err := cfg.councilGateHandler(http.NotFoundHandler())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}
