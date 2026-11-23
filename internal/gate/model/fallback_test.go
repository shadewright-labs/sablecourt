package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestModelGateFallsBackFromRateLimitedOpenAIToLocal(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "mg.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "agent-1", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent})
	_ = st.CreateTask(ctx, domain.Task{ID: "t-fb-1", WorkloadName: "agent-1", State: domain.TaskStateWorking, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	tok, err := MintTaskToken(ctx, st, "t-fb-1", "agent-1", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	bundle := &config.Bundle{
		Budgets: map[string]domain.Budget{
			"b1": {Metadata: domain.ObjectMeta{Name: "b1"}, Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxCostUSD: 5}}},
		},
		Workloads: map[string]domain.Workload{
			"agent-1": {
				Metadata: domain.ObjectMeta{Name: "agent-1", Zone: domain.ZoneRuntime},
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

	cli := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Host, "openai.com") {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"model":"qwen3:8b","usage":{"prompt_tokens":10,"completion_tokens":5}}`)),
			}, nil
		}),
	}

	gate := New(Options{
		Store:   st,
		Secrets: secret.NewMemoryStore(map[string]string{"openai_api_key": "sk-test-123"}),
		Bundle:  bundle,
		Client:  cli,
	})

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	gate.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after fallback to local, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Sablecourt-Fallback-Provider") != "local" {
		t.Fatalf("expected X-Sablecourt-Fallback-Provider=local, got %q", rec.Header().Get("X-Sablecourt-Fallback-Provider"))
	}
}
