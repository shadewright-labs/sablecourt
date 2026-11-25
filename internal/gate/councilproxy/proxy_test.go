package councilproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
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

func ipv4LoopbackZero() string { return net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0") }

func fixtureSPKI() ([sha256.Size]byte, string) {
	var pin [sha256.Size]byte
	for i := range pin {
		pin[i] = byte(i + 1)
	}
	return pin, hex.EncodeToString(pin[:])
}

func decodeSPKIPin(t *testing.T, spkiHex string) [sha256.Size]byte {
	t.Helper()
	raw, err := hex.DecodeString(spkiHex)
	if err != nil || len(raw) != sha256.Size {
		t.Fatalf("decode SPKI hex %q: %v", spkiHex, err)
	}
	var pin [sha256.Size]byte
	copy(pin[:], raw)
	return pin
}

func TestGateProxyBoundary(t *testing.T) {
	const token = "sc_task_fixture"
	_, spkiHex := fixtureSPKI()
	gateAuthority := net.JoinHostPort(Authority, "7432")

	for _, tc := range []struct {
		name, method, target, bearer string
		status                       int
	}{
		{"mcp", "POST", "/gate/mcp", token, 204},
		{"messages", "GET", "/gate/conclave/room/messages?since=0", token, 204},
		{"operator", "GET", "/api/v1/tasks", token, 403},
		{"model", "POST", "/gate/model/openai/v1/responses", token, 403},
		{"preflight blocked from workload", "HEAD", "/gate/_preflight", token, 403},
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
				Harness:        "claude",
				DaemonListen:   loopbackAddress(),
				LeaseSHA256:    fmt.Sprintf("%x", sha256.Sum256([]byte(token))),
				GateTLSPort:    "9443",
				GateSPKISHA256: spkiHex,
			}
			if tc.name == "login cannot call gate" {
				cfg.LeaseSHA256 = ""
				cfg.GateTLSPort = ""
				cfg.GateSPKISHA256 = ""
			}
			gateCalls := 0
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				gateCalls++
				if r.URL.Scheme != "https" || r.URL.Host != net.JoinHostPort(DockerHostGateway, "9443") ||
					r.Host != gateAuthority || r.URL.RequestURI() != tc.target ||
					r.Header.Get("Authorization") != "Bearer "+token ||
					r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("Cookie") != "" {
					t.Error("gate request did not preserve the pinned HTTPS target and stripped headers")
				}
				status := http.StatusNoContent
				if tc.name == "redirect" {
					status = http.StatusTemporaryRedirect
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Location": {"/login"}, "Set-Cookie": {"s=1"}},
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
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			wantCalls := 0
			if tc.status == 204 || tc.status == 502 {
				wantCalls = 1
			}
			if w.Code != tc.status || gateCalls != wantCalls ||
				w.Header().Get("Location") != "" || w.Header().Get("Set-Cookie") != "" {
				t.Fatalf("status %d, gate calls %d, headers %v", w.Code, gateCalls, w.Header())
			}
		})
	}
}

func newEd25519Certificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{Authority},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, hex.EncodeToString(sum[:])
}

