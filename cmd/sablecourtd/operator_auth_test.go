package main

import (
	"bytes"
	"context"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

func TestOperatorRoutesRequireOperatorCredentials(t *testing.T) {
	st, _, _, sanctum, citadel := newIntegrationTestEnv(t, domain.ConclaveWards{}, defaultIntegrationSecrets())
	addMessageTestLease(t, st)
	taskToken, err := model.MintTaskToken(t.Context(), st, "task", "worker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	routes := []string{
		"GET /", "GET /tasks/item", "GET /providers", "GET /studio", "GET /conclaves",
		"GET /conclaves/item", "GET /schedules", "GET /audit", "GET /memory", "GET /metrics",
		"GET /api/v1/providers", "GET /api/v1/providers/tiers", "POST /api/v1/providers/tiers",
		"POST /api/v1/providers/item/sync", "GET /api/v1/learnings", "POST /api/v1/learnings",
		"POST /api/v1/reload", "GET /api/v1/budget", "GET /api/v1/tasks/item",
		"POST /api/v1/tasks/item/cancel", "POST /api/v1/tasks/item/retry",
		"POST /api/v1/tasks/item/suspend", "POST /api/v1/tasks/item/resume",
		"GET /api/v1/tasks/item/scry", "POST /api/v1/tasks/item/takeover",
		"POST /api/v1/tasks/item/stdin", "POST /api/v1/tasks/item/detach",
		"GET /api/v1/stream", "GET /api/v1/voice/status", "POST /api/v1/voice/transcribe",
		"POST /api/v1/voice/speak", "POST /api/v1/conclaves/item/voice-decree",
		"POST /api/v1/notify/test", "GET /api/v1/observer/snapshot", "GET /api/v1/observer/stream",
		"GET /api/v1/memory", "POST /api/v1/memory", "DELETE /api/v1/memory/item/key",
		"GET /api/v1/workloads", "GET /api/v1/tasks", "POST /api/v1/tasks",
		"GET /api/v1/audit", "GET /api/v1/audit/verify", "GET /api/v1/audit/export",
		"GET /api/v1/schedules", "POST /api/v1/schedules/item/run",
		"GET /api/v1/approvals", "POST /api/v1/approvals/item/approve",
		"POST /api/v1/approvals/item/deny", "POST /api/v1/devices/pair/start",
		"POST /api/v1/devices/pair/complete", "GET /api/v1/devices", "POST /api/v1/devices/item/revoke",
		"POST /api/v1/promote/item", "GET /api/v1/conclaves", "POST /api/v1/conclaves",
		"GET /api/v1/conclaves/item", "GET /api/v1/conclaves/item/stream",
		"POST /api/v1/conclaves/item/decree", "POST /api/v1/conclaves/item/close",
		"POST /api/v1/conclaves/item/relics", "GET /api/v1/conclaves/item/relics/name",
		"POST /api/v1/hooks/github", "POST /api/v1/integrations/shadewright/incidents",
		"POST /api/v1/integrations/shadewright/verifications",
		"POST /ui/memory/set", "POST /ui/memory/learning", "POST /ui/memory/delete",
		"POST /ui/tasks/item/retry", "POST /ui/conclaves/item/relics", "POST /ui/conclaves/item/learning",
		"POST /ui/providers/tiers", "POST /ui/schedules/item/run", "POST /ui/conclaves",
		"POST /ui/conclaves/item/decree", "POST /ui/conclaves/item/close", "POST /ui/studio/conclave",
		"POST /ui/tasks", "POST /ui/tasks/item/cancel", "POST /ui/tasks/item/suspend",
		"POST /ui/tasks/item/resume", "POST /ui/tasks/item/messages", "POST /ui/approvals/item/approve",
		"POST /ui/approvals/item/deny", "POST /ui/providers/item/connect", "POST /ui/providers/item/disconnect",
		"POST /ui/providers/item/sync", "POST /ui/providers/item/models", "POST /ui/studio/agent",
		"POST /ui/studio/workflow", "POST /ui/workflows/item/run",
		"POST /gate/../api/v1/tasks", "POST /api/v1/%74asks", "POST /ui//tasks",
	}
	for _, mode := range []struct {
		name string
		h    http.Handler
	}{{"sanctum", sanctum}, {"citadel", citadel}} {
		for _, credential := range []struct {
			name, token string
		}{{"missing", ""}, {"active task lease", taskToken}} {
			for _, route := range routes {
				t.Run(mode.name+"/"+credential.name+"/"+route, func(t *testing.T) {
					defer func() {
						if recover() != nil {
							t.Error("request reached the handler without operator credentials")
						}
					}()
					method, path, _ := strings.Cut(route, " ")
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader("{}"))
					if credential.token != "" {
						req.Header.Set("Authorization", "Bearer "+credential.token)
					}
					w := httptest.NewRecorder()
					mode.h.ServeHTTP(w, req)
					if w.Code != http.StatusUnauthorized {
						t.Errorf("status = %d, want 401", w.Code)
					}
				})
			}
		}
	}
}

