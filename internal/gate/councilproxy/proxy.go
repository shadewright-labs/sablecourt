// Package councilproxy limits council containers to their workload gate and provider.
package councilproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	Authority         = "sablecourt-council-gate"
	DockerHostGateway = "host.docker.internal"
	Port              = "8888"
	dialTimeout       = 5 * time.Second
	tunnelLifetime    = 15 * time.Minute
	// MaxTunnels bounds concurrent CONNECT tunnels so they cannot occupy every
	// listener slot and starve gate calls.
	MaxTunnels = MaxGateConns / 2
)

// Config comes from the daemon at launch. An empty digest permits provider login only.
type Config struct {
	Harness        string
	DaemonListen   string
	LeaseSHA256    string
	GateSocket     string
	GateTLSPort    string
	GateSPKISHA256 string
	SocketRoot     string
	GateHandler    http.Handler
	// RecordResources persists cleanup identities before the first Docker call.
	RecordResources func(proxy, network string) error
}

func validPort(port string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == port
}

// GateAuthority identifies the workload-only virtual host on the loopback daemon.
func GateAuthority(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", errors.New("council gate requires a loopback listen address and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() || !validPort(port) {
		return "", errors.New("council gate requires a loopback listen address and port")
	}
	return net.JoinHostPort(Authority, port), nil
}

// AllowsGateRequest is shared by the proxy, per-launch Unix socket, and daemon council virtual host.
func AllowsGateRequest(r *http.Request) bool {
	u := r.URL
	if u == nil || u.IsAbs() || u.Host != "" || u.Opaque != "" || u.User != nil ||
		u.RawPath != "" || path.Clean(u.Path) != u.Path || strings.ContainsAny(u.Path, "\\%") {
		return false
	}
	switch u.Path {
	case "/gate/mcp":
		return r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodDelete
	case "/gate/task/messages":
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	}

	if action, ok := strings.CutPrefix(u.Path, "/gate/actions/"); ok {
		return action != "" && !strings.Contains(action, "/") && r.Method == http.MethodPost
	}
	room, ok := strings.CutPrefix(u.Path, "/gate/conclave/")
	if !ok {
		return false
	}
	id, resource, ok := strings.Cut(room, "/")
	if !ok || id == "" {
		return false
	}
	switch resource {
	case "messages":
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case "relics":
		return r.Method == http.MethodPost
	}
	name, ok := strings.CutPrefix(resource, "relics/")
	return ok && name != "" && !strings.Contains(name, "/") && r.Method == http.MethodGet
}

var providerHosts = map[string][]string{
	"claude": {"api.anthropic.com", "claude.ai", "console.anthropic.com", "platform.claude.com"},
	"codex":  {"api.openai.com", "auth.openai.com", "chatgpt.com"},
	"agy":    {"accounts.google.com", "antigravity.google", "oauth2.googleapis.com", "play.googleapis.com", "cloudcode-pa.googleapis.com", "daily-cloudcode-pa.googleapis.com"},
}

type lookupIP func(context.Context, string, string) ([]netip.Addr, error)
type dialContext func(context.Context, string, string) (net.Conn, error)

func verifyPinnedPeerCertificate(pinnedSPKI [sha256.Size]byte) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) != 1 {
			return errors.New("gate TLS peer must present a single leaf certificate")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parse gate TLS certificate: %w", err)
		}
		if cert.PublicKeyAlgorithm != x509.ECDSA {
			return errors.New("gate TLS certificate must use an ECDSA key")
		}
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			return errors.New("gate TLS certificate must use P-256")
		}
		now := time.Now()
		if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			return errors.New("gate TLS certificate is outside its validity window")
		}
		if err := cert.VerifyHostname(Authority); err != nil {
			return fmt.Errorf("gate TLS certificate authority mismatch: %w", err)
		}
		got := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(got[:], pinnedSPKI[:]) != 1 {
			return errors.New("gate TLS certificate SPKI digest mismatch")
		}
		return nil
	}
}

