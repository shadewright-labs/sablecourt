package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// SC-05: Fuzz and unit tests for joinUpstreamURL SSRF / authority override protection.
func TestSC05_JoinUpstreamURLBlocksSSRFAndTraversal(t *testing.T) {
	base := "https://api.openai.com"
	badPaths := []string{
		"/../admin",
		"/v1/../../etc/passwd",
		"/%2e%2e/secret",
		"/@evil.com/v1/chat/completions",
		"/https://evil.com/v1/chat/completions",
		"/v1\\..\\secret",
		"/v1/chat\r\nX-Injected: 1",
	}
	for _, p := range badPaths {
		if got, err := joinUpstreamURL(base, p, ""); err == nil {
			t.Errorf("expected joinUpstreamURL(%q, %q) to fail, got %q", base, p, got)
		}
	}

	got, err := joinUpstreamURL(base, "/v1/chat/completions", "stream=true")
	if err != nil {
		t.Fatalf("expected valid path to succeed, got %v", err)
	}
	u, err := url.Parse(got)
	if err != nil || u.Host != "api.openai.com" || u.Scheme != "https" || u.Path != "/v1/chat/completions" {
		t.Fatalf("unexpected joined URL: %q", got)
	}
}

func FuzzSC05_JoinUpstreamURLPreservesHostAndScheme(f *testing.F) {
	f.Add("/v1/chat/completions", "stream=true")
	f.Add("/../evil", "")
	f.Add("/%2e%2e/evil", "")
	f.Add("/@attacker.example/v1", "")
	f.Add("//attacker.example/v1", "")
	f.Add("/v1/messages", "a=1&b=2")

	const base = "https://api.openai.com"
	f.Fuzz(func(t *testing.T, reqPath, rawQuery string) {
		got, err := joinUpstreamURL(base, reqPath, rawQuery)
		if err != nil {
			return
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("joinUpstreamURL returned unparseable URL %q: %v", got, err)
		}
		if u.Scheme != "https" || u.Host != "api.openai.com" {
			t.Fatalf("SSRF host/scheme escape: input=%q -> %q (scheme=%q host=%q)", reqPath, got, u.Scheme, u.Host)
		}
		if strings.Contains(u.Path, "..") {
			t.Fatalf("path traversal survived: input=%q -> %q", reqPath, got)
		}
	})
}

// SC-06: Concurrent requests against a tight per-task budget cap under -race.
func TestSC06_ConcurrentBudgetEnforcement(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "budget-race.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "agent-race", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent})
	_ = st.CreateTask(ctx, domain.Task{ID: "t-race-1", WorkloadName: "agent-race", State: domain.TaskStateWorking, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	tok, err := MintTaskToken(ctx, st, "t-race-1", "agent-race", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	// Tight budget: 100 input tokens max per task.
	bundle := &config.Bundle{
		Budgets: map[string]domain.Budget{
			"tight": {
				Metadata: domain.ObjectMeta{Name: "tight"},
				Spec: domain.BudgetSpec{
					PerTask: domain.PerTaskBudget{MaxInputTokens: 100},
				},
			},
		},
		Workloads: map[string]domain.Workload{
			"agent-race": {
				Metadata: domain.ObjectMeta{Name: "agent-race", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						ModelGate: domain.ModelGateGrant{
							Allowed:   true,
							Providers: []string{"local"},
							BudgetRef: "tight",
						},
					},
				},
			},
		},
	}

	cli := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"model":"qwen3:8b","usage":{"prompt_tokens":60,"completion_tokens":10}}`)),
			}, nil
		}),
	}

	gate := New(Options{Store: st, Bundle: bundle, Client: cli})

	// Fire 2 sequential requests first to exceed 100 input tokens (60 + 60 = 120), then verify concurrent burst gets 429.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/local/v1/chat/completions", strings.NewReader(`{"model":"qwen3:8b"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		gate.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("warmup request %d expected 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	var denied atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/local/v1/chat/completions", strings.NewReader(`{"model":"qwen3:8b"}`))
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			gate.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusTooManyRequests {
				denied.Add(1)
			}
		}()
	}
	wg.Wait()

	if denied.Load() != 12 {
		t.Fatalf("expected all 12 post-ceiling concurrent requests to fail with 429, got %d", denied.Load())
	}
}

// SC-07: Fallback from openai to local must never forward openai_api_key to local endpoint.
func TestSC07_FallbackNeverLeaksPrimarySecretToSecondaryProvider(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "fb-secret.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "agent-fb", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent})
	_ = st.CreateTask(ctx, domain.Task{ID: "t-fb-sec", WorkloadName: "agent-fb", State: domain.TaskStateWorking, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	tok, err := MintTaskToken(ctx, st, "t-fb-sec", "agent-fb", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	bundle := &config.Bundle{
		Budgets: map[string]domain.Budget{
			"b1": {Metadata: domain.ObjectMeta{Name: "b1"}, Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxCostUSD: 5}}},
		},
		Workloads: map[string]domain.Workload{
			"agent-fb": {
				Metadata: domain.ObjectMeta{Name: "agent-fb", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						ModelGate: domain.ModelGateGrant{
							Allowed:   true,
							Providers: []string{"openai", "local"},
							BudgetRef: "b1",
						},
					},
				},
			},
		},
	}

	var localAuthHeader string
	var openaiAuthHeader string
	cli := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Host, "openai.com") {
				openaiAuthHeader = r.Header.Get("Authorization")
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"overloaded"}`)),
				}, nil
			}
			localAuthHeader = r.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"model":"qwen3:8b","usage":{"prompt_tokens":5,"completion_tokens":5}}`)),
			}, nil
		}),
	}

	gate := New(Options{
		Store:   st,
		Secrets: secret.NewMemoryStore(map[string]string{"openai_api_key": "sk-super-secret-openai-key"}),
		Bundle:  bundle,
		Client:  cli,
	})

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	gate.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after fallback, got %d: %s", rec.Code, rec.Body.String())
	}
	if openaiAuthHeader != "Bearer sk-super-secret-openai-key" {
		t.Fatalf("expected openai upstream to receive openai_api_key, got %q", openaiAuthHeader)
	}
	if strings.Contains(localAuthHeader, "sk-super-secret") || localAuthHeader != "" {
		t.Fatalf("SC-07 violation: local fallback received unexpected Authorization header %q", localAuthHeader)
	}
}
