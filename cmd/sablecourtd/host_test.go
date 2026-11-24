package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostAllowlistBeforeRouting(t *testing.T) {
	port := "7432"
	loopback := net.IPv4(127, 0, 0, 1).String()
	extra := "court" + ".invalid"
	untrusted := "attacker" + ".invalid"
	h, err := hostAllowlistMiddleware(newHandler(daemonTestStore(t), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil), net.JoinHostPort(loopback, port), []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		host   string
		path   string
		method string
		want   int
	}{
		{"loopback name", net.JoinHostPort("localhost", port), "/healthz", "GET", 200},
		{"loopback address", net.JoinHostPort(loopback, port), "/healthz", "GET", 200},
		{"ipv6 loopback", net.JoinHostPort(net.IPv6loopback.String(), port), "/healthz", "GET", 200},
		{"configured", net.JoinHostPort(extra, port), "/healthz", "GET", 200},
		{"case insensitive", net.JoinHostPort(strings.ToUpper(extra), port), "/healthz", "GET", 200},
		{"wrong port", net.JoinHostPort(loopback, "7433"), "/healthz", "GET", 403},
		{"missing port", "localhost", "/healthz", "GET", 403},
		{"empty", "", "/healthz", "GET", 403},
		{"trailing dot", net.JoinHostPort(extra+".", port), "/healthz", "GET", 403},
		{"forwarded host", net.JoinHostPort(untrusted, port), "/healthz", "GET", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Host = tc.host
			r.Header.Set("X-Forwarded-Host", net.JoinHostPort("localhost", port))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
	for _, unixSocket := range []bool{false, true} {
		guard, err := hostAllowlistMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), net.JoinHostPort(loopback, port), nil, unixSocket)
		if err != nil {
			t.Fatal(err)
		}
		for _, authority := range []string{"unix", net.JoinHostPort(untrusted, port)} {
			r := httptest.NewRequest("GET", "/healthz", nil)
			r.Host = authority
			w := httptest.NewRecorder()
			guard.ServeHTTP(w, r)
			want := 403
			if unixSocket && authority == "unix" {
				want = 200
			}
			if w.Code != want {
				t.Errorf("Unix transport %v: status = %d, want %d", unixSocket, w.Code, want)
			}
		}
	}
	for _, path := range []string{
		"/", "/healthz", "/metrics", "/.well-known/agent-card.json", "/a2a", "/missing", "/ui/tasks",
		"/api/v1/tasks", "/api/v1/stream", "/api/v1/hooks/github", "/api/v1/integrations/shadewright/incidents",
		"/api/v1/conclaves", "/api/v1/devices/pair/complete", "/gate/model/v1/chat/completions",
		"/gate/mcp", "/gate/actions/run", "/gate/task/messages", "/gate/conclave/session/messages",
	} {
		for _, method := range []string{"GET", "POST", "OPTIONS"} {
			t.Run(method+path, func(t *testing.T) {
				r := httptest.NewRequest(method, path, nil)
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
				r.Host = net.JoinHostPort(untrusted, port)
				r.Header.Set("Origin", "http://"+r.Host)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", w.Code)
				}
			})
		}
	}
}

func TestHostAllowlistConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen string
		extra  []string
	}{
		{"missing listen port", "localhost", nil},
		{"invalid listen port", ":bad", nil},
		{"zero listen port", ":0", nil},
		{"empty entry", ":7432", []string{""}},
		{"wildcard", ":7432", []string{"*"}},
		{"url", ":7432", []string{"http://court"}},
		{"path", ":7432", []string{"court/path"}},
		{"userinfo", ":7432", []string{"user@court"}},
		{"port too large", ":7432", []string{"court:65536"}},
		{"whitespace", ":7432", []string{" court "}},
		{"empty label", ":7432", []string{"court..invalid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := hostAllowlistMiddleware(http.NotFoundHandler(), tc.listen, tc.extra)
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestContainerGateAuthority(t *testing.T) {
	for _, host := range []string{"", net.JoinHostPort("gate", "8888")} {
		cfg := daemonConfig{listenAddr: net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432"), gateHost: host}
		h, err := cfg.hostHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			path string
			want int
		}{{"/gate/mcp", 204}, {"/api/v1/tasks", 403}} {
			r := httptest.NewRequest("GET", tc.path, nil)
			r.Host = cfg.containerGateHost()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("host %q path %s: got %d want %d", host, tc.path, w.Code, tc.want)
			}
		}
	}
}
