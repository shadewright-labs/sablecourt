package councilproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func loopbackAddress() string { return net.JoinHostPort(net.IPv6loopback.String(), "7432") }

func TestGateProxyBoundary(t *testing.T) {
	const token = "sc_task_fixture"
	for _, tc := range []struct {
		name, method, target, bearer string
		status                       int
	}{
		{"mcp", "POST", "/gate/mcp", token, 204},
		{"messages", "GET", "/gate/conclave/room/messages?since=0", token, 204},
		{"operator", "GET", "/api/v1/tasks", token, 403},
		{"model", "POST", "/gate/model/openai/v1/responses", token, 403},
		{"unknown", "POST", "/gate/other", token, 403},
		{"verb", "PUT", "/gate/task/messages", token, 403},
		{"absolute", "GET", "http://unlisted/gate/task/messages", token, 403},
		{"cleaned path", "GET", "/gate/task/../task/messages", token, 403},
		{"escaped path", "GET", "/gate/conclave/room%2Fother/messages", token, 403},
		{"missing lease", "GET", "/gate/task/messages", "", 401},
		{"wrong lease", "GET", "/gate/task/messages", "sc_task_other", 401},
		{"redirect", "GET", "/gate/task/messages?redirect", token, 502},
		{"login cannot call gate", "GET", "/gate/task/messages?login", token, 401},
		{"repeated bearer", "GET", "/gate/task/messages?repeat", token, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Harness: "claude", DaemonListen: loopbackAddress(), LeaseSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(token)))}
			if tc.name == "login cannot call gate" {
				cfg.LeaseSHA256 = ""
			}
			calls := 0
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme != "http" || r.URL.Host != net.JoinHostPort(DockerHostGateway, "7432") ||
					r.Host != net.JoinHostPort(Authority, "7432") || r.URL.RequestURI() != tc.target ||
					r.Header.Get("Authorization") != "Bearer "+token ||
					r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("Cookie") != "" {
					t.Error("gate request did not preserve the pinned target and bearer")
				}
				status := http.StatusNoContent
				if tc.name == "redirect" {
					status = http.StatusTemporaryRedirect
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"/login"}}, Body: http.NoBody}, nil
			})
			h, err := newProxy(cfg, rt, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tc.method, tc.target, nil)
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.name == "repeated bearer" {
				r.Header.Add("Authorization", "Bearer "+token)
			}
			r.Header.Set("Cookie", "operator=fixture")
			r.Header.Set("X-Forwarded-Host", "unlisted")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			wantCalls := 0
			if tc.status == 204 || tc.status == 502 {
				wantCalls = 1
			}
			if w.Code != tc.status || calls != wantCalls || w.Header().Get("Location") != "" {
				t.Fatalf("status %d, upstream calls %d, Location %q", w.Code, calls, w.Header().Get("Location"))
			}
		})
	}
}

