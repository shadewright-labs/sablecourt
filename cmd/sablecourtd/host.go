package main

import (
	"fmt"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
)

// hostAllowlistMiddleware checks the HTTP authority before auth, routing, or redirects.
// Extra entries are exact authorities; a bare name uses the listen port.
func hostAllowlistMiddleware(next http.Handler, listenAddr string, extraHosts []string, unixSocket ...bool) (http.Handler, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || !validHTTPPort(port) {
		return nil, fmt.Errorf("listen address must include a numeric port from 1 to 65535")
	}
	allowed := make(map[string]bool)
	for _, host := range []string{"localhost", net.IPv4(127, 0, 0, 1).String(), net.IPv6loopback.String()} {
		allowed[net.JoinHostPort(host, port)] = true
	}
	for _, entry := range extraHosts {
		authority := entry
		if net.ParseIP(entry) != nil || !strings.Contains(entry, ":") {
			authority = net.JoinHostPort(entry, port)
		}
		key, ok := hostAuthority(authority)
		if !ok {
			return nil, fmt.Errorf("allow-host must be an exact host or host:port")
		}
		allowed[key] = true
	}
	return securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The local CLI uses a synthetic authority over the private Unix socket.
		if len(unixSocket) > 0 && unixSocket[0] && r.Host == "unix" {
			next.ServeHTTP(w, r)
			return
		}
		key, ok := hostAuthority(r.Host)
		if !ok || !allowed[key] {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "request host is not allowed"})
			return
		}
		next.ServeHTTP(w, r)
	})), nil
}

func validHTTPPort(port string) bool {
	if port == "" {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

func hostAuthority(authority string) (string, bool) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || !validHTTPPort(port) || host == "" {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		if len(host) > 253 {
			return "", false
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", false
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					return "", false
				}
			}
		}
	}
	return net.JoinHostPort(strings.ToLower(host), port), true
}

func (cfg daemonConfig) containerGateHost() string {
	if cfg.gateHost != "" {
		return cfg.gateHost
	}
	if host, port, err := net.SplitHostPort(cfg.listenAddr); err == nil && (host == "" || host == net.IPv4(127, 0, 0, 1).String()) {
		return "host.docker.internal:" + port
	}
	return cfg.listenAddr
}

// hostGateURL is the data gate address for coding agents that run on the host, not in a
// container. It never depends on a Docker host alias, so it works without Docker Desktop.
func (cfg daemonConfig) hostGateURL() string {
	host, port, err := net.SplitHostPort(cfg.listenAddr)
	if err != nil {
		return ""
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/gate/mcp"
}

func (cfg daemonConfig) hostHandler(next http.Handler, unixSocket bool) (http.Handler, error) {
	host, _, err := net.SplitHostPort(cfg.listenAddr)
	if cfg.gateHost == "" && err == nil && (host == "" || net.ParseIP(host).IsUnspecified()) {
		return nil, fmt.Errorf("--gate-host is required for a wildcard listener")
	}
	gate, ok := hostAuthority(cfg.containerGateHost())
	if !ok {
		return nil, fmt.Errorf("gate-host requires an exact host and port")
	}
	regular, err := hostAllowlistMiddleware(next, cfg.listenAddr, cfg.allowedHosts, unixSocket)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, valid := hostAuthority(r.Host)
		// The council virtual host is never served on a listener. The per-launch
		// gate dispatches to councilGateHandler in process, behind the lease check.
		if valid && strings.HasPrefix(host, councilproxy.Authority+":") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "council gate route is not allowed"})
			return
		}
		if valid && host == gate && strings.HasPrefix(path.Clean(r.URL.Path), "/gate/") && strings.HasPrefix(r.URL.EscapedPath(), "/gate/") {
			next.ServeHTTP(w, r)
			return
		}
		regular.ServeHTTP(w, r)
	}), nil
}

// councilGateHandler is the in-process target of a per-launch council gate. It is
// never mounted on a network listener: the gate has already checked the task lease
// and bounded the request, and this handler admits only the council virtual host
// and the council route allow-list before the daemon's gate routes.
func (cfg daemonConfig) councilGateHandler(next http.Handler) (http.Handler, error) {
	councilGate, err := councilproxy.GateAuthority(cfg.listenAddr)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, valid := hostAuthority(r.Host)
		if !valid || host != councilGate || !councilproxy.AllowsGateRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "council gate route is not allowed"})
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}