func TestOperatorLogin(t *testing.T) {
	_, _, _, sanctum, citadel := newIntegrationTestEnv(t, domain.ConclaveWards{}, defaultIntegrationSecrets())
	for _, mode := range []struct {
		name string
		h    http.Handler
	}{{"sanctum", sanctum}, {"citadel", citadel}} {
		t.Run(mode.name, func(t *testing.T) {
			page := httptest.NewRecorder()
			mode.h.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", nil))
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `type="password"`) {
				t.Errorf("login page status = %d or password field missing", page.Code)
			}
			authority := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")
			origin := "http://" + authority
			otherOrigin := "http://" + net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7433")
			validBody := url.Values{"token": {testOperatorToken}}.Encode()
			for _, tc := range []struct {
				name, body, contentType, origin, fetchSite string
				want                                       int
			}{
				{"valid", validBody, "application/x-www-form-urlencoded", origin, "same-origin", 303},
				{"wrong", "token=wrong", "application/x-www-form-urlencoded", origin, "", 401},
				{"empty", "token=", "application/x-www-form-urlencoded", origin, "", 401},
				{"null", "token=null", "application/x-www-form-urlencoded", origin, "", 401},
				{"task token", "token=" + model.TokenPrefix + "test", "application/x-www-form-urlencoded", origin, "", 401},
				{"duplicate", validBody + "&token=other", "application/x-www-form-urlencoded", origin, "", 400},
				{"wrong type", `{"token":123}`, "application/json", origin, "", 415},
				{"oversized", validBody + "&extra=" + strings.Repeat("x", 8192), "application/x-www-form-urlencoded", origin, "", 413},
				{"null origin", validBody, "application/x-www-form-urlencoded", "null", "", 403},
				{"missing origin", validBody, "application/x-www-form-urlencoded", "", "", 403},
				{"other origin", validBody, "application/x-www-form-urlencoded", otherOrigin, "", 403},
				{"same site different origin", validBody, "application/x-www-form-urlencoded", origin, "same-site", 403},
				{"scheme mismatch", validBody, "application/x-www-form-urlencoded", "https://" + authority, "", 403},
			} {
				t.Run(tc.name, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodPost, origin+"/login", strings.NewReader(tc.body))
					req.Header.Set("Content-Type", tc.contentType)
					req.Header.Set("Origin", tc.origin)
					req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
					w := httptest.NewRecorder()
					mode.h.ServeHTTP(w, req)
					if w.Code != tc.want {
						t.Fatalf("login status = %d, want %d", w.Code, tc.want)
					}
					cookies := w.Result().Cookies()
					if tc.want != http.StatusSeeOther {
						if len(cookies) != 0 {
							t.Error("refused login set a cookie")
						}
						return
					}
					if len(cookies) != 1 || w.Header().Get("Location") != "/" {
						t.Fatal("login must set one cookie and redirect to the dashboard")
					}
					cookie := cookies[0]
					if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" ||
						cookie.Domain != "" || cookie.MaxAge <= 0 || strings.Contains(cookie.Value, testOperatorToken) {
						t.Fatal("invalid session cookie attributes")
					}
					for _, follow := range []struct {
						name, method, path, origin, bearer string
						tamper                             bool
						want                               int
					}{
						{"dashboard", "GET", "/", "", "", false, 200},
						{"api read", "GET", "/api/v1/tasks", "", "", false, 200},
						{"ui write", "POST", "/ui/memory/set", origin, "", false, 303},
						{"api write", "POST", "/api/v1/tasks", origin, "", false, 400},
						{"cross-origin write", "POST", "/api/v1/tasks", otherOrigin, "", false, 403},
						{"null origin write", "POST", "/api/v1/tasks", "null", "", false, 403},
						{"missing origin write", "POST", "/api/v1/tasks", "", "", false, 403},
						{"tampered cookie", "GET", "/api/v1/tasks", "", "", true, 401},
						{"task bearer and cookie", "GET", "/api/v1/tasks", "", model.TokenPrefix + "test", false, 401},
						{"gate with cookie", "GET", "/gate/task/messages", "", "", false, 401},
					} {
						t.Run(follow.name, func(t *testing.T) {
							body := "{}"
							if follow.path == "/ui/memory/set" {
								body = "key=login-test&value=from-session"
							}
							r := httptest.NewRequest(follow.method, origin+follow.path, strings.NewReader(body))
							if follow.path == "/ui/memory/set" {
								r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
							}
							r.Header.Set("Origin", follow.origin)
							if follow.bearer != "" {
								r.Header.Set("Authorization", "Bearer "+follow.bearer)
							}
							c := *cookie
							if follow.tamper {
								c.Value += "a"
							}
							r.AddCookie(&c)
							out := httptest.NewRecorder()
							mode.h.ServeHTTP(out, r)
							if out.Code != follow.want {
								t.Errorf("status = %d, want %d", out.Code, follow.want)
							}
						})
					}
				})
			}
		})
	}
}

