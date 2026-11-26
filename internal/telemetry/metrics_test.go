package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestMetricsHandlerExposesPrometheusFormat(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.Migrate(ctx)

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d1", AgentCardJSON: "{}",
	})
	_ = st.CreateTask(ctx, domain.Task{
		ID: "t1", IdempotencyKey: "k1", WorkloadName: "triage-agent", State: domain.TaskStateWorking,
	})
	_ = st.RecordBudgetUsage(ctx, domain.BudgetEntry{
		TaskID: "t1", WorkloadName: "triage-agent", Provider: "anthropic", Model: "claude-sonnet-4-5",
		InputTokens: 500, CacheWriteTokens: 200, CacheReadTokens: 1200, OutputTokens: 100, CostMicroUSD: 3000,
	})

	rec := httptest.NewRecorder()
	MetricsHandler(st).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`sablecourt_workloads_total{zone="runtime"} 1`,
		`sablecourt_tasks_total{state="working"} 1`,
		`sablecourt_budget_input_tokens_total 500`,
		`sablecourt_budget_cache_write_tokens_total 200`,
		`sablecourt_budget_cache_read_tokens_total 1200`,
		`sablecourt_budget_output_tokens_total 100`,
		`sablecourt_budget_cost_microusd_total 3000`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /metrics output:\n%s", want, body)
		}
	}
}

func TestMetricsHandlerConclaveWardTripsAndFallbacks(t *testing.T) {
	cases := []struct {
		name      string
		actions   []string
		wantWards string
		wantFB    string
	}{
		{
			name:      "counts conclave.ward events emitted by broker",
			actions:   []string{"conclave.ward"},
			wantWards: "sablecourt_conclave_ward_trips_recent 1",
			wantFB:    "sablecourt_model_fallbacks_recent 0",
		},
		{
			name:      "ignores unrelated conclave events and stale name",
			actions:   []string{"conclave.open", "conclave.post", "conclave.ward_tripped", "conclave.ward", "conclave.ward"},
			wantWards: "sablecourt_conclave_ward_trips_recent 2",
			wantFB:    "sablecourt_model_fallbacks_recent 0",
		},
		{
			name:      "counts both conclave.ward and model.fallback",
			actions:   []string{"conclave.ward", "model.fallback"},
			wantWards: "sablecourt_conclave_ward_trips_recent 1",
			wantFB:    "sablecourt_model_fallbacks_recent 1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}

			for _, action := range tc.actions {
				if err := st.AppendAudit(ctx, domain.AuditRecord{
					Category: "conclave",
					Action:   action,
					Outcome:  "paused",
				}); err != nil {
					t.Fatalf("AppendAudit %q: %v", action, err)
				}
			}

			rec := httptest.NewRecorder()
			MetricsHandler(st).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantWards) {
				t.Errorf("missing %q in /metrics output:\n%s", tc.wantWards, body)
			}
			if !strings.Contains(body, tc.wantFB) {
				t.Errorf("missing %q in /metrics output:\n%s", tc.wantFB, body)
			}
		})
	}
}
