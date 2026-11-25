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
| `Conclave` | Multi-agent room with governance breakers | `spec.vassals[]` (`role`, `workload`, `activation`: `always-on` or `on-mention`, `canMessage[]`), `spec.wards` (`maxMessages`, `maxPingPong`), `spec.budgetRef` |

## HTTP and SSE API endpoints (`sablecourtd`)

All mutating operator endpoints enforce same-origin CSRF checks (`Sec-Fetch-Site` and `Origin`), security headers (`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `CSP`), and a `2 MiB` body size limit. In both `--mode=sanctum` and `--mode=citadel`, operator routes always require `Authorization: Bearer <op_...>` (read from the `0600` `operator.token` file in the `0700` state directory) or a 30-minute `SameSite=Strict` `sablecourt_session` cookie issued by `POST /login`; `--operator-auth-required` is deprecated and ignored. `sablectl` reads the credential from `SABLECOURT_OPERATOR_TOKEN` or, for a local daemon, from `operator.token`; there is no `--token` flag, so the secret never appears in the process table. `POST /logout` clears the session cookie and revokes every session issued before it.

### Operator API

| Method and path | Scope | Purpose |
| :--- | :--- | :--- |
| `GET /healthz`, `GET /login`, `POST /login` | Public | Liveness check and form sign-in exchanging `operator.token` for `sablecourt_session` |
| `GET /metrics` | Operator | Prometheus operational metrics |
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
| `GET /api/v1/memory`, `POST /api/v1/memory`, `DELETE /api/v1/memory/{id}` | Operator | List, search with SQL `LIKE`/`ILIKE`, set, or delete `Court Memory` entries. FTS5 search is parked under tracking [epic #1](https://github.com/shadewright-labs/sablecourt/issues/1). |
| `POST /api/v1/schedules/{name}/trigger` | Operator | Manually trigger a declared `Schedule` immediately |
| `POST /api/v1/devices/pair`, `GET /api/v1/devices`, `DELETE /api/v1/devices/{id}` | Operator | Pair, list, or revoke ECDSA P-256 approval devices |

### Scoped integration and capability gate APIs

| Method and path | Auth token | Purpose |
| :--- | :--- | :--- |
| `GET /api/v1/observer/snapshot`, `GET /api/v1/observer/stream` | `observer_bearer_token` (read-only) | Nightloom Watcher bridge (active tasks, Conclaves, pending approvals, budget totals) |
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


## Context, learnings, and model tier endpoints

| Endpoint / Flag | Purpose |
| --- | --- |
| `GET /api/v1/learnings?repo=<name>&path=<paths>` | Return matching global, repo, and path-glob Scoped Learnings. |
| `POST /api/v1/learnings` | Save a redacted Scoped Learning (`scope`, `key`, `rule`, `author`). |
| `GET /api/v1/providers/tiers` | Read configured `fast`, `coding`, and `critic` model tier routes. |
| `POST /api/v1/providers/tiers` | Persist role-based model tier routes (`[{"tier":"critic","provider":"cli:claude","model":"claude-opus-5-5"}]`). |
| `POST /api/v1/tasks/{id}/retry?handoff=true` | Retry a task with a standardized `failure-handoff.md` built from artifacts and events. |
| `sablectl task retry <id> --handoff` | CLI shortcut for closed-loop failure handoff retry. |

`WorkflowStep` fields added in ADR-0010: `role` (`author` | `verifier` | `fast`), `tier` (`fast` | `coding` | `critic`), and `maxRetries` (opt-in closed-loop remediation limit).