func TestProviderProxyBoundary(t *testing.T) {
	// Addresses are constructed so the tests need no resolver or external service.
	public := netip.AddrFrom4([4]byte{8, 8, 4, 4})
	private := netip.AddrFrom4([4]byte{10, 0, 0, 1})
	for _, tc := range []struct {
		name, harness, method, host, port string
		resolved                          []netip.Addr
		status                            int
		dial                              bool
	}{
		{"anthropic inference", "claude", "CONNECT", "api.anthropic.com", "443", []netip.Addr{public}, 502, true},
		{"anthropic login", "claude", "CONNECT", "claude.ai", "443", []netip.Addr{public}, 502, true},
		{"openai login", "codex", "CONNECT", "auth.openai.com", "443", []netip.Addr{public}, 502, true},
		{"openai inference", "codex", "CONNECT", "api.openai.com", "443", []netip.Addr{public}, 502, true},
		{"google login", "agy", "CONNECT", "accounts.google.com", "443", []netip.Addr{public}, 502, true},
		{"google inference", "agy", "CONNECT", "play.googleapis.com", "443", []netip.Addr{public}, 502, true},
		{"cross-provider", "agy", "CONNECT", "claude.ai", "443", nil, 403, false},
		{"wrong port", "claude", "CONNECT", "claude.ai", "80", nil, 403, false},
		{"userinfo", "claude", "CONNECT", "user@claude.ai", "443", nil, 403, false},
		{"empty query", "claude", "CONNECT", "claude.ai", "443?", nil, 403, false},
		{"suffix", "claude", "CONNECT", "claude.ai.unlisted", "443", nil, 403, false},
		{"arbitrary host port", "claude", "CONNECT", DockerHostGateway, "7433", nil, 403, false},
		{"absolute HTTP", "claude", "GET", "claude.ai", "80", nil, 403, false},
		{"private DNS answer", "claude", "CONNECT", "claude.ai", "443", []netip.Addr{public, private}, 502, false},
		{"no DNS answer", "claude", "CONNECT", "claude.ai", "443", nil, 502, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups, dials := 0, 0
			lookup := func(_ context.Context, _ string, name string) ([]netip.Addr, error) {
				lookups++
				if name != tc.host+"." {
					t.Errorf("resolved %q instead of %q", name, tc.host)
				}
				return tc.resolved, nil
			}
			dial := func(_ context.Context, network, address string) (net.Conn, error) {
				dials++
				if network != "tcp" || address != net.JoinHostPort(public.String(), "443") {
					t.Error("dial did not use the validated address")
				}
				return nil, io.ErrClosedPipe
			}
			h, err := newProxy(Config{Harness: tc.harness, DaemonListen: loopbackAddress()}, nil, lookup, dial)
			if err != nil {
				t.Fatal(err)
			}
			target := net.JoinHostPort(tc.host, tc.port)
			if tc.method != "CONNECT" {
				target = "http://" + target + "/"
			}
			r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(tc.method + " " + target + " HTTP/1.1\r\nHost: " + net.JoinHostPort(tc.host, tc.port) + "\r\n\r\n")))
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || (dials == 1) != tc.dial || (lookups == 0) != (tc.status == 403) {
				t.Fatalf("status %d, lookups %d, dials %d", w.Code, lookups, dials)
			}
		})
	}
}

func TestProxyConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, harness, listen, gatePort, digest string
		ok                                      bool
	}{
		{"login", "agy", loopbackAddress(), "", "", true},
		{"gate port override", "agy", loopbackAddress(), "17432", "", true},
		{"unknown harness", "other", loopbackAddress(), "", "", false},
		{"dial override", "agy", net.JoinHostPort("unlisted", "7432"), "", "", false},
		{"unspecified address", "agy", net.JoinHostPort(net.IPv6unspecified.String(), "7432"), "", "", false},
		{"invalid port", "agy", net.JoinHostPort(net.IPv6loopback.String(), "0"), "", "", false},
		{"invalid gate port zero", "agy", loopbackAddress(), "0", "", false},
		{"invalid gate port range", "agy", loopbackAddress(), "65536", "", false},
		{"invalid gate port leading zero", "agy", loopbackAddress(), "07432", "", false},
		{"invalid gate port non-numeric", "agy", loopbackAddress(), "abc", "", false},
		{"raw lease", "agy", loopbackAddress(), "", "sc_task_fixture", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{Harness: tc.harness, DaemonListen: tc.listen, GatePort: tc.gatePort, LeaseSHA256: tc.digest})
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestGatePortPreservesPinnedAuthority(t *testing.T) {
	const token = "sc_task_fixture"
	cfg := Config{
		Harness:      "claude",
		DaemonListen: loopbackAddress(),
		GatePort:     "17432",
		LeaseSHA256:  fmt.Sprintf("%x", sha256.Sum256([]byte(token))),
	}
	called := false
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.Host != net.JoinHostPort(DockerHostGateway, "17432") ||
			r.Host != net.JoinHostPort(Authority, "7432") {
			t.Fatalf("unexpected upstream host=%q authority=%q", r.URL.Host, r.Host)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}, nil
	})
	h, err := newProxy(cfg, rt, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/gate/task/messages", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || !called {
		t.Fatalf("status %d called %v", w.Code, called)
	}
}