func TestApprovingDeviceEnrollmentRequiresIndependentProof(t *testing.T) {
	_, _, _, sanctum, citadel := newIntegrationTestEnv(t, domain.ConclaveWards{}, defaultIntegrationSecrets())
	for _, mode := range []struct {
		name string
		h    http.Handler
	}{{"sanctum", sanctum}, {"citadel", citadel}} {
		for _, step := range []string{"start", "complete"} {
			t.Run(mode.name+"/"+step, func(t *testing.T) {
				w := doBearerPOST(mode.h, "/api/v1/devices/pair/"+step, testOperatorToken, map[string]string{})
				if w.Code != http.StatusServiceUnavailable {
					t.Errorf("status = %d, want 503", w.Code)
				}
			})
		}
	}
}

func TestOperatorSessionLifetime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
		after   time.Duration
		want    int
	}{
		{"original handler", false, 0, http.StatusOK},
		{"after restart", true, 0, http.StatusUnauthorized},
		{"after expiry", false, 30 * time.Minute, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				newTestHandler := func() http.Handler {
					return newHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
						handlerConfig{operatorToken: testOperatorToken})
				}
				h := newTestHandler()
				host := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")
				origin := "https://" + host
				body := url.Values{"token": {testOperatorToken}}.Encode()
				r := httptest.NewRequest(http.MethodPost, origin+"/login", strings.NewReader(body))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Origin", origin)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				cookies := w.Result().Cookies()
				if w.Code != http.StatusSeeOther || len(cookies) != 1 || !cookies[0].Secure {
					t.Fatalf("HTTPS login failed to set a secure cookie, status %d", w.Code)
				}
				if tc.restart {
					h = newTestHandler()
				}
				time.Sleep(tc.after)
				r = httptest.NewRequest(http.MethodGet, origin+"/api/v1/providers", nil)
				r.AddCookie(cookies[0])
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Errorf("status = %d, want %d", w.Code, tc.want)
				}
			})
		})
	}
}

func TestOperatorLoginRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			handlerConfig{operatorToken: testOperatorToken})
		authority := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")
		origin := "http://" + authority
		doLogin := func(remoteAddr, token string) *httptest.ResponseRecorder {
			body := url.Values{"token": {token}}.Encode()
			req := httptest.NewRequest(http.MethodPost, origin+"/login", strings.NewReader(body))
			req.RemoteAddr = remoteAddr
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			return w
		}

		clientA := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "40001")
		clientB := net.JoinHostPort(net.IPv4(127, 0, 0, 2).String(), "40002")

		limited := false
		for i := range 50 {
			w := doLogin(clientA, "wrong")
			if w.Code == http.StatusTooManyRequests {
				if w.Header().Get("Retry-After") == "" {
					t.Fatal("429 response missing Retry-After header")
				}
				limited = true
				break
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("attempt %d: status = %d, want 401 or 429", i, w.Code)
			}
		}
		if !limited {
			t.Fatal("repeated wrong login attempts were never rate limited")
		}

		for _, remote := range []string{clientA, clientB} {
			w := doLogin(remote, testOperatorToken)
			if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 {
				t.Errorf("valid login status = %d, cookies = %d", w.Code, len(w.Result().Cookies()))
			}
		}
	})
}

func TestOperatorLoginOriginAndHostValidation(t *testing.T) {
	h := newHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		handlerConfig{operatorToken: testOperatorToken})
	authority := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")
	validBody := url.Values{"token": {testOperatorToken}}.Encode()

	for _, tc := range []struct {
		name       string
		host       string
		origin     string
		fetchSite  string
		wantStatus int
		wantSecure bool
	}{
		{"empty host with bare http origin", "", "http://", "", http.StatusForbidden, false},
		{"empty host with fetch site only", "", "", "same-origin", http.StatusForbidden, false},
		{"tls nil fetch site without origin", authority, "", "same-origin", http.StatusForbidden, false},
		{"tls nil https origin with fetch site", authority, "https://" + authority, "same-origin", http.StatusSeeOther, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://"+authority+"/login", strings.NewReader(validBody))
			req.Host = tc.host
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			cookies := w.Result().Cookies()
			if tc.wantStatus != http.StatusSeeOther {
				if len(cookies) != 0 {
					t.Fatal("refused login set a cookie")
				}
				return
			}
			if len(cookies) != 1 || cookies[0].Secure != tc.wantSecure {
				t.Fatalf("cookie count = %d, secure = %v, want 1 and %v", len(cookies), len(cookies) == 1 && cookies[0].Secure, tc.wantSecure)
			}
		})
	}
}

func TestNonCanonicalPathsRefusedBeforeExemptions(t *testing.T) {
	st, _, _, sanctum, citadel := newIntegrationTestEnv(t, domain.ConclaveWards{}, defaultIntegrationSecrets())
	addMessageTestLease(t, st)
	taskToken, err := model.MintTaskToken(t.Context(), st, "task", "worker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	routes := []string{
		"GET /api/../healthz",
		"GET /ui/../login",
		"POST /api/v1/./hooks/github",
		"POST /ui/../gate/mcp",
		"GET //healthz",
		"POST /api/v1/./a2a",
	}
	for _, mode := range []struct {
		name string
		h    http.Handler
	}{{"sanctum", sanctum}, {"citadel", citadel}} {
		for _, cred := range []struct{ name, token string }{
			{"missing", ""},
			{"task lease", taskToken},
			{"operator", testOperatorToken},
		} {
			for _, route := range routes {
				t.Run(mode.name+"/"+cred.name+"/"+route, func(t *testing.T) {
					method, rawPath, _ := strings.Cut(route, " ")
					req := httptest.NewRequest(method, rawPath, strings.NewReader("{}"))
					if cred.token != "" {
						req.Header.Set("Authorization", "Bearer "+cred.token)
					}
					w := httptest.NewRecorder()
					mode.h.ServeHTTP(w, req)
					if w.Code != http.StatusUnauthorized {
						t.Fatalf("status = %d, want 401 (location=%q)", w.Code, w.Header().Get("Location"))
					}
				})
			}
		}
	}
}

