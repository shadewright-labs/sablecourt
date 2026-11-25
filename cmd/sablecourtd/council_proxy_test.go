package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
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
