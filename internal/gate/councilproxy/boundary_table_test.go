package councilproxy

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAllowsGateRequestTable(t *testing.T) {
	cases := []struct {
		method string
		target string
		want   bool
	}{
		{http.MethodPost, "/gate/mcp", true},
		{http.MethodDelete, "/gate/mcp", true},
		{http.MethodPut, "/gate/mcp", false},
		{http.MethodGet, "/gate/task/messages", true},
		{http.MethodDelete, "/gate/task/messages", false},
		{http.MethodPost, "/gate/actions/git.open_pr", true},
		{http.MethodGet, "/gate/actions/git.open_pr", false},
		{http.MethodPost, "/gate/actions/", false},
		{http.MethodPost, "/gate/actions/a/b", false},
		{http.MethodGet, "/gate/conclave/c1/messages", true},
		{http.MethodPost, "/gate/conclave/c1/messages", true},
		{http.MethodDelete, "/gate/conclave/c1/messages", false},
		{http.MethodPost, "/gate/conclave/c1/relics", true},
		{http.MethodGet, "/gate/conclave/c1/relics", false},
		{http.MethodGet, "/gate/conclave/c1/relics/notes.txt", true},
		{http.MethodPost, "/gate/conclave/c1/relics/notes.txt", false},
		{http.MethodGet, "/gate/conclave/c1/relics/", false},
		{http.MethodGet, "/gate/conclave/c1/relics/a/b", false},
		{http.MethodGet, "/gate/conclave/c1", false},
		{http.MethodGet, "/gate/conclave//messages", false},
		{http.MethodGet, "/gate/conclave/c1/other", false},
		{http.MethodGet, "/api/v1/tasks", false},
		{http.MethodGet, "/gate/../api/v1/tasks", false},
		{http.MethodGet, "/gate/mcp/", false},
		{http.MethodGet, "/gate/%6Dcp", false},
		{http.MethodPost, "/gate/actions/a%2Fb", false},
		{http.MethodGet, "/gate/actions\\x", false},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://"+Authority+"/", nil)
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			r.URL = u
			if got := AllowsGateRequest(r); got != tc.want {
				t.Fatalf("AllowsGateRequest(%s %s) = %v, want %v", tc.method, tc.target, got, tc.want)
			}
		})
	}

	for name, u := range map[string]*url.URL{
		"absolute":  {Scheme: "http", Host: "evil.example", Path: "/gate/mcp"},
		"host only": {Host: "evil.example", Path: "/gate/mcp"},
		"opaque":    {Opaque: "/gate/mcp"},
		"user info": {User: url.User("x"), Path: "/gate/mcp"},
		"nil":       nil,
	} {
		r := httptest.NewRequest(http.MethodPost, "/gate/mcp", nil)
		r.URL = u
		if AllowsGateRequest(r) {
			t.Errorf("%s URL allowed", name)
		}
	}
}

type certSpec struct {
	key       crypto.Signer
	dnsName   string
	notBefore time.Time
	notAfter  time.Time
}

