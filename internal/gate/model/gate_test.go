package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func handlerTransport(h http.Handler) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			return rec.Result(), nil
		}),
	}
}

func setupGateTest(t *testing.T, upstreams map[string]UpstreamConfig, client *http.Client) (*Gate, *sqlite.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	bundle := &config.Bundle{
		Budgets: map[string]domain.Budget{
			"agent-budget": {
				Metadata: domain.ObjectMeta{Name: "agent-budget"},
				Spec: domain.BudgetSpec{
					PerTask: domain.PerTaskBudget{
						MaxInputTokens:  1000,
						MaxOutputTokens: 500,
						MaxCostUSD:      1.00,
					},
					Daily: domain.DailyBudget{
						MaxCostUSD: 5.00,
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{
			"ai-agent": {
				Metadata: domain.ObjectMeta{Name: "ai-agent", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						ModelGate: domain.ModelGateGrant{
							Allowed:   true,
							Providers: []string{"openai", "anthropic", "gemini"},
							BudgetRef: "agent-budget",
						},
					},
				},
			},
			"det-ctrl": {
				Metadata: domain.ObjectMeta{Name: "det-ctrl", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassDeterministicController,
					Grants: domain.GrantSet{
						ModelGate: domain.ModelGateGrant{Allowed: false},
					},
				},
			},
		},
	}

	for _, w := range bundle.Workloads {
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name:           w.Metadata.Name,
			Zone:           w.Metadata.Zone,
			Class:          w.Spec.Class,
			ManifestDigest: "test",
			AgentCardJSON:  "{}",
		}); err != nil {
			t.Fatalf("upsert workload: %v", err)
		}
	}

	secrets := secret.NewMemoryStore(map[string]string{
		"openai_api_key":    "sk-real-openai",
		"anthropic_api_key": "sk-real-anthropic",
		"gemini_api_key":    "AIza-real-gemini",
	})

	gate := New(Options{
		Store:     st,
		Secrets:   secrets,
		Bundle:    bundle,
		Upstreams: upstreams,
		Client:    client,
	})
	return gate, st
}

func doGateRequest(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://sablecourt.local"+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestModelGateProxiesOpenAIAnthropicAndGemini(t *testing.T) {
	ctx := context.Background()

	var openaiGotAuth, anthropicGotKey, geminiGotKey string
	upstreamMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "openai.test":
			openaiGotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"gpt-5","usage":{"prompt_tokens":120,"completion_tokens":40}}`))
		case "anthropic.test":
			anthropicGotKey = r.Header.Get("x-api-key")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":200,\"output_tokens\":0}}}\n\n")
			_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":80}}\n\n")
		case "gemini.test":
			geminiGotKey = r.Header.Get("x-goog-api-key")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":150,"candidatesTokenCount":50}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	gate, st := setupGateTest(t, map[string]UpstreamConfig{
		"openai":    {BaseURL: "https://openai.test", SecretName: "openai_api_key", RequireSecret: true},
		"anthropic": {BaseURL: "https://anthropic.test", SecretName: "anthropic_api_key", RequireSecret: true, AnthropicVer: "2023-06-01"},
		"gemini":    {BaseURL: "https://gemini.test", SecretName: "gemini_api_key", RequireSecret: true},
	}, handlerTransport(upstreamMux))

	if err := st.CreateTask(ctx, domain.Task{
		ID: "task-1", IdempotencyKey: "idem-1", WorkloadName: "ai-agent", State: domain.TaskStateWorking,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	token, err := MintTaskToken(ctx, st, "task-1", "ai-agent", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	h := gate.Handler()

	// 1. OpenAI JSON
	rec := doGateRequest(h, http.MethodPost, "/openai/v1/chat/completions", `{"model":"gpt-5"}`, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("openai call failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 2. Anthropic SSE using x-api-key carrying task token
	rec = doGateRequest(h, http.MethodPost, "/anthropic/v1/messages", `{"model":"claude-sonnet-4-5"}`, map[string]string{
		"x-api-key": token,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("anthropic call failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 3. Gemini JSON using x-goog-api-key carrying task token
	rec = doGateRequest(h, http.MethodPost, "/gemini/v1beta/models/gemini-2.5-pro:generateContent", `{}`, map[string]string{
		"x-goog-api-key": token,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("gemini call failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	if openaiGotAuth != "Bearer sk-real-openai" {
		t.Fatalf("expected real openai key at upstream, got %q", openaiGotAuth)
	}
	if anthropicGotKey != "sk-real-anthropic" {
		t.Fatalf("expected real anthropic key at upstream, got %q", anthropicGotKey)
	}
	if geminiGotKey != "AIza-real-gemini" {
		t.Fatalf("expected real gemini key at upstream, got %q", geminiGotKey)
	}

	totals, err := st.TaskBudgetTotals(ctx, "task-1")
	if err != nil {
		t.Fatalf("TaskBudgetTotals: %v", err)
	}
	if totals.InputTokens != 470 || totals.OutputTokens != 170 {
		t.Fatalf("unexpected token totals: %+v", totals)
	}
	if totals.CostMicroUSD <= 0 {
		t.Fatalf("expected positive cost, got %d", totals.CostMicroUSD)
	}
}

func TestModelGateDeniesDeterministicController(t *testing.T) {
	ctx := context.Background()
	var upstreamCalls atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	gate, st := setupGateTest(t, map[string]UpstreamConfig{
		"openai": {BaseURL: "https://openai.test", SecretName: "openai_api_key", RequireSecret: true},
	}, handlerTransport(upstream))

	if err := st.CreateTask(ctx, domain.Task{
		ID: "task-ctrl", IdempotencyKey: "idem-ctrl", WorkloadName: "det-ctrl", State: domain.TaskStateWorking,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	token, err := MintTaskToken(ctx, st, "task-ctrl", "det-ctrl", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	rec := doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions", `{"model":"gpt-5"}`, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for deterministic controller, got %d", rec.Code)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("expected 0 upstream calls, got %d", upstreamCalls.Load())
	}

	audits, err := st.ListAudit(ctx, 10)
	if err != nil || len(audits) == 0 || audits[0].Outcome != "denied" {
		t.Fatalf("expected denied audit record, got %+v err=%v", audits, err)
	}
}

func TestModelGateEnforcesBudgetAndLeaseRevocation(t *testing.T) {
	ctx := context.Background()
	var upstreamCalls atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "gpt-5",
			"usage": map[string]int64{"prompt_tokens": 1000, "completion_tokens": 10},
		})
	})

	gate, st := setupGateTest(t, map[string]UpstreamConfig{
		"openai": {BaseURL: "https://openai.test", SecretName: "openai_api_key", RequireSecret: true},
	}, handlerTransport(upstream))

	if err := st.CreateTask(ctx, domain.Task{
		ID: "task-b", IdempotencyKey: "idem-b", WorkloadName: "ai-agent", State: domain.TaskStateWorking,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	token, err := MintTaskToken(ctx, st, "task-b", "ai-agent", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	h := gate.Handler()

	// First call consumes full 1000 input token budget.
	rec := doGateRequest(h, http.MethodPost, "/openai/v1/chat/completions", `{"model":"gpt-5"}`, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("first call expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Second call must be rejected with 429 before touching upstream.
	rec = doGateRequest(h, http.MethodPost, "/openai/v1/chat/completions", `{"model":"gpt-5"}`, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when budget exhausted, got %d", rec.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", upstreamCalls.Load())
	}

	// Revoking task leases must fail authentication with 401.
	if err := st.RevokeTaskLeases(ctx, "task-b"); err != nil {
		t.Fatalf("RevokeTaskLeases: %v", err)
	}
	rec = doGateRequest(h, http.MethodPost, "/openai/v1/chat/completions", `{"model":"gpt-5"}`, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after revocation, got %d", rec.Code)
	}
}

func TestUpstreamRedirect(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			primary := httptest.NewRequest(http.MethodGet, "/", nil).Host
			secondary := "fallback." + primary
			outside := "redirect." + primary
			var requests []string
			client := handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.Host)
				if fallback && r.URL.Host == primary {
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if r.URL.Host == outside {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Location", "https://"+outside)
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			gate, st := setupGateTest(t, map[string]UpstreamConfig{
				"openai": {BaseURL: "https://" + primary},
				"local":  {BaseURL: "https://" + secondary},
			}, client)
			want := []string{primary}
			if fallback {
				wl := gate.bundle.Workloads["ai-agent"]
				wl.Spec.Grants.ModelGate.Providers = []string{"openai", "local"}
				gate.bundle.Workloads["ai-agent"] = wl
				want = append(want, secondary)
			}
			ctx := t.Context()
			if err := st.CreateTask(ctx, domain.Task{ID: "task", WorkloadName: "ai-agent", State: domain.TaskStateWorking}); err != nil {
				t.Fatal(err)
			}
			token, err := MintTaskToken(ctx, st, "task", "ai-agent", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			rec := doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions", "{}", map[string]string{"Authorization": "Bearer " + token})
			if !slices.Equal(requests, want) || rec.Code < 400 {
				t.Fatalf("requests = %v, status = %d", requests, rec.Code)
			}
		})
	}
}
