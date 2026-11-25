package councilproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func loopbackAddress() string { return net.JoinHostPort(net.IPv6loopback.String(), "7432") }

func fixtureSecret() ([32]byte, string) {
	var secret [32]byte
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	return secret, hex.EncodeToString(secret[:])
}

func TestGateProxyBoundary(t *testing.T) {
	const token = "sc_task_fixture"
	const launchID = "sablecourt-council-fixture"
	secret, secretHex := fixtureSecret()
	gateAuthority := net.JoinHostPort(Authority, "7432")

	for _, tc := range []struct {
		name, method, target, bearer string
		status                       int
	}{
		{"mcp", "POST", "/gate/mcp", token, 204},
		{"messages", "GET", "/gate/conclave/room/messages?since=0", token, 204},
		{"operator", "GET", "/api/v1/tasks", token, 403},
		{"model", "POST", "/gate/model/openai/v1/responses", token, 403},
		{"preflight blocked from workload", "HEAD", PreflightPath, token, 403},
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
			cfg := Config{
				Harness:      "claude",
				DaemonListen: loopbackAddress(),
				LeaseSHA256:  fmt.Sprintf("%x", sha256.Sum256([]byte(token))),
				GateLaunchID: launchID,
				GateSecret:   secretHex,
			}
			if tc.name == "login cannot call gate" {
				cfg.LeaseSHA256 = ""
				cfg.GateLaunchID = ""
				cfg.GateSecret = ""
			}
			preflights, gateCalls := 0, 0
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodHead && r.URL.Path == PreflightPath {
					preflights++
					if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" ||
						r.Host != gateAuthority || r.Header.Get(LaunchHeader) != launchID {
						t.Errorf("unexpected preflight request headers or host: %+v host=%q", r.Header, r.Host)
					}
					challenge := r.Header.Get(ChallengeHeader)
					proof := ComputeGateProof(secret, gateAuthority, launchID, challenge)
					return &http.Response{
						StatusCode: http.StatusNoContent,
						Header:     http.Header{ProofHeader: {proof}},
						Body:       http.NoBody,
					}, nil
				}
				gateCalls++
				if r.URL.Scheme != "http" || r.URL.Host != net.JoinHostPort(DockerHostGateway, "7432") ||
					r.Host != gateAuthority || r.URL.RequestURI() != tc.target ||
					r.Header.Get("Authorization") != "Bearer "+token ||
					r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("Cookie") != "" ||
					r.Header.Get(LaunchHeader) != "" || r.Header.Get(ChallengeHeader) != "" || r.Header.Get(ProofHeader) != "" {
					t.Error("gate request did not preserve the pinned target and stripped headers")
				}
				status := http.StatusNoContent
				if tc.name == "redirect" {
					status = http.StatusTemporaryRedirect
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Location": {"/login"}, ProofHeader: {"strip-me"}},
					Body:       http.NoBody,
				}, nil
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
			r.Header.Set(ChallengeHeader, "untrusted")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			wantCalls := 0
			if tc.status == 204 || tc.status == 502 {
				wantCalls = 1
			}
			if w.Code != tc.status || preflights != wantCalls || gateCalls != wantCalls ||
				w.Header().Get("Location") != "" || w.Header().Get(ProofHeader) != "" {
				t.Fatalf("status %d, preflights %d, gate calls %d, headers %v", w.Code, preflights, gateCalls, w.Header())
			}
		})
	}
}

