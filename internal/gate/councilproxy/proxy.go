// Package councilproxy limits council containers to their workload gate and provider.
package councilproxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
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
)

// Config comes from the daemon at launch. An empty digest permits provider login only.
type Config struct {
	Harness      string
	DaemonListen string
	LeaseSHA256  string
}

// GateAuthority identifies the workload-only virtual host on the loopback daemon.
func GateAuthority(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", errors.New("council gate requires a loopback listen address and port")
	}
	ip, err := netip.ParseAddr(host)
	n, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || !ip.IsLoopback() || portErr != nil || n == 0 || strconv.FormatUint(n, 10) != port {
		return "", errors.New("council gate requires a loopback listen address and port")
	}
	return net.JoinHostPort(Authority, port), nil
}

// AllowsGateRequest is shared by the proxy and the daemon's council virtual host.
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

// New constructs the gate and HTTPS CONNECT proxy without consulting environment proxy settings.
func New(cfg Config) (http.Handler, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return newProxy(cfg, &http.Transport{
		DialContext:           dialer.DialContext,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          8,
	}, net.DefaultResolver.LookupNetIP, dialer.DialContext)
}

func newProxy(cfg Config, transport http.RoundTripper, lookup lookupIP, dial dialContext) (http.Handler, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	gate, _ := GateAuthority(cfg.DaemonListen)
	_, port, _ := net.SplitHostPort(gate)
	digest, _ := hex.DecodeString(cfg.LeaseSHA256)
	upstream := &url.URL{Scheme: "http", Host: net.JoinHostPort(DockerHostGateway, port)}
	reverse := &httputil.ReverseProxy{
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
	hosts := providerHosts[cfg.Harness]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			providerConnect(w, r, hosts, lookup, dial)
			return
		}
		if !AllowsGateRequest(r) || r.Header.Get("Upgrade") != "" {
			http.Error(w, "council route refused", http.StatusForbidden)
			return
		}
		auth := r.Header.Values("Authorization")
		if len(auth) == 1 && len(auth[0]) <= 512 {
			token, ok := strings.CutPrefix(auth[0], "Bearer ")
			got := sha256.Sum256([]byte(token))
			if ok && subtle.ConstantTimeCompare(got[:], digest) == 1 {
				reverse.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "task lease required", http.StatusUnauthorized)
	}), nil
}

func (cfg Config) validate() error {
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

func providerConnect(w http.ResponseWriter, r *http.Request, hosts []string, lookup lookupIP, dial dialContext) {
	host, port, err := net.SplitHostPort(r.URL.Host)
	if err != nil || r.RequestURI != r.URL.Host || r.Host != r.URL.Host ||
		port != "443" || !listedHost(host, hosts) ||
		r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "provider destination refused", http.StatusForbidden)
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
	defer upstream.Close()
	client, stream, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "provider tunnel unavailable", http.StatusBadGateway)
		return
	}
	defer client.Close()
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
