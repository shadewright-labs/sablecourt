package main

import (
	"fmt"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}), nil
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

func (cfg daemonConfig) hostHandler(next http.Handler, unixSocket bool) (http.Handler, error) {
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
		if valid && host == gate && strings.HasPrefix(path.Clean(r.URL.Path), "/gate/") && strings.HasPrefix(r.URL.EscapedPath(), "/gate/") {
			next.ServeHTTP(w, r)
			return
		}
		regular.ServeHTTP(w, r)
	}), nil
}