func TestGatePreflightRefusesUnverifiedUpstream(t *testing.T) {
	const token = "sc_task_secret_lease"
	const launchID = "sablecourt-council-preflight"
	secret, secretHex := fixtureSecret()
	var wrongSecret [32]byte
	gateAuthority := net.JoinHostPort(Authority, "7432")

	for _, tc := range []struct {
		name  string
		reply func(challenge string) (*http.Response, error)
	}{
		{"missing proof header", func(_ string) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}, nil
		}},
		{"echoed challenge instead of hmac", func(challenge string) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{ProofHeader: {challenge}}, Body: http.NoBody}, nil
		}},
		{"wrong secret hmac", func(challenge string) (*http.Response, error) {
			proof := ComputeGateProof(wrongSecret, gateAuthority, launchID, challenge)
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{ProofHeader: {proof}}, Body: http.NoBody}, nil
		}},
		{"wrong status code", func(challenge string) (*http.Response, error) {
			proof := ComputeGateProof(secret, gateAuthority, launchID, challenge)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{ProofHeader: {proof}}, Body: http.NoBody}, nil
		}},
		{"multiple proof headers", func(challenge string) (*http.Response, error) {
			proof := ComputeGateProof(secret, gateAuthority, launchID, challenge)
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{ProofHeader: {proof, proof}}, Body: http.NoBody}, nil
		}},
		{"transport failure", func(_ string) (*http.Response, error) {
			return nil, io.ErrClosedPipe
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var leakedCredentials atomic.Int32
			var forwardedGateCalls atomic.Int32
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "" || strings.Contains(r.URL.String(), token) {
					leakedCredentials.Add(1)
				}
				if r.Method == http.MethodHead && r.URL.Path == PreflightPath {
					return tc.reply(r.Header.Get(ChallengeHeader))
				}
				forwardedGateCalls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})
			h, err := newProxy(Config{
				Harness:      "claude",
				DaemonListen: loopbackAddress(),
				LeaseSHA256:  fmt.Sprintf("%x", sha256.Sum256([]byte(token))),
				GateLaunchID: launchID,
				GateSecret:   secretHex,
			}, rt, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/gate/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0"}`))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadGateway || leakedCredentials.Load() != 0 || forwardedGateCalls.Load() != 0 {
				t.Fatalf("status=%d leaked=%d forwarded=%d", w.Code, leakedCredentials.Load(), forwardedGateCalls.Load())
			}
		})
	}
}

func TestSubstitutedUpstreamReceivesZeroCredentials(t *testing.T) {
	const token = "sc_task_live_lease_regression"
	const launchID = "sablecourt-council-regression"
	secret, secretHex := fixtureSecret()
	leaseHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))

	ln, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	daemonAddr := ln.Addr().String()
	_, port, err := net.SplitHostPort(daemonAddr)
	if err != nil {
		t.Fatal(err)
	}
	gateAuthority, err := GateAuthority(daemonAddr)
	if err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	reg.Register(gateAuthority, launchID, secret)
	daemonSrv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead && r.URL.Path == PreflightPath {
				reg.HandlePreflight(w, r, gateAuthority)
				return
			}
			if r.Host != gateAuthority || r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("daemon-ok"))
		}),
	}
	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		_ = daemonSrv.Serve(ln)
	}()

	dialLoopback := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), port))
	}
	transport := &http.Transport{DialContext: dialLoopback}
	t.Cleanup(transport.CloseIdleConnections)

	proxyHandler, err := newProxy(Config{
		Harness:      "claude",
		DaemonListen: daemonAddr,
		LeaseSHA256:  leaseHash,
		GateLaunchID: launchID,
		GateSecret:   secretHex,
	}, transport, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// First request succeeds while the original daemon owns the address.
	req1 := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	rec1 := httptest.NewRecorder()
	proxyHandler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK || rec1.Body.String() != "daemon-ok" {
		t.Fatalf("initial daemon call failed: status=%d body=%q", rec1.Code, rec1.Body.String())
	}

	// Daemon exits and releases the address; an unrelated listener takes the exact same address and port.
	_ = daemonSrv.Close()
	<-daemonDone

	subLn, err := net.Listen("tcp4", daemonAddr)
	if err != nil {
		t.Fatalf("bind substituted listener at %s: %v", daemonAddr, err)
	}
	var credentialLeaks atomic.Int32
	var preflightProbes atomic.Int32
	subSrv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			if r.Header.Get("Authorization") != "" || strings.Contains(string(body), token) {
				credentialLeaks.Add(1)
			}
			if ch := r.Header.Get(ChallengeHeader); ch != "" {
				preflightProbes.Add(1)
				// Even attempting to echo the challenge back must fail verification.
				w.Header().Set(ProofHeader, ch)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	subDone := make(chan struct{})
	go func() {
		defer close(subDone)
		_ = subSrv.Serve(subLn)
	}()
	t.Cleanup(func() {
		_ = subSrv.Close()
		<-subDone
	})

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader(`{"secret_context":"do-not-forward"}`)
		}
		target := "/gate/task/messages"
		if method == http.MethodPost {
			target = "/gate/mcp"
		}
		req := httptest.NewRequest(method, target, body)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		proxyHandler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s to substituted upstream returned %d, want 502", method, rec.Code)
		}
	}
	if credentialLeaks.Load() != 0 {
		t.Fatalf("substituted listener received %d credential-bearing requests, want 0", credentialLeaks.Load())
	}
	if preflightProbes.Load() == 0 {
		t.Fatal("expected surviving proxy to probe substituted listener before refusing")
	}
}