func TestVoiceBodyLimits(t *testing.T) {
	h := newHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, conclave.NewBroker(conclave.Options{}), nil, nil, nil,
		handlerConfig{operatorToken: testOperatorToken})

	t.Run("speak enforces 2 MiB limit", func(t *testing.T) {
		oversized := `{"text":"` + strings.Repeat("a", (2<<20)+1024) + `"}`
		req := operatorRequest(http.MethodPost, "/api/v1/voice/speak", strings.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("speak oversized status = %d, want 413 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("transcribe enforces its own limit", func(t *testing.T) {
		oversized := strings.Repeat("a", (24<<20)+1024)
		req := operatorRequest(http.MethodPost, "/api/v1/voice/transcribe", strings.NewReader(oversized))
		req.Header.Set("Content-Type", "audio/webm")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("transcribe oversized status = %d, want 413 (body=%s)", w.Code, w.Body.String())
		}
	})

	for _, tc := range []struct {
		path  string
		limit int
	}{
		{"/api/v1/voice/transcribe", voiceTranscribeMaxBytes},
		{"/api/v1/conclaves/room/voice-decree", 2 << 20},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			if err := form.WriteField("language", strings.Repeat("a", tc.limit+1)); err != nil {
				t.Fatal(err)
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			req := operatorRequest(http.MethodPost, tc.path, &body)
			req.Header.Set("Content-Type", form.FormDataContentType())
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversized multipart status = %d, want 413 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestLoginLimiterMemoryBound(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newLoginLimiter(3, time.Minute, 4)

	// Block client-0.
	for range 3 {
		lim.recordFailure("client-0", now)
	}
	if lim.recordFailure("client-0", now) == 0 {
		t.Fatal("client-0 should be blocked")
	}

	// Fill and overflow with single failures from distinct clients.
	for i := 1; i <= 20; i++ {
		client := "client-" + string(rune('a'+i))
		lim.recordFailure(client, now.Add(time.Duration(i)*time.Second))
		if len(lim.entries) > 4 {
			t.Fatalf("limiter entries = %d, exceeds bound 4", len(lim.entries))
		}
	}

	// Blocked client-0 must not have been evicted by low-count churn.
	if lim.recordFailure("client-0", now.Add(30*time.Second)) == 0 {
		t.Fatal("blocked client-0 was evicted before its window expired")
	}

	// If all slots are blocked, new unknown clients fail closed until expiry.
	for i := 1; i < 4; i++ {
		c := "blocked-" + string(rune('0'+i))
		for range 3 {
			lim.recordFailure(c, now)
		}
	}
	if lim.recordFailure("fresh-client", now.Add(10*time.Second)) == 0 {
		t.Fatal("saturated limiter should fail closed for new clients")
	}
	if lim.recordFailure("fresh-client", now.Add(2*time.Minute)) != 0 {
		t.Fatal("limiter should recover after entries expire")
	}
}

func TestObserverTokenDigestEqual(t *testing.T) {
	for _, tc := range []struct {
		name, got, want string
		expect          bool
	}{
		{"matching", "obs-secret-token", "obs-secret-token", true},
		{"same length mismatch", "obs-secret-tokeX", "obs-secret-token", false},
		{"different length mismatch", "short", "obs-secret-token", false},
		{"empty got", "", "obs-secret-token", false},
		{"empty want", "obs-secret-token", "", false},
		{"both empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenDigestEqual(tc.got, tc.want); got != tc.expect {
				t.Errorf("tokenDigestEqual = %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestOperatorCredentialFormat(t *testing.T) {
	for name, token := range map[string]string{
		"short": "op_ab", "wrong prefix": strings.Repeat("a", 64), "nonhex": "op_" + strings.Repeat("g", 64),
		"empty": "", "null": "null", "oversized": "op_" + strings.Repeat("0", maxBearerSize),
	} {
		t.Run(name, func(t *testing.T) {
			if newOperatorAuth(token).matches(token) {
				t.Error("invalid configured credential authenticated")
			}
		})
	}
}

func TestLoginAddressBuckets(t *testing.T) {
	ip4 := netip.AddrFrom4([4]byte{127, 0, 0, 1})
	ip6 := netip.AddrFrom16([16]byte{0xfe, 0x80, 15: 1})
	mapped := netip.AddrFrom16(ip4.As16())
	for _, tc := range []struct {
		name string
		ip   netip.Addr
		want string
	}{
		{"mapped", mapped, ip4.String()}, {"zoned", ip6.WithZone("test"), netip.PrefixFrom(ip6, 64).Masked().String()},
		{"same subnet", ip6.Next(), netip.PrefixFrom(ip6, 64).Masked().String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := loginClientKey(net.JoinHostPort(tc.ip.String(), "42"))
			if key != tc.want {
				t.Errorf("key = %q, want %q", key, tc.want)
			}
		})
	}
}

func TestDistributedLoginFailures(t *testing.T) {
	for _, distinct := range []int{2, 4} {
		const capacity = 4
		t.Run(strconv.Itoa(distinct), func(t *testing.T) {
			a := newOperatorAuth(testOperatorToken)
			a.limiter = newLoginLimiter(3, time.Minute, capacity)
			req := func(client int, token string) int {
				r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
				r.RemoteAddr = net.JoinHostPort(net.IPv4(127, 0, 0, byte(client+1)).String(), "80")
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Origin", "http://"+r.Host)
				w := httptest.NewRecorder()
				a.login(w, r)
				return w.Code
			}
			for i := range capacity {
				if got := req(i%distinct, "bad"); got != 401 {
					t.Fatalf("initial failure = %d", got)
				}
			}
			if got := req(capacity, "bad"); got != 429 {
				t.Errorf("new address at capacity = %d", got)
			}
			first := net.IPv4(127, 0, 0, 1).String()
			if a.limiter.entries[first].failures != capacity/distinct {
				t.Error("active failure counter was lost")
			}
			if got := req(capacity, testOperatorToken); got != 303 {
				t.Errorf("valid login at capacity = %d", got)
			}
		})
	}
}

func TestOperatorCredentialCollisions(t *testing.T) {
	for _, tc := range []struct{ name, path, token, configured, scope string }{
		{"integration on operator", "/tasks", testOperatorToken, testOperatorToken, "shadewright_bearer_token"},
		{"operator on observer", "/api/v1/observer/snapshot", testOperatorToken, "", "observer_bearer_token"},
		{"task on observer", "/api/v1/observer/snapshot", model.TokenPrefix + "test", "", "observer_bearer_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET "+tc.path, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			h := newOperatorAuth(tc.configured).requireOperatorAuth(mux, mux, secret.NewMemoryStore(map[string]string{tc.scope: tc.token}))
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}
}

func TestLogoutRevokesSessions(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		want         int
		revoked      bool
	}{
		{"same origin", "same", 303, true}, {"missing origin", "", 403, false}, {"opaque origin", "null", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, handlerConfig{operatorToken: testOperatorToken})
			login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token="+testOperatorToken))
			login.Header.Set("Origin", "http://"+login.Host)
			login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, login)
			cookies := w.Result().Cookies()
			if w.Code != 303 || len(cookies) != 1 {
				t.Fatalf("login status %d, cookies %d", w.Code, len(cookies))
			}
			r := httptest.NewRequest(http.MethodPost, "/logout", nil)
			origin := tc.origin
			if origin == "same" {
				origin = "http://" + r.Host
			}
			r.Header.Set("Origin", origin)
			r.AddCookie(cookies[0])
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("logout = %d, want %d", w.Code, tc.want)
			}
			if tc.revoked {
				c := w.Result().Cookies()
				if len(c) != 1 || c[0].Name != operatorCookie || c[0].MaxAge >= 0 || c[0].Path != "/" || c[0].Value != "" {
					t.Fatal("missing session deletion")
				}
			}
			r = httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil)
			r.AddCookie(cookies[0])
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if (tc.revoked && w.Code != 401) || (!tc.revoked && w.Code != 200) {
				t.Errorf("old cookie returned %d", w.Code)
			}
		})
	}
}

