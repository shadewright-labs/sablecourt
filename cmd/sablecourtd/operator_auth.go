package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/integrations"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	operatorCookie          = "sablecourt_session"
	sessionTTL              = 30 * time.Minute
	loginBodyLimit          = 1024
	maxBearerSize           = 512
	loginMaxFailures        = 10
	loginFailureWindow      = time.Minute
	loginLimiterMaxClients  = 1024
	voiceTranscribeMaxBytes = 24 << 20
)

type loginSecretsKey struct{}

type operatorAuth struct {
	keyMu     sync.RWMutex
	hasToken  bool
	tokenHash [sha256.Size]byte
	signKey   [32]byte
	limiter   *loginLimiter
	// onRoute, when set, is told every pattern registered here (tests only).
	onRoute func(pattern string)
}

func newOperatorAuth(token string) *operatorAuth {
	suffix, prefixed := strings.CutPrefix(token, "op_")
	valid := prefixed && len(suffix) == 2*sha256.Size
	if valid {
		_, err := hex.DecodeString(suffix)
		valid = err == nil
	}
	auth := &operatorAuth{
		hasToken:  valid,
		tokenHash: sha256.Sum256([]byte(token)),
		limiter:   newLoginLimiter(loginMaxFailures, loginFailureWindow, loginLimiterMaxClients),
	}
	rand.Read(auth.signKey[:])
	redact.Register(token)
	redact.RegisterBytes(auth.signKey[:])
	return auth
}

func (a *operatorAuth) matches(token string) bool {
	if !a.hasToken || len(token) != len("op_")+2*sha256.Size || !strings.HasPrefix(token, "op_") {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return hmac.Equal(digest[:], a.tokenHash[:])
}

func tokenDigestEqual(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	gotDigest := sha256.Sum256([]byte(got))
	wantDigest := sha256.Sum256([]byte(want))
	return hmac.Equal(gotDigest[:], wantDigest[:])
}

func isCanonicalPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	clean := path.Clean(p)
	if p[len(p)-1] == '/' && clean != "/" {
		clean += "/"
	}
	return p == clean
}

func (a *operatorAuth) requireOperatorAuth(mux *http.ServeMux, next http.Handler, secrets secret.SecretStore) http.Handler {
	routes := http.NewServeMux()
	recordingMux{routes, a.onRoute}.HandleFunc("POST /logout", a.logout)
	routes.Handle("/", next)
	scoped := integrations.RejectShadewrightTokenOnOtherRoutes(routes, secrets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isCanonicalPath(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "canonical request path required"})
			return
		}
		_, pattern := mux.Handler(r)
		switch pattern {
		case "POST /login":
			if secrets != nil {
				r = r.WithContext(context.WithValue(r.Context(), loginSecretsKey{}, secrets))
			}
			scoped.ServeHTTP(w, r)
			return
		case "GET /login",
			"GET /healthz", "GET /.well-known/agent-card.json",
			"GET /favicon.ico", "GET /apple-touch-icon.png", "GET /assets/",
			"POST /api/v1/hooks/github", "POST /a2a":
			scoped.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(pattern, "/gate/") || strings.Contains(pattern, " /gate/") || integrations.IsShadewrightRequest(r) {
			scoped.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		auth := r.Header.Values("Authorization")
		if len(auth) == 1 {
			if token, ok := strings.CutPrefix(auth[0], "Bearer "); ok {
				if a.matches(token) {
					scoped.ServeHTTP(w, r.WithContext(store.WithLearningOperator(r.Context(), "operator")))
					return
				}
				if r.Method == http.MethodGet && secrets != nil && len(token) <= maxBearerSize &&
					!strings.HasPrefix(token, "op_") && !strings.HasPrefix(token, model.TokenPrefix) && isObserverRoute(pattern) {
					if want, err := secrets.Get(r.Context(), "observer_bearer_token"); err == nil {
						redact.Register(want)
						if tokenDigestEqual(token, strings.TrimSpace(want)) {
							scoped.ServeHTTP(w, r)
							return
						}
					}
				}
			}
		}
		if len(auth) == 0 {
			cookies := r.CookiesNamed(operatorCookie)
			if len(cookies) == 1 && a.validSession(r.Host, cookies[0].Value, time.Now()) {
				if isWrite(r.Method) && browserOriginScheme(r) == "" {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "same-origin request required"})
					return
				}
				scoped.ServeHTTP(w, r.WithContext(store.WithLearningOperator(r.Context(), "operator")))
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			!strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/ui/") &&
			strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `<!doctype html><title>Sablecourt</title><a href="/login">Sign in to Sablecourt</a>`)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator credentials required"})
	})
}