func makeCert(t *testing.T, spec certSpec) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: spec.dnsName},
		DNSNames:     []string{spec.dnsName},
		NotBefore:    spec.notBefore,
		NotAfter:     spec.notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, spec.key.Public(), spec.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func spkiPin(t *testing.T, der []byte) [sha256.Size]byte {
	t.Helper()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

func TestVerifyPinnedPeerCertificateRefusals(t *testing.T) {
	now := time.Now()
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	valid := certSpec{key: p256, dnsName: Authority, notBefore: now.Add(-time.Hour), notAfter: now.Add(time.Hour)}

	good := makeCert(t, valid)
	pin := spkiPin(t, good)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherSpec := valid
	otherSpec.key = other
	expiredSpec := valid
	expiredSpec.notAfter = now.Add(-time.Minute)
	futureSpec := valid
	futureSpec.notBefore = now.Add(time.Hour)
	futureSpec.notAfter = now.Add(2 * time.Hour)
	wrongName := valid
	wrongName.dnsName = "api.anthropic.com"
	curve := valid
	curve.key = p384
	ed := valid
	ed.key = edKey

	cases := []struct {
		name    string
		raw     [][]byte
		wantErr string
	}{
		{"pinned leaf", [][]byte{good}, ""},
		{"no certificate", nil, "single leaf"},
		{"chain of two", [][]byte{good, good}, "single leaf"},
		{"garbage", [][]byte{[]byte("nope")}, "parse gate TLS certificate"},
		{"ed25519 key", [][]byte{makeCert(t, ed)}, "ECDSA"},
		{"P-384 key", [][]byte{makeCert(t, curve)}, "P-256"},
		{"expired", [][]byte{makeCert(t, expiredSpec)}, "validity window"},
		{"not yet valid", [][]byte{makeCert(t, futureSpec)}, "validity window"},
		{"wrong authority", [][]byte{makeCert(t, wrongName)}, "authority mismatch"},
		{"other key", [][]byte{makeCert(t, otherSpec)}, "SPKI digest mismatch"},
	}
	verify := verifyPinnedPeerCertificate(pin)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verify(tc.raw, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verify = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("verify = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestGateLimitsDefaultsAndListenerCap(t *testing.T) {
	got := GateLimits{MaxConns: 2}.normalized()
	def := DefaultGateLimits()
	if got.MaxConns != 2 || got.ReadTimeout != def.ReadTimeout || got.WriteTimeout != def.WriteTimeout ||
		got.ReadHeaderTimeout != def.ReadHeaderTimeout || got.IdleTimeout != def.IdleTimeout ||
		got.MaxHeaderBytes != def.MaxHeaderBytes || got.MaxBodyBytes != def.MaxBodyBytes {
		t.Fatalf("normalized = %+v", got)
	}
	if neg := (GateLimits{MaxConns: -1, MaxBodyBytes: -5}).normalized(); neg != def {
		t.Fatalf("negative limits normalized to %+v, want defaults", neg)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	limited := LimitListener(ln, 0).(*limitListener)
	if cap(limited.sem) != MaxGateConns {
		t.Fatalf("default connection cap = %d, want %d", cap(limited.sem), MaxGateConns)
	}
	if err := limited.Close(); err != nil {
		t.Fatal(err)
	}
	if err := limited.Close(); err == nil {
		t.Fatal("second Close of the underlying listener succeeded")
	}
	if _, err := limited.Accept(); err == nil {
		t.Fatal("Accept after Close succeeded")
	}
}

type fakeConn struct {
	net.Conn
	closed bool
}

func (c *fakeConn) Close() error { c.closed = true; return nil }

func TestProviderConnectRefusals(t *testing.T) {
	hosts := providerHosts["claude"]
	// A global unicast address outside every special range; nothing is dialled for real.
	publicIP := netip.AddrFrom4([4]byte{1, 2, 3, 4})
	public := []netip.Addr{publicIP}
	okLookup := func(context.Context, string, string) ([]netip.Addr, error) { return public, nil }

	cases := []struct {
		name    string
		target  string
		body    string
		lookup  lookupIP
		dialErr bool
		fill    bool
		code    int
		msg     string
		dialed  bool
	}{
		{"unlisted host", "evil.example:443", "", okLookup, false, false, http.StatusForbidden, "destination refused", false},
		{"wrong port", "api.anthropic.com:80", "", okLookup, false, false, http.StatusForbidden, "destination refused", false},
		{"no port", "api.anthropic.com", "", okLookup, false, false, http.StatusForbidden, "destination refused", false},
		{"body on CONNECT", "api.anthropic.com:443", "x", okLookup, false, false, http.StatusForbidden, "destination refused", false},
		{"tunnels busy", "api.anthropic.com:443", "", okLookup, false, true, http.StatusServiceUnavailable, "tunnels busy", false},
		{"lookup fails", "api.anthropic.com:443", "", func(context.Context, string, string) ([]netip.Addr, error) { return nil, errors.New("nxdomain") }, false, false, http.StatusBadGateway, "provider unavailable", false},
		{"no addresses", "api.anthropic.com:443", "", func(context.Context, string, string) ([]netip.Addr, error) { return nil, nil }, false, false, http.StatusBadGateway, "provider unavailable", false},
		{"resolves to loopback", "API.anthropic.com:443", "", func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{publicIP, netip.MustParseAddr("127.0.0.1")}, nil
		}, false, false, http.StatusBadGateway, "provider unavailable", false},
		{"resolves to metadata address", "api.anthropic.com:443", "", func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}, false, false, http.StatusBadGateway, "provider unavailable", false},
		{"dial fails", "api.anthropic.com:443", "", okLookup, true, false, http.StatusBadGateway, "provider unavailable", true},
		{"recorder cannot hijack", "api.anthropic.com:443", "", okLookup, false, false, http.StatusBadGateway, "tunnel unavailable", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lookedUp string
			lookup := func(ctx context.Context, network, host string) ([]netip.Addr, error) {
				lookedUp = host
				return tc.lookup(ctx, network, host)
			}
			dialed := false
			upstream := &fakeConn{}
			dial := func(_ context.Context, _, addr string) (net.Conn, error) {
				dialed = true
				if addr != net.JoinHostPort(publicIP.String(), "443") {
					t.Errorf("dialled %q", addr)
				}
				if tc.dialErr {
					return nil, errors.New("refused")
				}
				return upstream, nil
			}
			tunnels := make(chan struct{}, 1)
			if tc.fill {
				tunnels <- struct{}{}
			}
			r := httptest.NewRequest(http.MethodConnect, tc.target, strings.NewReader(tc.body))
			r.RequestURI, r.Host, r.URL = tc.target, tc.target, &url.URL{Host: tc.target}
			if tc.body == "" {
				r.ContentLength = 0
			}
			rec := httptest.NewRecorder()
			providerConnect(rec, r, hosts, tunnels, lookup, dial)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.msg) {
				t.Fatalf("got %d %q, want %d %q", rec.Code, rec.Body.String(), tc.code, tc.msg)
			}
			if dialed != tc.dialed {
				t.Fatalf("dialed = %v, want %v", dialed, tc.dialed)
			}
			if tc.dialed && !tc.dialErr && !upstream.closed {
				t.Fatal("upstream connection left open")
			}
			if lookedUp != "" && !strings.HasSuffix(lookedUp, ".") {
				t.Fatalf("lookup of %q has no trailing dot", lookedUp)
			}
			if !tc.fill && len(tunnels) != 0 {
				t.Fatal("tunnel slot not released")
			}
		})
	}
}