func TestSecureLoginCookieOnHTTPLoopback(t *testing.T) {
	_, _, _, h, _ := newIntegrationTestEnv(t, domain.ConclaveWards{}, defaultIntegrationSecrets())
	ipv4 := net.IPv4(127, 0, 0, 1)
	for _, tc := range []struct {
		name string
		ip   net.IP
		tcp  bool
	}{
		{"IPv4", ipv4, false}, {"IPv6", net.IPv6loopback, false}, {"TCP", ipv4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := "http://" + net.JoinHostPort(tc.ip.String(), "7432")
			var client *http.Client
			if tc.tcp {
				l, err := net.Listen("tcp", net.JoinHostPort(tc.ip.String(), "0"))
				if err != nil {
					t.Fatal(err)
				}
				server := &httptest.Server{Listener: l, Config: &http.Server{Handler: h}}
				server.Start()
				t.Cleanup(server.Close)
				origin, client = server.URL, server.Client()
			} else {
				client = httptest.NewTestServer(t, h).Client()
			}
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client.Jar = jar
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/login", strings.NewReader("token="+testOperatorToken))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			login, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer login.Body.Close()
			if login.StatusCode != http.StatusSeeOther || !strings.Contains(login.Header.Get("Set-Cookie"), "; Secure") || login.Header.Get("Location") != "/" {
				t.Fatal("login did not redirect to the dashboard with a Secure cookie")
			}
			dashboard, err := client.Get(origin + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer dashboard.Body.Close()
			if dashboard.StatusCode != http.StatusOK {
				t.Errorf("dashboard status %d", dashboard.StatusCode)
			}
		})
	}
}

