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
| `sablecourt_budget_input_tokens_total` | counter | none | Uncached input tokens recorded in the budget ledger, including Anthropic compaction and advisor uncached input. |
| `sablecourt_budget_cache_write_tokens_total` | counter | none | Cache write input tokens recorded in the budget ledger, including Anthropic compaction, advisor, and unclassified iteration input. |
| `sablecourt_budget_cache_read_tokens_total` | counter | none | Cache read input tokens recorded in the budget ledger, including Anthropic compaction and advisor cache reads. |
| `sablecourt_budget_output_tokens_total` | counter | none | Output tokens recorded in the budget ledger, including Anthropic compaction, advisor, and unclassified iteration output, Gemini thinking tokens, and any OpenAI `total_tokens` excess above the reported parts. |
| `sablecourt_budget_cost_microusd_total` | counter | none | Cost in millionths of a US dollar. |
| `sablecourt_conclaves_total` | gauge | `state` | Conclave sessions per state: `active`, `input-required`, `completed`, `cancelled`. Only states that have sessions appear. |
| `sablecourt_conclave_ward_trips_recent` | gauge | none | Audit rows with action `conclave.ward` among the last 500 rows. |
| `sablecourt_model_fallbacks_recent` | gauge | none | Audit rows with action `model.fallback` among the last 500 rows. |
| `sablecourt_audit_events_recent` | gauge | `outcome` | The last 500 audit rows, counted by outcome. The set of outcomes is open. |

The five budget series are computed from the ledger totals on each scrape, so they can fall if ledger rows are removed. The `_recent` series look at a sliding window of audit rows, not at time.

## Input token counter migration

`sablecourt_budget_input_tokens_total` now counts uncached input tokens only. Cached prompt tokens are recorded in `sablecourt_budget_cache_write_tokens_total` and `sablecourt_budget_cache_read_tokens_total` instead. For example, an OpenAI response with 100 prompt tokens and 80 cached tokens adds 20 to `sablecourt_budget_input_tokens_total` and 80 to `sablecourt_budget_cache_read_tokens_total`, whereas earlier releases added 100 to `sablecourt_budget_input_tokens_total`.

The counter stays monotonic, so existing dashboards and alerts that treat `sablecourt_budget_input_tokens_total` as total input will see a lower rate on cached traffic. Query the total input token rate with one selector over the three counters. It also returns the earlier input series from before the upgrade, when the cache counters did not exist (adding three `sum(rate(...))` terms returns nothing while one of them is empty):

```promql
sum(rate({__name__=~"sablecourt_budget_(input|cache_write|cache_read)_tokens_total"}[5m]))
```

## In depth

- [Audit](../how-it-works/audit.md) for what the audit rows record, and [limits](limits.md) for the audit list sizes.
- [Model gate](../how-it-works/model-gate.md) for fallbacks and [conclaves](../how-it-works/conclaves.md) for Wards.
