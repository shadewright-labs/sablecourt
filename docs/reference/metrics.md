# Metrics reference

`GET /metrics` returns Prometheus text format 0.0.4, written by hand in the daemon. It needs the operator credential, so a scraper sends `Authorization: Bearer <operator token>`. A store failure while reading workloads is a plain-text 500.

Status: built.

The metric names, types and label names are checked by `TestMetricsReferenceListsEveryMetric` in `internal/telemetry`, which `go test ./...` runs: it fails when a metric the handler writes is missing here, a listed metric is not written, or a type or the set of label names differs. The label values in parentheses are not checked.

## Metrics

Source: `internal/telemetry/metrics.go`.

| Name | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `sablecourt_workloads_total` | gauge | `zone` (`runtime`, `sandbox`) | Declared workloads per zone. Both labels are always present. |
| `sablecourt_tasks_total` | gauge | `state` | Tasks per state. Only states that have tasks appear. States: `submitted`, `working`, `input-required`, `suspended`, `completed`, `failed`, `cancelled`. |
| `sablecourt_approvals_pending` | gauge | none | Approvals waiting for a decision. |
| `sablecourt_budget_input_tokens_total` | counter | none | Uncached input tokens recorded in the budget ledger. |
| `sablecourt_budget_cache_write_tokens_total` | counter | none | Cache write input tokens recorded in the budget ledger. |
| `sablecourt_budget_cache_read_tokens_total` | counter | none | Cache read input tokens recorded in the budget ledger. |
| `sablecourt_budget_output_tokens_total` | counter | none | Output tokens recorded in the budget ledger. |
| `sablecourt_budget_cost_microusd_total` | counter | none | Cost in millionths of a US dollar. |
| `sablecourt_conclaves_total` | gauge | `state` | Conclave sessions per state: `active`, `input-required`, `completed`, `cancelled`. Only states that have sessions appear. |
| `sablecourt_conclave_ward_trips_recent` | gauge | none | Audit rows with action `conclave.ward` among the last 500 rows. |
| `sablecourt_model_fallbacks_recent` | gauge | none | Audit rows with action `model.fallback` among the last 500 rows. |
| `sablecourt_audit_events_recent` | gauge | `outcome` | The last 500 audit rows, counted by outcome. The set of outcomes is open. |

The five budget series are computed from the ledger totals on each scrape, so they can fall if ledger rows are removed. The `_recent` series look at a sliding window of audit rows, not at time.

## In depth

- [Audit](../how-it-works/audit.md) for what the audit rows record, and [limits](limits.md) for the audit list sizes.
- [Model gate](../how-it-works/model-gate.md) for fallbacks and [conclaves](../how-it-works/conclaves.md) for Wards.
