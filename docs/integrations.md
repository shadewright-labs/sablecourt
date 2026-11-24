# Sablecourt integrations and extensions

This guide documents how Sablecourt connects to external MCP servers, Kubernetes Deployments, Shadewright incident workflows, Prometheus monitoring, and `Court Memory`.

## 1. Upstream MCP server bridge (`spec.grants.mcpServers`)

Sablecourt's Data and Action Gate (`internal/gate/mcp/upstream.go`) proxies remote MCP Streamable HTTP servers (such as Linear, Jira, Sentry, or read-only Postgres MCP) so that:

- Credentials remain inside `sablecourtd`'s `SecretStore` and never enter the workload container or process environment.
- Only tools listed in `allowedTools` are registered on the workload's MCP server under `<serverName>.<toolName>`.
- Every tool call passes through the deterministic Policy Engine (`mcp:<serverName>.<toolName>`), Approval Queue (when policy effect is `approve`), and linear-time secret redactor (`internal/redact`) before returning to the agent.

### Example `Workload` manifest

```yaml
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: issue-triage-agent
  zone: runtime
spec:
  class: long-lived-agent
  runtime:
    type: k8s-deployment
    image: ghcr.io/shadewright-labs/sablecourt-agent:v0.1.0
    readOnlyRootfs: true
  grants:
    modelGate:
      allowed: true
      providers: [openai, local]
      budgetRef: default-budget
    mcpTools:
      - linear.list_issues
      - memory.search
    mcpServers:
      - name: linear
        url: https://mcp.linear.app/mcp
        secretName: linear_api_key
        authHeader: Authorization
        authPrefix: "Bearer "
        allowedTools:
          - list_issues
```

## 2. Shadewright incident escalation API (Contract 3)

When `shadewright-analyzer` determines that a Kubernetes incident requires an application source-code change (outside its closed GitOps YAML remediation catalog), it escalates the incident to Sablecourt using a narrowly scoped `shadewright_bearer_token` (which is rejected on all non-integration routes).

| Endpoint | Direction | Action |
| :--- | :--- | :--- |
| `POST /api/v1/integrations/shadewright/incidents` | `shadewright-analyzer` -> `sablecourtd` | Opens a `Conclave`, posts an opening operator Decree, attaches the redacted incident evidence + logs as `incident-<id>.json` Relic, and logs `shadewright.incident_escalated` to the SHA-256 audit chain |
| `POST /api/v1/integrations/shadewright/verifications` | `shadewright-verifier` -> `sablecourtd` | Attaches the post-merge rollout `VerificationReport` (`RolledOut`, `Cleared`, or `Failed`) as `verification-<id>.json` Relic and posts a status update into the Conclave |

## 3. Kubernetes Deployment and Job execution (`internal/runtime/k8s`)

In `--mode=citadel`, `internal/runtime/k8s` supports two Kubernetes workload primitives while keeping identical Pod hardening (`AutomountServiceAccountToken: false`, `RunAsNonRoot: true`, `AllowPrivilegeEscalation: false`, `Drop: ["ALL"]`, `RuntimeDefault` seccomp, and optional `RuntimeClassName: gvisor`):

| `runtime.type` | Kubernetes object | Lifecycle mapping |
| :--- | :--- | :--- |
| `k8s` / `container` (default) | `batch/v1 Job` (`BackoffLimit: 0`, `TTLSecondsAfterFinished: 3600`) | Best for bounded tasks and one-shot runs; `Suspend`/`Resume` toggles `spec.suspend` |
| `k8s-deployment` / `deployment` | `apps/v1 Deployment` (`RestartPolicy: Always`) | Best for persistent `long-lived-agent` and `deterministic-controller` workloads; `Suspend`/`Resume` scales `spec.replicas` between `0` and `1`, and `Scry`/`Takeover` attach via `sablecourt.io/deployment=<name>` selector |

## 4. Operational Prometheus metrics (`GET /metrics`)

`internal/telemetry/metrics.go` exposes control-plane state in Prometheus `0.0.4` text format:

| Metric | Type | Description |
| :--- | :--- | :--- |
| `sablecourt_workloads_total{zone}` | gauge | Declared workloads by `runtime` and `sandbox` zone |
| `sablecourt_tasks_total{state}` | gauge | Tasks grouped by lifecycle state (`submitted`, `working`, `input-required`, `suspended`, `completed`, `failed`, `cancelled`) |
| `sablecourt_conclaves_total{state}` | gauge | Multi-agent `Conclave` rooms grouped by state (`active`, `input-required`, `closed`) |
| `sablecourt_approvals_pending` | gauge | Pending human out-of-band approvals |
| `sablecourt_budget_input_tokens_total` | counter | Aggregate observed input tokens across all providers |
| `sablecourt_budget_output_tokens_total` | counter | Aggregate observed output tokens across all providers |
| `sablecourt_budget_cost_microusd_total` | counter | Aggregate observed spend in micro-USD |
| `sablecourt_conclave_ward_trips_recent` | gauge | Conclave Ward breaker trips in the recent audit window |
| `sablecourt_model_fallbacks_recent` | gauge | Model Gate provider fallback transitions (`openai <-> local`) in the recent audit window |
| `sablecourt_audit_events_recent{outcome}` | gauge | Recent audit events by outcome (`allowed`, `denied`, `ok`, `error`) |

## 5. Court Memory UI, operator Relics, and task retry

| Feature | Endpoints and commands | Purpose |
| :--- | :--- | :--- |
| Court Memory Web UI | `GET /memory`, `POST /ui/memory/set`, `POST /ui/memory/delete` | Search persisted entries by scope and query using SQL `LIKE`/`ILIKE`, store new redacted knowledge, and remove obsolete entries. FTS5 search is parked under tracking [epic #1](https://github.com/shadewright-labs/sablecourt/issues/1). |
| Operator Relic upload | `POST /ui/conclaves/{id}/relics` | The route accepts a Relic, but the Conclave page has no upload form. Use `sablectl conclave relic` or the API. The UI form is parked under tracking [epic #1](https://github.com/shadewright-labs/sablecourt/issues/1). |
| Task retry | `POST /api/v1/tasks/{id}/retry`, `POST /ui/tasks/{id}/retry`, `sablectl task retry <id>` | Re-submit an interrupted or failed task with its original `workload` and `inputJSON` under a fresh idempotency key and log `task.retry` to the audit chain |