func isObserverRoute(pattern string) bool {
	switch pattern {
	case "GET /api/v1/observer/snapshot", "GET /api/v1/observer/stream", "GET /api/v1/tasks/{id}/scry":
		return true
	}
	return false
}

func operatorLoginPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to Sablecourt</title>
<h1>Sign in to Sablecourt</h1>
<form method="post" action="/login">
  <label for="token">Operator token</label>
  <input type="password" id="token" name="token" required autocomplete="off" spellcheck="false">
  <button type="submit">Sign in</button>
</form>
</html>
`)
}

func (a *operatorAuth) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	scheme := browserOriginScheme(r)
	if scheme == "" || (r.TLS == nil && strings.TrimSpace(r.Header.Get("Origin")) == "") {
		http.Error(w, "same-origin request required", http.StatusForbidden)
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		http.Error(w, "form data required", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "login form too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "invalid login form", http.StatusBadRequest)
		return
	}
	values, err := url.ParseQuery(string(body))
	if err != nil || len(values) != 1 || len(values["token"]) != 1 {
		http.Error(w, "provide one operator token", http.StatusBadRequest)
		return
	}
	client := loginClientKey(r.RemoteAddr)
	token := strings.TrimSpace(values.Get("token"))
	if !a.matches(token) {
		if retry := a.limiter.recordFailure(client, time.Now()); retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(max(int(retry.Round(time.Second)/time.Second), 1)))
			http.Error(w, "too many login attempts", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "invalid operator token", http.StatusUnauthorized)
		return
	}
	// A form credential must pass the same integration scope check as a bearer.
	if secrets, ok := r.Context().Value(loginSecretsKey{}).(secret.SecretStore); ok {
		authRequest := r.Clone(r.Context())
		authRequest.Header.Set("Authorization", "Bearer "+token)
		integrations.RejectShadewrightTokenOnOtherRoutes(http.HandlerFunc(a.issueSession), secrets).ServeHTTP(w, authRequest)
		return
	}
	a.issueSession(w, r)
}

func (a *operatorAuth) issueSession(w http.ResponseWriter, r *http.Request) {
	expires := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     operatorCookie,
		Value:    a.sessionValue(r.Host, expires.Unix()),
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL / time.Second),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *operatorAuth) logout(w http.ResponseWriter, r *http.Request) {
	if browserOriginScheme(r) == "" {
		http.Error(w, "same-origin request required", http.StatusForbidden)
		return
	}
	var key [32]byte
	rand.Read(key[:])
	redact.RegisterBytes(key[:])
	a.keyMu.Lock()
	a.signKey = key
	a.keyMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: operatorCookie, Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *operatorAuth) validSession(host, value string, now time.Time) bool {
	if !a.hasToken || strings.TrimSpace(host) == "" || len(value) > 20+1+sha256.Size*2 {
		return false
	}
	expiry, _, ok := strings.Cut(value, ".")
	expires, err := strconv.ParseInt(expiry, 10, 64)
	return ok && err == nil && expires > now.Unix() &&
		hmac.Equal([]byte(value), []byte(a.sessionValue(host, expires)))
}

func (a *operatorAuth) sessionValue(host string, expires int64) string {
	expiry := strconv.FormatInt(expires, 10)
	a.keyMu.RLock()
	key := a.signKey
	a.keyMu.RUnlock()
	mac := hmac.New(sha256.New, key[:])
	_, _ = io.WriteString(mac, expiry+"\n"+strings.ToLower(host))
	return expiry + "." + hex.EncodeToString(mac.Sum(nil))
}

type loginFailureEntry struct {
	failures int
	resetAt  time.Time
}

type loginLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	maxEntries int
	global     loginFailureEntry
	entries    map[string]loginFailureEntry
}

func newLoginLimiter(limit int, window time.Duration, maxEntries int) *loginLimiter {
	return &loginLimiter{
		limit:      limit,
		window:     window,
		maxEntries: maxEntries,
		entries:    make(map[string]loginFailureEntry),
	}
}

// recordFailure counts one invalid credential or returns the time until a retry.
func (l *loginLimiter) recordFailure(client string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.global.resetAt) {
		l.global = loginFailureEntry{resetAt: now.Add(l.window)}
	}
	entry := l.entries[client]
	if l.global.failures >= l.maxEntries {
		retry := l.global.resetAt.Sub(now)
		// A client blocked by its own window must wait for the later of the two deadlines.
		if now.Before(entry.resetAt) && entry.failures >= l.limit {
			retry = max(retry, entry.resetAt.Sub(now))
		}
		return retry
	}
	if !now.Before(entry.resetAt) {
		if len(l.entries) >= l.maxEntries {
			for key, e := range l.entries {
				if !now.Before(e.resetAt) {
					delete(l.entries, key)
				}
			}
			if len(l.entries) >= l.maxEntries {
				return l.window
			}
		}
		entry = loginFailureEntry{resetAt: now.Add(l.window)}
	}
	if entry.failures >= l.limit {
		return entry.resetAt.Sub(now)
	}
	entry.failures++
	l.entries[client] = entry
	l.global.failures++
	return 0
}

func loginClientKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	ip = ip.WithZone("").Unmap()
	if ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String()
}

func isWrite(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func browserOriginScheme(r *http.Request) string {
	if strings.TrimSpace(r.Host) == "" {
		return ""
	}
	fetch := r.Header.Values("Sec-Fetch-Site")
	if len(fetch) > 1 || (len(fetch) == 1 && fetch[0] != "" && fetch[0] != "same-origin") {
		return ""
	}
	sameOrigin := len(fetch) == 1 && fetch[0] == "same-origin"
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 || (len(origins) == 1 && origins[0] == "") {
		if sameOrigin {
			return scheme
		}
		return ""
	}
	if len(origins) != 1 {
		return ""
	}
	u, err := url.Parse(origins[0])
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(origins[0], "#") {
		return ""
	}
	// Fetch Metadata gives the browser's origin check through a TLS-terminating proxy.
	if u.Scheme == scheme || (sameOrigin && u.Scheme == "https") {
		return u.Scheme
	}
	return ""
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; media-src 'self' blob:; img-src 'self' data:; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// maxOperatorWriteBody caps the body of every write outside the Model Gate
// and the voice transcribe route.
const maxOperatorWriteBody = 2 << 20

func guardOperatorRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWrite(r.Method) {
			p := r.URL.Path
			scoped := strings.HasPrefix(p, "/gate/") || p == "/api/v1/hooks/github" || p == "/a2a"
			if !scoped && (r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "") && browserOriginScheme(r) == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked"})
				return
			}
			// The Model Gate and voice transcribe endpoint enforce their own larger limits.
			if !strings.HasPrefix(p, "/gate/model/") && p != "/api/v1/voice/transcribe" {
				r.Body = http.MaxBytesReader(w, r.Body, maxOperatorWriteBody)
			}
		}
		next.ServeHTTP(w, r)
	})
}
