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
		InputTokens: 500, OutputTokens: 100, CostMicroUSD: 3000,
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
		`sablecourt_budget_cost_microusd_total 3000`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /metrics output:\n%s", want, body)
		}
	}
}