func TestUnixSocketProxyNeverDialsTCP(t *testing.T) {
	const token = "sc_task_unix_only"
	leaseHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))

	var tcpLeaks, tcpHits atomic.Int32
	tcpLn, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr := tcpLn.Addr().String()
	tcpSrv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tcpHits.Add(1)
			if r.Header.Get("Authorization") != "" {
				tcpLeaks.Add(1)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	tcpDone := make(chan struct{})
	go func() {
		defer close(tcpDone)
		_ = tcpSrv.Serve(tcpLn)
	}()
	t.Cleanup(func() {
		_ = tcpSrv.Close()
		<-tcpDone
	})

	rootDir, err := os.MkdirTemp("/tmp", "sc-px-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	sockPath := filepath.Join(rootDir, "gate.sock")

	uln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	gateAuthority, err := GateAuthority(tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != gateAuthority || r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(uln)
	}()

	h, err := New(Config{
		Harness:      "codex",
		DaemonListen: tcpAddr,
		LeaseSHA256:  leaseHash,
		GateSocket:   sockPath,
	})
	if err != nil {
		_ = srv.Close()
		<-done
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unix socket gate status=%d, want 204", rec.Code)
	}

	// Close and remove the daemon's Unix socket while tcpSrv still listens on DaemonListen.
	_ = srv.Close()
	<-done
	_ = os.Remove(sockPath)

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("post-close unix socket proxy status=%d, want 502", rec2.Code)
	}
	if tcpHits.Load() != 0 || tcpLeaks.Load() != 0 {
		t.Fatalf("unix socket proxy dialed TCP fallback: hits=%d leaks=%d", tcpHits.Load(), tcpLeaks.Load())
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
	_, secretHex := fixtureSecret()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("sc_task_fixture")))
	for _, tc := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"login", Config{Harness: "agy", DaemonListen: loopbackAddress()}, true},
		{"unix socket mode", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSocket: ContainerGateSocket}, true},
		{"host gateway mode", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateLaunchID: "launch-1", GateSecret: secretHex}, true},
		{"lease without upstream auth or socket", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest}, false},
		{"both socket and secret", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSocket: ContainerGateSocket, GateLaunchID: "launch-1", GateSecret: secretHex}, false},
		{"relative socket path", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSocket: "gate.sock"}, false},
		{"nonclean socket path", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSocket: "/tmp/../gate.sock"}, false},
		{"short secret", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateLaunchID: "launch-1", GateSecret: "abcd"}, false},
		{"missing launch id", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSecret: secretHex}, false},
		{"login with gate socket", Config{Harness: "agy", DaemonListen: loopbackAddress(), GateSocket: ContainerGateSocket}, false},
		{"unknown harness", Config{Harness: "other", DaemonListen: loopbackAddress()}, false},
		{"dial override", Config{Harness: "agy", DaemonListen: net.JoinHostPort("unlisted", "7432")}, false},
		{"unspecified address", Config{Harness: "agy", DaemonListen: net.JoinHostPort(net.IPv6unspecified.String(), "7432")}, false},
		{"invalid port", Config{Harness: "agy", DaemonListen: net.JoinHostPort(net.IPv6loopback.String(), "0")}, false},
		{"raw lease", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: "sc_task_fixture", GateSocket: ContainerGateSocket}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
