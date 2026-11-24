package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
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
					if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" ||
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