func newTLSGateTransport(pinnedSPKI [sha256.Size]byte, dial dialContext) *http.Transport {
	if dial == nil {
		dialer := &net.Dialer{Timeout: dialTimeout}
		dial = dialer.DialContext
	}
	verifyPeer := verifyPinnedPeerCertificate(pinnedSPKI)
	tlsCfg := &tls.Config{
		MinVersion:            tls.VersionTLS13,
		MaxVersion:            tls.VersionTLS13,
		ServerName:            Authority,
		InsecureSkipVerify:    true, // #nosec G402 -- VerifyPeerCertificate enforces P-256, validity window, and constant-time SPKI SHA-256 pinning on every TLS 1.3 handshake.
		ClientSessionCache:    nil,
		VerifyPeerCertificate: verifyPeer,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.Version != tls.VersionTLS13 || cs.DidResume || len(cs.PeerCertificates) != 1 {
				return errors.New("gate TLS connection must complete a fresh TLS 1.3 certificate handshake")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(got[:], pinnedSPKI[:]) != 1 {
				return errors.New("gate TLS connection SPKI digest mismatch")
			}
			return nil
		},
	}
	return &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("plain TCP council gate connection refused")
		},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			rawConn, err := dial(dialCtx, network, addr)
			if err != nil {
				return nil, err
			}
			if deadline, ok := dialCtx.Deadline(); ok {
				_ = rawConn.SetDeadline(deadline)
			}
			tlsConn := tls.Client(rawConn, tlsCfg.Clone())
			if err := tlsConn.HandshakeContext(dialCtx); err != nil {
				_ = rawConn.Close()
				return nil, err
			}
			_ = rawConn.SetDeadline(time.Time{})
			return tlsConn, nil
		},
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          8,
	}
}

func newUnixGateTransport(socketPath string) *http.Transport {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          8,
	}
}

func newGateReverseProxy(upstream *url.URL, gate string, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(upstream)
			p.Out.Host = gate
			p.Out.Header.Del("Cookie")
		},
		ModifyResponse: func(r *http.Response) error {
			if r.StatusCode >= http.StatusMultipleChoices && r.StatusCode < http.StatusBadRequest {
				return errors.New("gate redirect refused")
			}
			r.Header.Del("Location")
			r.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "council gate unavailable", http.StatusBadGateway)
		},
	}
}

func validLeaseHeader(r *http.Request, digest []byte) bool {
	if len(digest) != sha256.Size {
		return false
	}
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || len(auth[0]) > 512 {
		return false
	}
	token, ok := strings.CutPrefix(auth[0], "Bearer ")
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], digest) == 1
}

// gateRouteHandler enforces the shared route and bearer boundary on both the proxy and per-launch Unix socket.
func gateRouteHandler(expectedHost string, digest []byte, forward http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (expectedHost != "" && r.Host != expectedHost) || !AllowsGateRequest(r) || r.Header.Get("Upgrade") != "" {
			http.Error(w, "council route refused", http.StatusForbidden)
			return
		}
		if validLeaseHeader(r, digest) {
			forward.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "task lease required", http.StatusUnauthorized)
	})
}

// New constructs the gate and HTTPS CONNECT proxy without consulting environment proxy settings.
func New(cfg Config) (http.Handler, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return newProxy(cfg, nil, net.DefaultResolver.LookupNetIP, dialer.DialContext)
}

func newProxy(cfg Config, transport http.RoundTripper, lookup lookupIP, dial dialContext) (http.Handler, error) {
	if err := cfg.validateProxy(); err != nil {
		return nil, err
	}
	gate, _ := GateAuthority(cfg.DaemonListen)
	_, port, _ := net.SplitHostPort(gate)
	digest, _ := hex.DecodeString(cfg.LeaseSHA256)

	var forward http.Handler = http.NotFoundHandler()
	switch {
	case cfg.GateSocket != "":
		if transport == nil {
			transport = newUnixGateTransport(cfg.GateSocket)
		}
		forward = newGateReverseProxy(&url.URL{Scheme: "http", Host: "unix"}, gate, transport)
	case cfg.GateSPKISHA256 != "":
		targetPort := port
		if cfg.GateTLSPort != "" {
			targetPort = cfg.GateTLSPort
		}
		if transport == nil {
			rawPin, _ := hex.DecodeString(cfg.GateSPKISHA256)
			var pin [sha256.Size]byte
			copy(pin[:], rawPin)
			transport = newTLSGateTransport(pin, nil)
		}
		upstream := &url.URL{Scheme: "https", Host: net.JoinHostPort(DockerHostGateway, targetPort)}
		forward = newGateReverseProxy(upstream, gate, transport)
	}

	gateHandler := gateRouteHandler("", digest, forward)
	hosts := providerHosts[cfg.Harness]
	tunnels := make(chan struct{}, MaxTunnels)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			providerConnect(w, r, hosts, tunnels, lookup, dial)
			return
		}
		gateHandler.ServeHTTP(w, r)
	}), nil
}

func (cfg Config) validateCommon() error {
	if _, ok := providerHosts[cfg.Harness]; !ok {
		return errors.New("unknown council harness")
	}
	if _, err := GateAuthority(cfg.DaemonListen); err != nil {
		return err
	}
	if digest, err := hex.DecodeString(cfg.LeaseSHA256); err != nil || (cfg.LeaseSHA256 != "" && len(digest) != sha256.Size) {
		return errors.New("invalid council task lease digest")
	}
	return nil
}