func TestIntegrationCollisionCannotIssueSession(t *testing.T) {
	for _, tc := range []struct {
		name, secret string
		want         int
	}{{"collision", testOperatorToken, 401}, {"distinct", "integration-test", 303}} {
		t.Run(tc.name, func(t *testing.T) {
			secrets := secret.NewMemoryStore(map[string]string{"shadewright_bearer_token": tc.secret})
			h := newHandler(nil, secrets, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, handlerConfig{operatorToken: testOperatorToken})
			r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token="+testOperatorToken))
			r.Header.Set("Origin", "http://"+r.Host)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want || (w.Code == 401 && len(w.Result().Cookies()) != 0) {
				t.Errorf("status %d, cookies %d", w.Code, len(w.Result().Cookies()))
			}
		})
	}
}

func TestLoginLimiterRetryAfterCoversEveryGate(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newLoginLimiter(1, time.Minute, 2)
	if lim.recordFailure("x", now) != 0 || lim.recordFailure("y", now.Add(10*time.Second)) != 0 {
		t.Fatal("first failures must be counted")
	}
	// The global cap is full until second 60, but y stays blocked until second 70.
	if got := lim.recordFailure("y", now.Add(20*time.Second)); got != 50*time.Second {
		t.Fatalf("retry delay = %s, want 50s", got)
	}
}

func TestLoginLimiterClientTableSaturation(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newLoginLimiter(3, time.Minute, 3)
	// Spread entries over two global windows so the table is full while the global count is not.
	for _, step := range []struct {
		client string
		at     time.Duration
	}{{"z", 0}, {"a", 20 * time.Second}, {"b", 61 * time.Second}, {"c", 62 * time.Second}} {
		if lim.recordFailure(step.client, now.Add(step.at)) != 0 {
			t.Fatalf("%s must be counted", step.client)
		}
	}
	if lim.global.failures >= lim.maxEntries {
		t.Fatal("test setup must leave the global cap open")
	}
	if len(lim.entries) != 3 {
		t.Fatalf("table holds %d entries, want 3", len(lim.entries))
	}
	if lim.recordFailure("d", now.Add(63*time.Second)) == 0 {
		t.Fatal("a full table of active entries must refuse a new client")
	}
	if lim.recordFailure("d", now.Add(81*time.Second)) != 0 {
		t.Fatal("a new client must be admitted once an entry expires")
	}
}
