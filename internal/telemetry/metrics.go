package telemetry

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// MetricsHandler exposes observed control plane facts in Prometheus text exposition format (0.0.4).
func MetricsHandler(st store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		workloads, err := st.ListWorkloads(ctx)
		if err != nil {
			http.Error(w, "metrics error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		tasks, _ := st.ListTasks(ctx)
		pendingApprovals, _ := st.ListApprovals(ctx, domain.ApprovalPending)
		budget, _ := st.TotalBudgetTotals(ctx)
		audits, _ := st.ListAudit(ctx, 500)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		// Workloads by zone
		zoneCounts := map[string]int{"runtime": 0, "sandbox": 0}
		for _, wl := range workloads {
			zoneCounts[string(wl.Zone)]++
		}
		fmt.Fprintln(w, "# HELP sablecourt_workloads_total Declared workloads by zone.")
		fmt.Fprintln(w, "# TYPE sablecourt_workloads_total gauge")
		for _, z := range sortedKeys(zoneCounts) {
			fmt.Fprintf(w, "sablecourt_workloads_total{zone=%q} %d\n", z, zoneCounts[z])
		}

		// Tasks by state
		stateCounts := make(map[string]int)
		for _, t := range tasks {
			stateCounts[string(t.State)]++
		}
		fmt.Fprintln(w, "# HELP sablecourt_tasks_total Tasks by A2A lifecycle state.")
		fmt.Fprintln(w, "# TYPE sablecourt_tasks_total gauge")
		for _, s := range sortedKeys(stateCounts) {
			fmt.Fprintf(w, "sablecourt_tasks_total{state=%q} %d\n", s, stateCounts[s])
		}

		// Pending approvals
		fmt.Fprintln(w, "# HELP sablecourt_approvals_pending Pending out-of-band human approvals.")
		fmt.Fprintln(w, "# TYPE sablecourt_approvals_pending gauge")
		fmt.Fprintf(w, "sablecourt_approvals_pending %d\n", len(pendingApprovals))

		// Model budget totals
		fmt.Fprintln(w, "# HELP sablecourt_budget_input_tokens_total Observed model input tokens.")
		fmt.Fprintln(w, "# TYPE sablecourt_budget_input_tokens_total counter")
		fmt.Fprintf(w, "sablecourt_budget_input_tokens_total %d\n", budget.InputTokens)

		fmt.Fprintln(w, "# HELP sablecourt_budget_output_tokens_total Observed model output tokens.")
		fmt.Fprintln(w, "# TYPE sablecourt_budget_output_tokens_total counter")
		fmt.Fprintf(w, "sablecourt_budget_output_tokens_total %d\n", budget.OutputTokens)

		fmt.Fprintln(w, "# HELP sablecourt_budget_cost_microusd_total Observed model cost in micro-USD.")
		fmt.Fprintln(w, "# TYPE sablecourt_budget_cost_microusd_total counter")
		fmt.Fprintf(w, "sablecourt_budget_cost_microusd_total %d\n", budget.CostMicroUSD)

		// Recent audit outcomes
		outcomeCounts := make(map[string]int)
		for _, a := range audits {
			outcomeCounts[a.Outcome]++
		}
		fmt.Fprintln(w, "# HELP sablecourt_audit_events_recent Recent audit events by outcome (last 500).")
		fmt.Fprintln(w, "# TYPE sablecourt_audit_events_recent gauge")
		for _, o := range sortedKeys(outcomeCounts) {
			fmt.Fprintf(w, "sablecourt_audit_events_recent{outcome=%q} %d\n", o, outcomeCounts[o])
		}
	})
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
