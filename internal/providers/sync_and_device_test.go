package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

func runtimeKey(prefix string) string {
	return prefix + strings.Repeat("a", 24)
}

func TestFetchModelsWithHTTPTestServers(t *testing.T) {
	ctx := context.Background()

	t.Run("OpenAI filters chat models, sorts and verifies bearer header", func(t *testing.T) {
		apiKey := runtimeKey("sk-")
		var gotAuth, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[
				{"id":"o3-mini"},
				{"id":"gpt-5"},
				{"id":"o1-preview"},
				{"id":"o4-mini"},
				{"id":"chatgpt-4o-latest"},
				{"id":"gpt-4o-audio-preview"},
				{"id":"gpt-4o-realtime-preview"},
				{"id":"text-embedding-3-large"},
				{"id":"whisper-1"},
				{"id":"tts-1"},
				{"id":"dall-e-3"},
				{"id":"omni-moderation-latest"},
				{"id":"babbage-002"},
				{"id":"davinci-002"},
				{"id":"custom-other"}
			]}`))
		}))
		defer srv.Close()

		secrets := secret.NewMemoryStore(map[string]string{"openai_api_key": apiKey})
		reg := NewRegistry(Options{
			Secrets: secrets,
			Client:  srv.Client(),
			BaseURL: map[string]string{"openai": srv.URL + "/"},
		})

		models, err := reg.SyncModels(ctx, "openai")
		if err != nil {
			t.Fatalf("SyncModels openai: %v", err)
		}
		if gotPath != "/v1/models" || gotAuth != "Bearer "+apiKey {
			t.Fatalf("request path=%q auth=%q", gotPath, gotAuth)
		}
		var ids []string
		for _, m := range models {
			if m.Provider != "openai" {
				t.Errorf("model provider = %q, want openai", m.Provider)
			}
			ids = append(ids, m.ID)
		}
		want := []string{"chatgpt-4o-latest", "gpt-5", "o1-preview", "o3-mini", "o4-mini"}
		if !slices.Equal(ids, want) {
			t.Fatalf("openai models = %v, want %v", ids, want)
		}
	})

	t.Run("Local keeps all model IDs and optionally sends bearer header", func(t *testing.T) {
		var authHeaders []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeaders = append(authHeaders, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5-coder:32b"},{"id":"llama3.3:70b"}]}`))
		}))
		defer srv.Close()

		secrets := secret.NewMemoryStore(nil)
		reg := NewRegistry(Options{
			Secrets: secrets,
			Client:  srv.Client(),
			BaseURL: map[string]string{"local": srv.URL},
		})

		models, err := reg.SyncModels(ctx, "local")
		if err != nil {
			t.Fatalf("SyncModels local (no key): %v", err)
		}
		if len(models) != 2 || models[0].ID != "llama3.3:70b" || models[1].ID != "qwen2.5-coder:32b" {
			t.Fatalf("local models = %+v", models)
		}
		if authHeaders[0] != "" {
			t.Errorf("expected empty Authorization without secret, got %q", authHeaders[0])
		}

		var localStatus ProviderStatus
		for _, st := range reg.List(ctx) {
			if st.ID == "local" {
				localStatus = st
			}
		}
		if !localStatus.Connected || localStatus.Detail != "local endpoint reachable" || localStatus.LastSyncedAt == nil {
			t.Fatalf("local status after sync = %+v", localStatus)
		}

		localKey := runtimeKey("loc-")
		if err := reg.Connect(ctx, "local", localKey); err != nil {
			t.Fatalf("Connect local: %v", err)
		}
		if _, err := reg.SyncModels(ctx, "local"); err != nil {
			t.Fatalf("SyncModels local (with key): %v", err)
		}
		if authHeaders[1] != "Bearer "+localKey {
			t.Errorf("second Authorization = %q, want Bearer key", authHeaders[1])
		}
	})

	t.Run("Anthropic verifies headers and sorts models", func(t *testing.T) {
		apiKey := runtimeKey("ant-")
		var gotKey, gotVer, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotKey = r.Header.Get("x-api-key")
			gotVer = r.Header.Get("anthropic-version")
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5-5"},{"id":"claude-haiku-5-5"}]}`))
		}))
		defer srv.Close()

		reg := NewRegistry(Options{
			Secrets: secret.NewMemoryStore(map[string]string{"anthropic_api_key": apiKey}),
			Client:  srv.Client(),
			BaseURL: map[string]string{"anthropic": srv.URL},
		})
		models, err := reg.SyncModels(ctx, "anthropic")
		if err != nil {
			t.Fatalf("SyncModels anthropic: %v", err)
		}
		if gotPath != "/v1/models" || gotKey != apiKey || gotVer != "2023-06-01" {
			t.Fatalf("headers path=%q key=%q ver=%q", gotPath, gotKey, gotVer)
		}
		if len(models) != 2 || models[0].ID != "claude-haiku-5-5" || models[1].ID != "claude-sonnet-5-5" {
			t.Fatalf("anthropic models = %+v", models)
		}
	})

	t.Run("Gemini filters by generateContent, strips prefix and sorts", func(t *testing.T) {
		apiKey := runtimeKey("gem-")
		var gotKey, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotKey = r.Header.Get("x-goog-api-key")
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[
				{"name":"models/gemini-4-neon","supportedGenerationMethods":["generateContent"]},
				{"name":"models/gemini-default-methods","supportedGenerationMethods":[]},
				{"name":"models/embedding-001","supportedGenerationMethods":["embedContent"]}
			]}`))
		}))
		defer srv.Close()

		reg := NewRegistry(Options{
			Secrets: secret.NewMemoryStore(map[string]string{"gemini_api_key": apiKey}),
			Client:  srv.Client(),
			BaseURL: map[string]string{"gemini": srv.URL},
		})
		models, err := reg.SyncModels(ctx, "gemini")
		if err != nil {
			t.Fatalf("SyncModels gemini: %v", err)
		}
		if gotPath != "/v1beta/models" || gotKey != apiKey {
			t.Fatalf("headers path=%q key=%q", gotPath, gotKey)
		}
		var ids []string
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		want := []string{"gemini-4-neon", "gemini-default-methods"}
		if !slices.Equal(ids, want) {
			t.Fatalf("gemini models = %v, want %v", ids, want)
		}
	})

	t.Run("fetch error handling records lastErrors across all providers", func(t *testing.T) {
		for _, providerID := range []string{"openai", "anthropic", "gemini", "local"} {
			for _, tc := range []struct {
				name   string
				status int
				body   string
				badURL bool
				closed bool
				want   string
			}{
				{"http 401", http.StatusUnauthorized, `{"error":"denied"}`, false, false, "HTTP 401"},
				{"http 500", http.StatusInternalServerError, `server down`, false, false, "HTTP 500"},
				{"malformed JSON", http.StatusOK, `{not-json`, false, false, "invalid character"},
				{"invalid URL", http.StatusOK, `{}`, true, false, "missing protocol scheme"},
				{"transport failure", http.StatusOK, `{}`, false, true, "connect"},
			} {
				t.Run(providerID+"/"+tc.name, func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(tc.body))
					}))
					base := srv.URL
					if tc.closed {
						srv.Close()
					} else {
						defer srv.Close()
					}
					if tc.badURL {
						base = "://bad-url"
					}
					secrets := secret.NewMemoryStore(map[string]string{
						"openai_api_key":    runtimeKey("sk-"),
						"anthropic_api_key": runtimeKey("ant-"),
						"gemini_api_key":    runtimeKey("gem-"),
					})
					reg := NewRegistry(Options{
						Secrets: secrets,
						Client:  srv.Client(),
						BaseURL: map[string]string{providerID: base},
					})
					_, err := reg.SyncModels(ctx, providerID)
					if err == nil || !strings.Contains(err.Error(), tc.want) {
						t.Fatalf("SyncModels(%s) err = %v, want %q", providerID, err, tc.want)
					}
					var found DetailCheck
					for _, st := range reg.List(ctx) {
						if st.ID == providerID {
							found = DetailCheck{ok: true, detail: st.Detail}
						}
					}
					if !found.ok || !strings.Contains(found.detail, tc.want) {
						t.Fatalf("List detail for %s = %q, want %q", providerID, found.detail, tc.want)
					}
				})
			}
		}
	})
}

type DetailCheck struct {
	ok     bool
	detail string
}

func TestSyncCLIModels(t *testing.T) {
	ctx := context.Background()

	t.Run("missing binary returns error and updates List detail", func(t *testing.T) {
		for _, id := range []string{"cli:codex", "cli:claude"} {
			t.Run(id, func(t *testing.T) {
				reg := NewRegistry(Options{})
				reg.look = func(string) (string, error) { return "", errors.New("not found") }
				if _, err := reg.SyncModels(ctx, id); err == nil || !strings.Contains(err.Error(), "binary not found in PATH") {
					t.Fatalf("SyncModels(%s) = %v", id, err)
				}
				var detail string
				for _, st := range reg.List(ctx) {
					if st.ID == id {
						detail = st.Detail
					}
				}
				if !strings.Contains(detail, "binary not found in PATH") {
					t.Fatalf("List detail = %q", detail)
				}
			})
		}
	})

	t.Run("codex reads cache with slug/id fallback and deduplication", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		cacheDir := filepath.Join(home, ".codex")
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			t.Fatal(err)
		}
		cacheJSON := `{"models":[
			{"slug":"gpt-6.2-sol","id":"ignored-when-slug"},
			{"slug":"  ","id":"fallback-id"},
			{"slug":"gpt-6.2-sol"},
			{"slug":"","id":""}
		]}`
		if err := os.WriteFile(filepath.Join(cacheDir, "models_cache.json"), []byte(cacheJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry(Options{})
		reg.look = func(bin string) (string, error) { return "/usr/bin/" + bin, nil }
		models, err := reg.SyncModels(ctx, "cli:codex")
		if err != nil {
			t.Fatalf("SyncModels cli:codex: %v", err)
		}
		var ids []string
		for _, m := range models {
			if m.Provider != "cli:codex" {
				t.Errorf("provider = %q, want cli:codex", m.Provider)
			}
			ids = append(ids, m.ID)
		}
		if !slices.Equal(ids, []string{"gpt-6.2-sol", "fallback-id"}) {
			t.Fatalf("codex cached models = %v", ids)
		}
	})

	t.Run("codex falls back to defaults when cache missing or corrupt", func(t *testing.T) {
		def, _ := findCLIDef("cli:codex")
		for _, tc := range []struct {
			name    string
			content string
			write   bool
		}{
			{"missing cache", "", false},
			{"corrupt JSON", "{", true},
			{"empty models list", `{"models":[]}`, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				if tc.write {
					cacheDir := filepath.Join(home, ".codex")
					if err := os.MkdirAll(cacheDir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(cacheDir, "models_cache.json"), []byte(tc.content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				reg := NewRegistry(Options{})
				reg.look = func(bin string) (string, error) { return "/usr/bin/" + bin, nil }
				models, err := reg.SyncModels(ctx, "cli:codex")
				if err != nil {
					t.Fatalf("SyncModels cli:codex: %v", err)
				}
				var ids []string
				for _, m := range models {
					ids = append(ids, m.ID)
				}
				if !slices.Equal(ids, def.DefaultModels) {
					t.Fatalf("fallback models = %v, want %v", ids, def.DefaultModels)
				}
			})
		}
	})

	t.Run("claude merges live Anthropic models ahead of defaults and tolerates API failure", func(t *testing.T) {
		def, _ := findCLIDef("cli:claude")
		apiKey := runtimeKey("ant-")
		var fail atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if r.Header.Get("x-api-key") != apiKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-live-new"},{"id":"claude-opus-5-5"}]}`))
		}))
		defer srv.Close()

		reg := NewRegistry(Options{
			Secrets: secret.NewMemoryStore(map[string]string{"anthropic_api_key": apiKey}),
			Client:  srv.Client(),
			BaseURL: map[string]string{"anthropic": srv.URL},
		})
		reg.look = func(bin string) (string, error) { return "/usr/bin/" + bin, nil }

		models, err := reg.SyncModels(ctx, "cli:claude")
		if err != nil {
			t.Fatalf("SyncModels cli:claude: %v", err)
		}
		var ids []string
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		if ids[0] != "claude-live-new" {
			t.Fatalf("expected live model first, got %v", ids)
		}
		count := 0
		for _, id := range ids {
			if id == "claude-opus-5-5" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("duplicate model claude-opus-5-5 count = %d in %v", count, ids)
		}

		// When the live Anthropic API fails, cli:claude still succeeds with defaults.
		fail.Store(true)
		fallback, err := reg.SyncModels(ctx, "cli:claude")
		if err != nil {
			t.Fatalf("SyncModels cli:claude with failing API: %v", err)
		}
		var fbIDs []string
		for _, m := range fallback {
			fbIDs = append(fbIDs, m.ID)
		}
		if !slices.Equal(fbIDs, def.DefaultModels) {
			t.Fatalf("fallback models = %v, want %v", fbIDs, def.DefaultModels)
		}

		// Synced CLI status shows LastSyncedAt in List.
		var synced *time.Time
		for _, st := range reg.List(ctx) {
			if st.ID == "cli:claude" {
				synced = st.LastSyncedAt
			}
		}
		if synced == nil {
			t.Fatal("expected LastSyncedAt after CLI sync")
		}
	})

	t.Run("unsupported CLI def returns error", func(t *testing.T) {
		reg := NewRegistry(Options{})
		reg.look = func(bin string) (string, error) { return "/usr/bin/" + bin, nil }
		if _, err := reg.syncCLIModels(ctx, cliDef{ID: "cli:other", Bin: "other"}); err == nil || !strings.Contains(err.Error(), "sync not supported") {
			t.Fatalf("unsupported CLI err = %v", err)
		}
	})
}

func TestRegistryValidationAndRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("Connect Disconnect SavePrefs SyncModels refusals", func(t *testing.T) {
		noSec := NewRegistry(Options{})
		if err := noSec.Connect(ctx, "unknown", "k"); err == nil {
			t.Error("Connect accepted unknown provider")
		}
		if err := noSec.Connect(ctx, "openai", "   "); err == nil {
			t.Error("Connect accepted blank API key")
		}
		if err := noSec.Connect(ctx, "openai", runtimeKey("sk-")); err == nil || !strings.Contains(err.Error(), "secret store unavailable") {
			t.Errorf("Connect without secret store = %v", err)
		}
		if err := noSec.Disconnect(ctx, "unknown"); err == nil {
			t.Error("Disconnect accepted unknown provider")
		}
		if err := noSec.Disconnect(ctx, "openai"); err == nil || !strings.Contains(err.Error(), "secret store unavailable") {
			t.Errorf("Disconnect without secret store = %v", err)
		}
		if _, err := noSec.SyncModels(ctx, "unknown"); err == nil {
			t.Error("SyncModels accepted unknown provider")
		}
		if _, err := noSec.SyncModels(ctx, "openai"); err == nil || !strings.Contains(err.Error(), "secret store unavailable") {
			t.Errorf("SyncModels without secret store = %v", err)
		}
		if err := noSec.SavePrefs(ctx, "unknown", "m1", nil); err == nil {
			t.Error("SavePrefs accepted unknown provider")
		}

		// SavePrefs prepends defaultModel when not in enabledModels and deduplicates.
		if err := noSec.SavePrefs(ctx, "openai", "gpt-5", []string{" o3 ", "o3", "", "gpt-6-sol"}); err != nil {
			t.Fatalf("SavePrefs: %v", err)
		}
		var oa ProviderStatus
		for _, st := range noSec.List(ctx) {
			if st.ID == "openai" {
				oa = st
			}
		}
		if oa.DefaultModel != "gpt-5" || !slices.Equal(oa.EnabledModels, []string{"gpt-5", "o3", "gpt-6-sol"}) {
			t.Fatalf("saved prefs = default:%q enabled:%v", oa.DefaultModel, oa.EnabledModels)
		}

		// Disconnect on a missing secret is idempotent.
		reg := NewRegistry(Options{Secrets: secret.NewMemoryStore(nil)})
		if err := reg.Disconnect(ctx, "openai"); err != nil {
			t.Fatalf("Disconnect absent secret = %v", err)
		}
	})
}

