# Sablecourt technical architecture

Sablecourt is a Go control plane (`sablecourtd`) and operator CLI (`sablectl`) that runs heterogeneous workloads (`coding-agent`, `long-lived-agent`, `deterministic-controller`) and multi-agent `Conclaves` behind deterministic capability gates, cost ceilings, and a SHA-256 audit chain whose verification has limits described below.

## System overview

[![Callers reach the sablecourtd HTTP server, which dispatches to the reconciler, scheduler and Conclave broker backed by the store and audit chain; the reconciler launches execution adapters with task leases, adapters call the in-process Model Gate and MCP Gate (with deterministic controllers refused at the Model Gate), and actions requiring approval pass to the Approval Queue.](diagrams/architecture-overview.svg)](diagrams/architecture-overview.svg "Open full size")

## Package map

| Package | Responsibility |
| :--- | :--- |
| `cmd/sablecourtd` | HTTP server, operating modes (`sanctum` / `citadel`), auth and CSRF middleware, GitOps manifest watcher, REST/SSE APIs, Prometheus `/metrics`, graceful shutdown |
| `cmd/sablectl` | Operator CLI (`init`, `doctor`, `task`, `conclave`, `approval`, `audit`, `memory`, `pair`, `device`, `voice`, `service`, `tailscale`, `mcp-server`) |
| `internal/config` | YAML manifest parser (`sablecourt.io/v1alpha1`), cross-reference and security zone validator, hot-reload `GitSyncer` |
| `internal/domain` | Core structs for `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule`, `Conclave`, `Task`, `Lease`, `Approval`, `AuditRecord`, `MemoryEntry` |
| `internal/store` | `Store` interface implemented by `internal/store/sqlite` (WAL, `LIKE` memory search) and `internal/store/postgres` (`pgx/v5`, `ILIKE` memory search) |
| `internal/secret` | `SecretStore` interface backed by macOS Keychain, encrypted local file, or Kubernetes `0400` volume mount |
| `internal/policy` | Deterministic `allow` / `approve` / `deny` evaluator (fail-closed default `deny`) and failure `CircuitBreaker` |
| `internal/redact` | Linear-time RE2 pattern redactor that masks some known secret shapes before storage or broadcast; best effort, defence in depth ([ADR-0015](adr/0015-redaction-is-defence-in-depth.md)) |
| `internal/gate/model` | HTTP/SSE proxy for OpenAI, Anthropic, Gemini, local endpoints, and upstream `agentgateway`; enforces `sc_task_` leases, budgets, SSRF protection, and `openai <-> local` fallback |
| `internal/gate/mcp` | MCP Streamable HTTP server for allowlisted built-in tools (`conclave.*`, `memory.*`, `sablecourt.*`), closed GitOps actions (`git.create_branch`, `git.push`, `git.open_pr`, `git.pr_checks`, `git.merge_pr`), and proxied upstream MCP servers |
| `internal/approval` | Pinned SHA-256 approval queue, macOS native `osascript` out-of-band prompt, and ECDSA P-256 device signature verification |
| `internal/conclave` | Multi-agent room broker enforcing `canMessage` graphs, one-time-on-open `always-on` and `on-mention` activation, `Wards` (`maxMessages`, `maxPingPong`), `spec.budgetRef`, operator `Decrees`, and shared `Relics` |
| `internal/attach` | 64 KiB redacted ring buffer (`Scry` SSE streaming) and exclusive operator `stdin` lock (`Takeover`) |
| `internal/runtime/*` | Execution adapters: `coding` (Git worktrees, harness container launch, and a macOS Seatbelt host path; no Bubblewrap exists, and host runs are off by default and unsupported for 0.1.0 per the [ADR-0011 amendment](adr/0011-sanctum-scope-trust-and-cli-harnesses.md#amendment-2026-11-27-container-only-harnesses-for-010)), `docker` (Docker/Podman/OrbStack/Colima/Rancher), `k8s` (hardened Jobs and Deployments), `controller` (zero-LLM) |
| `internal/notify` | Outbound alerts to Slack, Discord, ntfy, or generic webhooks on approvals, Ward pauses, and task failures |
| `internal/voice` | Local-first OpenAI-compatible Whisper STT and Kokoro/Piper/macOS `say` TTS for voice Decrees and Conclave readback |
| `internal/ui` | Server-rendered HTML/HTMX cockpit (`/` dashboard, `/tasks/{id}`, `/conclaves`, `/conclaves/{id}`, `/schedules`, `/audit`, `/memory`, `/providers`, `/studio`) |
| `internal/telemetry` | Prometheus metrics registry (`/metrics`) and structured logging |

## Operating modes: Sanctum and Citadel

| Aspect | Sanctum (`--mode=sanctum`, default) | Citadel (`--mode=citadel`) |
| :--- | :--- | :--- |
| Target host | Personal macOS or Ubuntu workstation | Hardened Kubernetes cluster (`deploy/k8s/overlays/citadel/`) |
| Default bind address | `127.0.0.1:7432` | `0.0.0.0:7432` behind TLS ingress or Tailscale |
| Operator auth | Mandatory `op_<64 hex>` bearer (`0600` `operator.token` in `0700` state directory) or 30-minute `SameSite=Strict` `sablecourt_session` cookie from `POST /login`; same-origin CSRF enforced on writes; `sablectl` takes the token from `SABLECOURT_OPERATOR_TOKEN` or the token file, never a flag | Same mandatory `op_<64 hex>` bearer or `sablecourt_session` cookie (`--operator-auth-required` is deprecated and ignored) |
| Database | Embedded SQLite WAL (`~/.sablecourt/sablecourt.db`) | PostgreSQL (`--postgres-dsn`) |
| Container / sandbox | Auto-discovered Podman, Docker, OrbStack, Colima, Rancher Desktop, plus macOS Seatbelt for the unsupported host path | Kubernetes `batch/v1 Job` and `apps/v1 Deployment` with `RuntimeClassName: gvisor`, `NetworkPolicy`, and `AutomountServiceAccountToken: false` |
| Upstream AI routing | Direct provider keys in Keychain / `0400` file, or local Ollama/vLLM | Optional `--model-gateway-url` fronting Linux Foundation `agentgateway` |

## Request and execution lifecycle

| Stage | Behavior |
| :--- | :--- |
| 1. Manifest load | `sablecourtd` loads local YAML files or syncs a GitOps repository (`--manifests-git-repo`). `validateBundle` rejects any `sandbox`-zone workload that requests mutating GitOps actions, runtime-zone Conclave messaging, or external Agent Card advertisement. |
| 2. Task creation | An operator (UI, CLI, webhook, Schedule, A2A, or Shadewright Escalation API) submits a task with an idempotency key. `sablecourtd` persists the task in `submitted` state. |
| 3. Lease minting | The control plane reconciler mints a scoped task bearer token (`sc_task_<64 hex>`) and stores only its SHA-256 hash in `leases`. Raw provider keys never enter the workload environment. |
| 4. Isolated launch | The matching runtime adapter starts the workload. Local `coding-agent` runs write a read-only `.sablecourt-session.json` marker so Nightloom recognizes the session as Sablecourt-managed. |
| 5. Capability mediation | Model requests hit `/gate/model/{provider}/...` with `Authorization: Bearer sc_task_...` (checked against leases, grants, and budgets). Tool and action requests hit `/gate/mcp` or `/gate/action/{name}` and pass through deterministic policy (`allow`, `approve`, `deny`) and the Approval Queue. |
| 6. Observation and steering | Operators watch live redacted output via `Scry`, claim exclusive `stdin` via `Takeover`, or steer multi-agent rooms via `Decree` (text or voice). |
| 7. Tamper-evident audit | Every state transition, policy decision, approval, and model call is appended to `audit_log` chained with `SHA-256(prev_hash \|\| record \|\| created_at)`. `sablectl audit verify` recomputes the chain from `genesis`. |


## Agentic context, learnings, and verification pipeline (ADR-0010)

Before dispatching a non-deterministic task, `Reconciler.launchTask` runs `internal/contextpack.Assemble` to build a redacted, 32 KiB-capped context packet for the task input. Learnings enter the packet only when its model destination is local (see [context and learning](how-it-works/context-and-learning.md#destination-check)). The packet combines:

1. Task intent (`prompt`, `input`, workflow step metadata, and any failure handoff).
2. For a local destination only, up to ten accepted, unexpired learnings from the ADR-0012 learning record, in a section of at most 4 KiB. Legacy `learning:*` rows in Court Memory are not read.
3. Recent Conclave Decrees and Relics when the task runs inside a Conclave.
4. A read-only repository pre-check (10-second timeout, 16 KiB cap) running `git status --short`, `git diff --stat`, `go vet ./...` for Go modules, and `sg scan` when `sgconfig.yml` is present.

## Audit verification limits

The SQLite and PostgreSQL verifiers recalculate hashes and previous-row links for rows that have hashes. They skip legacy rows with an empty hash and verify only rows still present in the database. A deleted middle row breaks a later row's previous-hash link; deletion of the final row leaves no later link to check. External anchoring for detecting tail deletion is parked under tracking [epic #1](https://github.com/shadewright-labs/sablecourt/issues/1).

### Dated amendment, 2026-11-24

The description above corrects the implementation claim in ADR-0009. That accepted ADR was outside this documentation work package and remains unchanged; this amendment records the limit without changing the decision text.

Workflows can pair an authoring step with an independent `role: verifier` step running on a separate model or CLI harness. The reconciler wraps the verifier prompt with author artifacts and expects `VERDICT: PASS` or `VERDICT: CHANGES_REQUESTED`. On verifier rejection or runtime failure, opt-in remediation (`maxRetries` on `WorkflowStep`, or manual `Fix from failure` / `sablectl task retry --handoff`) packages redacted artifacts and events into `failure-handoff.md` for the next attempt. The manifest loader refuses a verifier step whose workload holds any grant other than the read-only `mcpTools` `memory.get`, `memory.search`, `conclave.read`, `conclave.relic_get` and `sablecourt.list_workloads` (so no `actions`, `delegateTo`, `mcpServers`, `repos` or `modelGate`), or is also used by a non-verifier step before it, and a verifier step on a model workload with no earlier step that runs a model.

Role-based model tiering (`fast`, `coding`, `critic`) is stored in `system:model-tiers` and editable in `/providers` or via `/api/v1/providers/tiers`. Workflow steps declaring `tier` without explicit `provider`/`model` fields resolve from this table at dispatch time.