func (cfg Config) validateProvision() error {
	if err := cfg.validateCommon(); err != nil {
		return err
	}
	if cfg.GateSocket != "" || cfg.GateTLSPort != "" || cfg.GateSPKISHA256 != "" {
		return errors.New("provision selects gate transport parameters automatically")
	}
	if cfg.LeaseSHA256 != "" && cfg.GateHandler == nil {
		return errors.New("council gate provision requires an in-process gate handler")
	}
	return nil
}

func (cfg Config) validateProxy() error {
	if err := cfg.validateCommon(); err != nil {
		return err
	}
	if cfg.LeaseSHA256 == "" {
		if cfg.GateSocket != "" || cfg.GateTLSPort != "" || cfg.GateSPKISHA256 != "" {
			return errors.New("login proxy must not carry gate upstream parameters")
		}
		return nil
	}
	hasSocket := cfg.GateSocket != ""
	hasTLS := cfg.GateSPKISHA256 != "" || cfg.GateTLSPort != ""
	if hasSocket == hasTLS {
		return errors.New("council gate proxy requires either a unix socket or a pinned TLS SPKI digest")
	}
	if hasSocket {
		if !strings.HasPrefix(cfg.GateSocket, "/") || path.Clean(cfg.GateSocket) != cfg.GateSocket || strings.ContainsRune(cfg.GateSocket, 0) {
			return errors.New("council gate socket must be a clean absolute path")
		}
		return nil
	}
	if !validHexBytes(cfg.GateSPKISHA256, sha256.Size, sha256.Size) {
		return errors.New("invalid council gate TLS SPKI SHA-256 digest")
	}
	if cfg.GateTLSPort != "" && !validPort(cfg.GateTLSPort) {
		return errors.New("invalid council gate TLS port")
	}
	return nil
}

func providerConnect(w http.ResponseWriter, r *http.Request, hosts []string, tunnels chan struct{}, lookup lookupIP, dial dialContext) {
	host, port, err := net.SplitHostPort(r.URL.Host)
	if err != nil || r.RequestURI != r.URL.Host || r.Host != r.URL.Host ||
		port != "443" || !listedHost(host, hosts) ||
		r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "provider destination refused", http.StatusForbidden)
		return
	}
	select {
	case tunnels <- struct{}{}:
		defer func() { <-tunnels }()
	default:
		w.Header().Set("Connection", "close")
		http.Error(w, "provider tunnels busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	// The trailing dot prevents resolver search domains from changing the target.
	ips, err := lookup(ctx, "ip", strings.ToLower(host)+".")
	if err != nil || len(ips) == 0 {
		http.Error(w, "provider unavailable", http.StatusBadGateway)
		return
	}
	for _, ip := range ips {
		if !publicAddress(ip) {
			http.Error(w, "provider unavailable", http.StatusBadGateway)
			return
		}
	}
	var upstream net.Conn
	for _, ip := range ips {
		upstream, err = dial(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			break
		}
	}
	if upstream == nil {
		http.Error(w, "provider unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close() //nolint:errcheck // acknowledged dropped error: releases the upstream connection on every return; the handler has no caller to report a close error to, and both io.Copy errors below are dropped too
	client, stream, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "provider tunnel unavailable", http.StatusBadGateway)
		return
	}
	defer client.Close() //nolint:errcheck // acknowledged dropped error: releases the hijacked client connection on every return, including those before any copy; nothing reports it
	deadline := time.Now().Add(tunnelLifetime)
	if client.SetDeadline(deadline) != nil || upstream.SetDeadline(deadline) != nil {
		return
	}
	if _, err := stream.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || stream.Flush() != nil {
		return
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = io.Copy(upstream, stream)
		closeWrite(upstream)
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	_ = upstream.Close()
	<-finished
}

func listedHost(host string, hosts []string) bool {
	for _, allowed := range hosts {
		if strings.EqualFold(allowed, host) {
			return true
		}
	}
	return false
}

func closeWrite(conn net.Conn) {
	if halfCloser, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
	} else {
		_ = conn.Close()
	}
}

var specialAddresses = []netip.Prefix{
	netip.PrefixFrom(netip.AddrFrom4([4]byte{0}), 8),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64}), 10),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 0}), 24),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2}), 24),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 18}), 15),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 51, 100}), 24),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{203, 0, 113}), 24),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{240}), 4),
	netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x01}), 23),
	netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8}), 32),
	netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x02}), 16),
}

var globalIPv6 = netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20}), 3)

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !globalIPv6.Contains(ip)) {
		return false
	}
	for _, prefix := range specialAddresses {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