func TestGitHubDeviceFlowWithHTTPTestServer(t *testing.T) {
	ctx := context.Background()

	t.Run("NewGitHubDeviceFlow defaults", func(t *testing.T) {
		f := NewGitHubDeviceFlow("Iv1.test")
		if f.ClientID != "Iv1.test" || f.BaseURL != "https://github.com" || f.Client == nil {
			t.Fatalf("NewGitHubDeviceFlow = %+v", f)
		}
		noClient := &GitHubDeviceFlow{ClientID: "Iv1.test"}
		if noClient.httpClient() == nil {
			t.Fatal("httpClient returned nil")
		}
	})

	t.Run("Start validates inputs, defaults scope and handles server errors", func(t *testing.T) {
		var gotScope, gotClientID, gotAccept, gotCT string
		var status int
		var body string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAccept = r.Header.Get("Accept")
			gotCT = r.Header.Get("Content-Type")
			_ = r.ParseForm()
			gotScope = r.Form.Get("scope")
			gotClientID = r.Form.Get("client_id")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()

		cases := []struct {
			name      string
			clientID  string
			baseURL   string
			scopes    string
			status    int
			body      string
			wantScope string
			wantErr   string
		}{
			{name: "blank client ID", clientID: " ", baseURL: srv.URL, wantErr: "client_id is required"},
			{name: "invalid base URL", clientID: "cid", baseURL: "://bad", wantErr: "missing protocol scheme"},
			{name: "default repo scope", clientID: "cid-1", baseURL: srv.URL + "/", scopes: "", status: http.StatusOK,
				body:      `{"device_code":"dev-1","user_code":"AAAA-BBBB","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`,
				wantScope: "repo"},
			{name: "custom scope", clientID: "cid-2", baseURL: srv.URL, scopes: "read:user repo", status: http.StatusOK,
				body:      `{"device_code":"dev-2","user_code":"CCCC-DDDD","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`,
				wantScope: "read:user repo"},
			{name: "http 400 error", clientID: "cid", baseURL: srv.URL, status: http.StatusBadRequest, body: `{"error":"bad"}`, wantErr: "HTTP 400"},
			{name: "invalid JSON", clientID: "cid", baseURL: srv.URL, status: http.StatusOK, body: `{`, wantErr: "unexpected EOF"},
			{name: "incomplete response missing user_code", clientID: "cid", baseURL: srv.URL, status: http.StatusOK, body: `{"device_code":"dev-1"}`, wantErr: "incomplete device code"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				status, body = tc.status, tc.body
				flow := &GitHubDeviceFlow{ClientID: tc.clientID, BaseURL: tc.baseURL, Client: srv.Client()}
				res, err := flow.Start(ctx, tc.scopes)
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("Start = %+v, %v; want %q", res, err, tc.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("Start unexpected err: %v", err)
				}
				if gotScope != tc.wantScope || gotClientID != tc.clientID || gotAccept != "application/json" || gotCT != "application/x-www-form-urlencoded" {
					t.Fatalf("form scope=%q client=%q accept=%q ct=%q", gotScope, gotClientID, gotAccept, gotCT)
				}
				if res.DeviceCode == "" || res.UserCode == "" {
					t.Fatalf("incomplete Start result: %+v", res)
				}
			})
		}

		// Closed server triggers transport error.
		closedSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedSrv.Close()
		flow := &GitHubDeviceFlow{ClientID: "cid", BaseURL: closedSrv.URL, Client: closedSrv.Client()}
		if _, err := flow.Start(ctx, "repo"); err == nil {
			t.Fatal("Start succeeded against closed server")
		}
	})

	t.Run("PollToken transitions and errors", func(t *testing.T) {
		tokVal := runtimeKey("gho_")

		t.Run("pending then token with form verification", func(t *testing.T) {
			var calls atomic.Int32
			var gotGrant, gotDevice, gotClient string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				gotGrant = r.Form.Get("grant_type")
				gotDevice = r.Form.Get("device_code")
				gotClient = r.Form.Get("client_id")
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"` + tokVal + `"}`))
			}))
			defer srv.Close()

			flow := &GitHubDeviceFlow{ClientID: "cid-poll", BaseURL: srv.URL, Client: srv.Client()}
			got, err := flow.PollToken(ctx, "dev-poll", time.Nanosecond)
			if err != nil || got != tokVal {
				t.Fatalf("PollToken = %q, %v; want %q", got, err, tokVal)
			}
			if gotGrant != "urn:ietf:params:oauth:grant-type:device_code" || gotDevice != "dev-poll" || gotClient != "cid-poll" {
				t.Fatalf("form grant=%q device=%q client=%q", gotGrant, gotDevice, gotClient)
			}
		})

		for _, tc := range []struct {
			name     string
			response string
			interval time.Duration
			cancelOn bool
			wantErr  string
		}{
			{"expired_token", `{"error":"expired_token"}`, time.Nanosecond, false, "github device flow failed: expired_token"},
			{"access_denied", `{"error":"access_denied"}`, time.Nanosecond, false, "github device flow failed: access_denied"},
			{"other oauth error", `{"error":"incorrect_client_credentials"}`, time.Nanosecond, false, "github oauth error: incorrect_client_credentials"},
			{"slow_down honors cancel", `{"error":"slow_down"}`, time.Nanosecond, true, context.Canceled.Error()},
			{"non-positive interval defaults and honors cancel", `{"error":"authorization_pending"}`, 0, true, context.Canceled.Error()},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pollCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.response))
					if tc.cancelOn {
						cancel()
					}
				}))
				defer srv.Close()

				flow := &GitHubDeviceFlow{ClientID: "cid", BaseURL: srv.URL, Client: srv.Client()}
				_, err := flow.PollToken(pollCtx, "dev-1", tc.interval)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("PollToken = %v, want %q", err, tc.wantErr)
				}
			})
		}

		t.Run("invalid URL and transport error", func(t *testing.T) {
			badFlow := &GitHubDeviceFlow{ClientID: "cid", BaseURL: "://bad"}
			if _, err := badFlow.PollToken(ctx, "dev-1", time.Nanosecond); err == nil {
				t.Fatal("PollToken accepted bad URL")
			}
			closedSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			closedSrv.Close()
			closedFlow := &GitHubDeviceFlow{ClientID: "cid", BaseURL: closedSrv.URL, Client: closedSrv.Client()}
			if _, err := closedFlow.PollToken(ctx, "dev-1", time.Nanosecond); err == nil {
				t.Fatal("PollToken succeeded against closed server")
			}
		})
	})
}
