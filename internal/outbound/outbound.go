// Package outbound decides whether the daemon may send a request to a
// configured URL. Every URL the daemon calls out to (the model gateway,
// provider base URLs, upstream MCP servers, the GitHub API, the chat webhook,
// the voice backends) passes through Check when it is loaded and again before
// each request, so one rule covers every entry point.
//
// The rule: https is accepted; http is accepted only when the host is a
// loopback address (127.0.0.0/8, ::1) or the name localhost, because a request
// that never leaves the machine cannot be read or changed on the network. Any
// other scheme, a missing host, or a URL that does not parse is refused.
package outbound

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Check returns nil when raw is a URL the daemon may call, and otherwise an
// error that names field. The error never contains the URL, so user info,
// a token in the path (as in chat webhooks) or a query key cannot reach a log.
func Check(field, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%s: not a valid URL", field)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !Loopback(u.Hostname()) {
			return fmt.Errorf("%s: plain http is refused for a host that is not loopback; use https, or http on 127.0.0.1, [::1] or localhost", field)
		}
	case "":
		return fmt.Errorf("%s: the URL has no scheme; use https, or http on 127.0.0.1, [::1] or localhost", field)
	default:
		return fmt.Errorf("%s: only https, or http on 127.0.0.1, [::1] or localhost, is accepted", field)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%s: the URL has no host", field)
	}
	return nil
}

// Loopback reports whether host (without a port or brackets) names this
// machine: a loopback IP literal or the name localhost.
func Loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Display returns raw without user info, query or fragment, for a log line
// or an error that has to say where a request went. A value that does not
// parse is replaced by a placeholder, never echoed.
func Display(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "(unparsable URL)"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// CheckGitRemote applies the same rule to a Git remote. The forms Git sends
// over an authenticated or local channel pass: https, ssh, file, an scp-like
// user@host:path and a local path, plus http on a loopback host. Plain http
// to another host and git:// (unauthenticated, unencrypted) are refused, as
// is Git's transport::address syntax (ext::, fd:: and the like), which runs a
// helper instead of fetching.
func CheckGitRemote(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%s: the remote is empty", field)
	}
	scheme, _, hasScheme := strings.Cut(raw, "://")
	if !hasScheme {
		if transport, _, ok := strings.Cut(raw, "::"); ok && !strings.ContainsAny(transport, "/@") {
			return fmt.Errorf("%s: a Git remote transport helper is refused", field)
		}
		return nil
	}
	switch strings.ToLower(scheme) {
	case "https", "http":
		return Check(field, raw)
	case "ssh", "file":
		return nil
	default:
		return fmt.Errorf("%s: only https, ssh, file, an scp-like address, a local path, or http on 127.0.0.1, [::1] or localhost, is accepted", field)
	}
}

// DisplayGitRemote is Display for a Git remote: an scp-like address or a
// local path carries no password and is returned as is.
func DisplayGitRemote(raw string) string {
	if !strings.Contains(raw, "://") {
		return strings.TrimSpace(raw)
	}
	return Display(raw)
}

// Client returns a copy of base (a default client when nil) that also applies
// Check to every redirect target, so an https endpoint cannot move a request,
// and the credential headers Go keeps for the same host, to plain http. The
// redirect policy of base still applies after that check; without one, Go's
// default limit of ten redirects holds.
func Client(base *http.Client, field string) *http.Client {
	var c http.Client
	if base != nil {
		c = *base
	}
	next := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := Check(field+" redirect", req.URL.String()); err != nil {
			return err
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}
