# Sablecourt reference: manifests, APIs, and CLI

This document describes the YAML manifest schema (`sablecourt.io/v1alpha1`), HTTP/SSE/MCP endpoints exposed by `sablecourtd`, and commands in `sablectl`.

## Manifest kinds (`sablecourt.io/v1alpha1`)

| Kind | Purpose | Key fields |
| :--- | :--- | :--- |
| `Workload` | Declares an agent, coding harness, or deterministic controller | `metadata.zone` (`runtime` or `sandbox`), `spec.class` (`coding-agent`, `long-lived-agent`, `deterministic-controller`), `spec.runtime`, `spec.grants` (`modelGate`, `mcpTools`, `mcpServers`, `actions`) |
| `Policy` | Deterministic capability rules | `spec.defaultEffect` (always `deny`), `spec.rules[]` (`workload`, `capability`, `effect`: `allow` / `approve` / `deny`) |
| `Budget` | Token and USD spend ceilings | `spec.perTask` (`maxInputTokens`, `maxOutputTokens`, `maxCostUSD`), `spec.daily.maxCostUSD` |
| `Workflow` | Multi-step task DAG | `spec.steps[]` referencing declared `Workload` names |
| `Schedule` | Cron-driven recurring task trigger | `spec.cron`, `spec.workload`, `spec.input`, `spec.enabled` |
| `Conclave` | Multi-agent room with governance breakers | `spec.vassals[]` (`role`, `workload`, `activation`: `always-on` or `on-mention`, `canMessage[]`), `spec.wards` (`maxMessages`, `maxPingPong`, `budgetRef`) |

## HTTP and SSE API endpoints (`sablecourtd`)

All mutating operator endpoints enforce same-origin CSRF checks (`Sec-Fetch-Site` and `Origin`), security headers (`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `CSP`), and a `2 MiB` body size limit. In `--mode=citadel` (or `--operator-auth-required`), operator routes require `Authorization: Bearer <operator_bearer_token>`.

### Operator API

| Method and path | Scope | Purpose |
| :--- | :--- | :--- |
| `GET /healthz`, `GET /metrics` | Public / Scrape | Liveness check and Prometheus operational metrics |
| `GET /api/v1/workloads` | Operator | List declared workloads and zones |
| `GET /api/v1/tasks`, `POST /api/v1/tasks` | Operator | List tasks or submit a new task |
| `GET /api/v1/tasks/{id}`, `POST /api/v1/tasks/{id}/cancel`, `POST /api/v1/tasks/{id}/retry` | Operator | Inspect, cancel, or retry a task with its original input |
| `GET /api/v1/tasks/{id}/scry` | Operator or Observer | Live SSE stream of redacted stdout/stderr (64 KiB ring buffer) |
| `POST /api/v1/tasks/{id}/takeover`, `/stdin`, `/detach` | Operator | Claim exclusive `stdin`, send input, and release lock |
| `GET /api/v1/conclaves`, `POST /api/v1/conclaves` | Operator | List or open a `Conclave` session |
| `GET /api/v1/conclaves/{id}`, `GET /api/v1/conclaves/{id}/stream` | Operator | Inspect Conclave transcript/Relics or stream live SSE updates |
| `POST /api/v1/conclaves/{id}/decree`, `/voice-decree`, `/relics`, `/close` | Operator | Inject text or voice Decree, attach operator Relic, or close Conclave |
| `GET /api/v1/approvals`, `POST /api/v1/approvals/{id}/approve`, `/deny` | Operator or Paired Device | List pending approvals and approve (including ECDSA P-256 signature) or deny |
| `GET /api/v1/audit`, `GET /api/v1/audit/verify` | Operator | Query audit log and verify SHA-256 hash chain integrity |
| `GET /api/v1/memory`, `POST /api/v1/memory`, `DELETE /api/v1/memory/{id}` | Operator | List, search (FTS5), set, or delete `Court Memory` entries |
| `POST /api/v1/schedules/{name}/trigger` | Operator | Manually trigger a declared `Schedule` immediately |
| `POST /api/v1/devices/pair`, `GET /api/v1/devices`, `DELETE /api/v1/devices/{id}` | Operator | Pair, list, or revoke ECDSA P-256 approval devices |

### Scoped integration and capability gate APIs

| Method and path | Auth token | Purpose |
| :--- | :--- | :--- |
| `GET /api/v1/observer/snapshot`, `GET /api/v1/observer/stream` | `observer_bearer_token` (read-only) | Nightloom Watchman bridge (active tasks, Conclaves, pending approvals, budget totals) |
| `POST /api/v1/integrations/shadewright/incidents` | `shadewright_bearer_token` | Contract 3: escalate a Kubernetes code-level `Incident` to a Conclave and attach evidence as a `Relic` |
| `POST /api/v1/integrations/shadewright/verifications` | `shadewright_bearer_token` | Contract 3: post a `VerificationReport` (`RolledOut` / `Cleared`) back into the Conclave as a `Relic` |
| `POST /gate/model/{provider}/*` | `sc_task_...` lease | Model Gate proxy (`openai`, `anthropic`, `gemini`, `local`) with budget accounting and fallback |
| `POST /gate/mcp`, `POST /gate/action/{name}` | `sc_task_...` lease | Data & Action Gate (MCP Streamable HTTP + closed GitOps actions + upstream MCP server bridge) |
| `POST /webhooks/github`, `POST /a2a`, `GET /.well-known/agent-card.json` | HMAC-SHA256 / A2A | GitHub webhook ingress and runtime-zone A2A JSON-RPC broker |

## CLI command reference (`sablectl`)

| Command group | Subcommands |
| :--- | :--- |
| Setup and health | `sablectl init`, `sablectl doctor`, `sablectl validate <path>`, `sablectl service install`, `sablectl tailscale expose\|status` |
| Workloads and tasks | `sablectl workload list`, `sablectl task submit\|list\|inspect\|diff\|scry\|takeover\|retry\|cancel` |
| Multi-agent Conclaves | `sablectl conclave open\|list\|inspect\|watch\|decree\|relic\|close` |
| Governance and audit | `sablectl approval list\|approve\|deny`, `sablectl audit verify\|export`, `sablectl pair`, `sablectl device list\|revoke` |
| Memory, voice, and MCP | `sablectl memory list\|set\|delete`, `sablectl voice status\|speak`, `sablectl mcp-server` (stdio bridge for Claude/Codex/Cursor) |