func TestPinnedTLSGateRejectsUnverifiedPeers(t *testing.T) {
	const token = "sc_task_secret_lease"
	leaseHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))

	validCert, validSPKI, err := newLaunchCertificate(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, wrongSPKI, err := newLaunchCertificate(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expiredCert, expiredSPKI, err := newLaunchCertificate(time.Now().Add(-3*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	futureCert, futureSPKI, err := newLaunchCertificate(time.Now().Add(time.Hour), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	edCert, edSPKI := newEd25519Certificate(t)

	for _, tc := range []struct {
		name      string
		pinnedHex string
		plainTCP  bool
		dialErr   bool
		tlsCfg    *tls.Config
	}{
		{
			name:      "plain TCP listener",
			pinnedHex: validSPKI,
			plainTCP:  true,
		},
		{
			name:      "wrong P-256 certificate SPKI",
			pinnedHex: wrongSPKI,
			tlsCfg: &tls.Config{
				Certificates: []tls.Certificate{validCert},
				MinVersion:   tls.VersionTLS13,
				MaxVersion:   tls.VersionTLS13,
			},
		},
		{
			name:      "TLS 1.2 server even with matching key",
			pinnedHex: validSPKI,
			tlsCfg: &tls.Config{
				Certificates: []tls.Certificate{validCert},
				MinVersion:   tls.VersionTLS12,
				MaxVersion:   tls.VersionTLS12,
			},
		},
		{
			name:      "expired certificate with matching SPKI",
			pinnedHex: expiredSPKI,
			tlsCfg: &tls.Config{
				Certificates: []tls.Certificate{expiredCert},
				MinVersion:   tls.VersionTLS13,
				MaxVersion:   tls.VersionTLS13,
			},
		},
		{
			name:      "not yet valid certificate with matching SPKI",
			pinnedHex: futureSPKI,
			tlsCfg: &tls.Config{
				Certificates: []tls.Certificate{futureCert},
				MinVersion:   tls.VersionTLS13,
				MaxVersion:   tls.VersionTLS13,
			},
		},
		{
			name:      "non-P256 Ed25519 certificate even with matching SPKI",
			pinnedHex: edSPKI,
			tlsCfg: &tls.Config{
				Certificates: []tls.Certificate{edCert},
				MinVersion:   tls.VersionTLS13,
				MaxVersion:   tls.VersionTLS13,
			},
		},
		{
			name:      "dial failure",
			pinnedHex: validSPKI,
			dialErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var leaked atomic.Int32
			var targetAddr string
			if !tc.dialErr {
				rawLn, err := net.Listen("tcp4", ipv4LoopbackZero())
				if err != nil {
					t.Fatal(err)
				}
				targetAddr = rawLn.Addr().String()
				if tc.plainTCP {
					done := make(chan struct{})
					go func() {
						defer close(done)
						for {
							c, err := rawLn.Accept()
							if err != nil {
								return
							}
							_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
							buf, _ := io.ReadAll(io.LimitReader(c, 8192))
							if bytes.Contains(buf, []byte(token)) || bytes.Contains(buf, []byte("Authorization")) {
								leaked.Add(1)
							}
							_, _ = io.WriteString(c, "HTTP/1.1 204 No Content\r\n\r\n")
							_ = c.Close()
						}
					}()
					t.Cleanup(func() {
						_ = rawLn.Close()
						<-done
					})
				} else {
					srv := &http.Server{
						ReadHeaderTimeout: time.Second,
						ErrorLog:          log.New(io.Discard, "", 0),
						Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
							if r.Header.Get("Authorization") != "" || strings.Contains(string(body), token) {
								leaked.Add(1)
							}
							w.WriteHeader(http.StatusOK)
						}),
					}
					done := make(chan struct{})
					go func() {
						defer close(done)
						_ = srv.Serve(tls.NewListener(rawLn, tc.tlsCfg))
					}()
					t.Cleanup(func() {
						_ = srv.Close()
						<-done
					})
				}
			}

			pin := decodeSPKIPin(t, tc.pinnedHex)
			transport := newTLSGateTransport(pin, func(ctx context.Context, network, _ string) (net.Conn, error) {
				if tc.dialErr {
					return nil, io.ErrClosedPipe
				}
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, targetAddr)
			})
			t.Cleanup(transport.CloseIdleConnections)

			// Verify plain HTTP cannot use this transport either.
			plainReq, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(DockerHostGateway, "7432")+"/gate/task/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			plainReq.Header.Set("Authorization", "Bearer "+token)
			if _, err := transport.RoundTrip(plainReq); err == nil {
				t.Fatal("expected plain HTTP RoundTrip to fail on TLS gate transport")
			}

			h, err := newProxy(Config{
				Harness:        "claude",
				DaemonListen:   loopbackAddress(),
				LeaseSHA256:    leaseHash,
				GateSPKISHA256: tc.pinnedHex,
			}, transport, nil, nil)
			if err != nil {
				t.Fatal(err)
			}

			r := httptest.NewRequest(http.MethodPost, "/gate/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0"}`))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadGateway || leaked.Load() != 0 {
				t.Fatalf("status=%d leaked=%d, want 502 and 0 leaks", w.Code, leaked.Load())
			}
		})
	}
}

func TestSubstitutedUpstreamReceivesZeroCredentials(t *testing.T) {
	const token = "sc_task_live_lease_regression"
	leaseHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	daemonAddr := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")
	gateAuthority, err := GateAuthority(daemonAddr)
	if err != nil {
		t.Fatal(err)
	}

	tg, err := startTLSGate(Config{
		Harness:      "claude",
		DaemonListen: daemonAddr,
		LeaseSHA256:  leaseHash,
		GateHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != gateAuthority || r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// Close after first response so subsequent calls must verify a fresh second connection.
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "daemon-ok")
		}),
	}, DefaultGateLimits())
	if err != nil {
		t.Fatal(err)
	}
	targetAddr := tg.addr
	pin := decodeSPKIPin(t, tg.spkiSHA256)

	transport := newTLSGateTransport(pin, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, targetAddr)
	})
	t.Cleanup(transport.CloseIdleConnections)

	proxyHandler, err := newProxy(Config{
		Harness:        "claude",
		DaemonListen:   daemonAddr,
		LeaseSHA256:    leaseHash,
		GateTLSPort:    tg.port,
		GateSPKISHA256: tg.spkiSHA256,
	}, transport, nil, nil)
	if err != nil {
		_ = tg.close()
		t.Fatal(err)
	}

	// First request succeeds over verified TLS 1.3 connection.
	req1 := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	rec1 := httptest.NewRecorder()
	proxyHandler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK || rec1.Body.String() != "daemon-ok" {
		_ = tg.close()
		t.Fatalf("initial daemon call failed: status=%d body=%q", rec1.Code, rec1.Body.String())
	}

	// End the launch: listener closes and private key is dropped in memory.
	if err := tg.close(); err != nil {
		t.Fatal(err)
	}
	if tg.cert.Load() != nil {
		t.Fatal("private key certificate remained loaded after launch close")
	}
	if c, err := tg.tlsConfig.GetCertificate(nil); err == nil || c != nil {
		t.Fatalf("closed tlsGate returned certificate %v err=%v", c, err)
	}

	// 1. Plain TCP replacement listener at the exact same address and port.
	plainLn, err := net.Listen("tcp4", targetAddr)
	if err != nil {
		t.Fatalf("bind plain substituted listener at %s: %v", targetAddr, err)
	}
	var credentialLeaks atomic.Int32
	plainDone := make(chan struct{})
	go func() {
		defer close(plainDone)
		for {
			c, err := plainLn.Accept()
			if err != nil {
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			raw, _ := io.ReadAll(io.LimitReader(c, 8192))
			if bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte("Authorization")) {
				credentialLeaks.Add(1)
			}
			_ = c.Close()
		}
	}()
	reqPlain := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
	reqPlain.Header.Set("Authorization", "Bearer "+token)
	recPlain := httptest.NewRecorder()
	proxyHandler.ServeHTTP(recPlain, reqPlain)
	_ = plainLn.Close()
	<-plainDone
	if recPlain.Code != http.StatusBadGateway || credentialLeaks.Load() != 0 {
		t.Fatalf("plain substituted listener status=%d leaks=%d", recPlain.Code, credentialLeaks.Load())
	}

	// 2. TLS 1.3 replacement listener with a different P-256 certificate on the exact same port (second connection).
	subCert, _, err := newLaunchCertificate(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	subLn, err := net.Listen("tcp4", targetAddr)
	if err != nil {
		t.Fatalf("bind TLS substituted listener at %s: %v", targetAddr, err)
	}
	subSrv := &http.Server{
		ReadHeaderTimeout: time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			if r.Header.Get("Authorization") != "" || strings.Contains(string(body), token) {
				credentialLeaks.Add(1)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	subDone := make(chan struct{})
	go func() {
		defer close(subDone)
		_ = subSrv.Serve(tls.NewListener(subLn, &tls.Config{
			Certificates: []tls.Certificate{subCert},
			MinVersion:   tls.VersionTLS13,
			MaxVersion:   tls.VersionTLS13,
		}))
	}()
	t.Cleanup(func() {
		_ = subSrv.Close()
		<-subDone
	})

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var body io.Reader
		target := "/gate/task/messages"
		if method == http.MethodPost {
			target = "/gate/mcp"
			body = strings.NewReader(`{"secret_context":"do-not-forward"}`)
		}
		req := httptest.NewRequest(method, target, body)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		proxyHandler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s to substituted TLS upstream returned %d, want 502", method, rec.Code)
		}
	}
	if credentialLeaks.Load() != 0 {
		t.Fatalf("substituted listener received %d credential-bearing requests, want 0", credentialLeaks.Load())
	}
}

func TestLiveRelayAndUnverifiedSecondConnectionReceiveZeroCredentials(t *testing.T) {
	const token = "sc_task_live_relay_check"
	leaseHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	daemonAddr := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")

	tg, err := startTLSGate(Config{
		Harness:      "claude",
		DaemonListen: daemonAddr,
		LeaseSHA256:  leaseHash,
		GateHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "daemon-ok")
		}),
	}, DefaultGateLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tg.close() })
	pin := decodeSPKIPin(t, tg.spkiSHA256)

	// A live TLS 1.3 intermediary terminates TLS with its own P-256 certificate and relays to the live daemon.
	mitmCert, _, err := newLaunchCertificate(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mitmLn, err := net.Listen("tcp4", ipv4LoopbackZero())
	if err != nil {
		t.Fatal(err)
	}
	var relayLeaks atomic.Int32
	upstreamTransport := newTLSGateTransport(pin, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, tg.addr)
	})
	t.Cleanup(upstreamTransport.CloseIdleConnections)

	mitmSrv := &http.Server{
		ReadHeaderTimeout: time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				relayLeaks.Add(1)
			}
			outReq := r.Clone(r.Context())
			outReq.RequestURI = ""
			outReq.URL.Scheme = "https"
			outReq.URL.Host = tg.addr
			res, err := upstreamTransport.RoundTrip(outReq)
			if err != nil {
				http.Error(w, "upstream err", http.StatusBadGateway)
				return
			}
			defer res.Body.Close()
			w.WriteHeader(res.StatusCode)
		}),
	}
	mitmDone := make(chan struct{})
	go func() {
		defer close(mitmDone)
		_ = mitmSrv.Serve(tls.NewListener(mitmLn, &tls.Config{
			Certificates: []tls.Certificate{mitmCert},
			MinVersion:   tls.VersionTLS13,
			MaxVersion:   tls.VersionTLS13,
		}))
	}()
	t.Cleanup(func() {
		_ = mitmSrv.Close()
		<-mitmDone
	})

	mitmAddr := mitmLn.Addr().String()
	proxyTransport := newTLSGateTransport(pin, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, mitmAddr)
	})
	t.Cleanup(proxyTransport.CloseIdleConnections)

	proxyHandler, err := newProxy(Config{
		Harness:        "claude",
		DaemonListen:   daemonAddr,
		LeaseSHA256:    leaseHash,
		GateTLSPort:    tg.port,
		GateSPKISHA256: tg.spkiSHA256,
	}, proxyTransport, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	proxyHandler.ServeHTTP(rec, req)
	if relayLeaks.Load() != 0 || rec.Code != http.StatusBadGateway {
		t.Fatalf("live TLS intermediary received lease or succeeded: leaks=%d status=%d", relayLeaks.Load(), rec.Code)
	}

	// Second connection hijacked while the original daemon is still live must also fail closed with zero leaks.
	t.Run("unverified second connection while daemon stays alive", func(t *testing.T) {
		tg2, err := startTLSGate(Config{
			Harness:      "claude",
			DaemonListen: daemonAddr,
			LeaseSHA256:  leaseHash,
			GateHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Connection", "close")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "daemon-first-conn")
			}),
		}, DefaultGateLimits())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tg2.close() })
		pin2 := decodeSPKIPin(t, tg2.spkiSHA256)

		var dialCount atomic.Int32
		redialTransport := newTLSGateTransport(pin2, func(ctx context.Context, network, _ string) (net.Conn, error) {
			target := tg2.addr
			if dialCount.Add(1) > 1 {
				target = mitmAddr
			}
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, target)
		})
		t.Cleanup(redialTransport.CloseIdleConnections)

		h2, err := newProxy(Config{
			Harness:        "claude",
			DaemonListen:   daemonAddr,
			LeaseSHA256:    leaseHash,
			GateTLSPort:    tg2.port,
			GateSPKISHA256: tg2.spkiSHA256,
		}, redialTransport, nil, nil)
		if err != nil {
			t.Fatal(err)
		}

		req1 := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
		req1.Header.Set("Authorization", "Bearer "+token)
		rec1 := httptest.NewRecorder()
		h2.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusOK || rec1.Body.String() != "daemon-first-conn" {
			t.Fatalf("first verified connection failed: status=%d body=%q", rec1.Code, rec1.Body.String())
		}

		req2 := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
		req2.Header.Set("Authorization", "Bearer "+token)
		rec2 := httptest.NewRecorder()
		h2.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusBadGateway || relayLeaks.Load() != 0 {
			t.Fatalf("unverified second connection status=%d leaks=%d, want 502 and 0 leaks", rec2.Code, relayLeaks.Load())
		}
	})
}

func TestUnixSocketProxyNeverDialsTCP(t *testing.T) {
	const token = "sc_task_unix_only"
	leaseHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))

	var tcpLeaks, tcpHits atomic.Int32
	tcpLn, err := net.Listen("tcp4", ipv4LoopbackZero())
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
	_, spkiHex := fixtureSPKI()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("sc_task_fixture")))
	for _, tc := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"login", Config{Harness: "agy", DaemonListen: loopbackAddress()}, true},
		{"unix socket mode", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSocket: ContainerGateSocket}, true},
		{"host gateway pinned TLS mode", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateTLSPort: "9443", GateSPKISHA256: spkiHex}, true},
		{"host gateway pinned TLS default port", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSPKISHA256: spkiHex}, true},
		{"lease without upstream pin or socket", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest}, false},
		{"both socket and spki", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSocket: ContainerGateSocket, GateSPKISHA256: spkiHex}, false},
		{"tls port without spki", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateTLSPort: "9443"}, false},
		{"invalid tls port", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateTLSPort: "0", GateSPKISHA256: spkiHex}, false},
		{"relative socket path", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSocket: "gate.sock"}, false},
		{"nonclean socket path", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSocket: "/tmp/../gate.sock"}, false},
		{"short spki digest", Config{Harness: "agy", DaemonListen: loopbackAddress(), LeaseSHA256: digest, GateSPKISHA256: "abcd"}, false},
		{"login with gate socket", Config{Harness: "agy", DaemonListen: loopbackAddress(), GateSocket: ContainerGateSocket}, false},
		{"login with gate spki", Config{Harness: "agy", DaemonListen: loopbackAddress(), GateSPKISHA256: spkiHex}, false},
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
