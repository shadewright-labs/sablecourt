# Sablecourt v0.1 design

Status: draft for v0.1 implementation (23 November 2026). This document turns the accepted architecture decisions in [ADR-0001](adr/0001-local-first-control-plane-with-k8s-adapters.md) through [ADR-0007](adr/0007-strict-boundary-with-nightloom.md) into concrete interfaces, schemas, upstream library choices, and package boundaries.

## Binaries and deployment shape

Sablecourt v0.1 ships as two Go binaries built from one module:

- `sablecourtd`: local-first control plane daemon. It runs the reconciler loop, owns the SQLite (WAL) store, hosts the Model Gate and Data/Action Gate, manages the runtime approval queue, and serves the HTTP API and embedded Web UI on `127.0.0.1` plus a Unix domain socket for local CLI calls.
- `sablectl`: command-line client for submitting tasks, inspecting workloads and audit records, reloading Git-tracked manifests, and driving the approval queue.

The daemon never executes untrusted workload code inside its own process. Every infrastructure dependency (store, container runtime, secret store, out-of-band approver, and telemetry exporter) sits behind a Go interface so a future Kubernetes deployment can swap in Postgres/CRDs, `client-go`, or `kubernetes-sigs/agent-sandbox` without rewriting domain logic.

## Upstream Go library stack

Sablecourt avoids reimplementing standard protocols or low-level drivers in v0.1. Each upstream dependency is isolated behind an internal adapter package:

| Concern | Upstream library | Reason |
| --- | --- | --- |
| Manifest parsing | `sigs.k8s.io/yaml` | Parses Kubernetes-style `apiVersion`/`kind`/`metadata`/`spec` YAML cleanly and maps directly to future CRDs. |
| Embedded SQLite | `modernc.org/sqlite` | Pure Go SQLite driver with WAL support and zero CGO, keeping `go test -race ./...` and cross-compilation simple. |
| MCP server and client | `github.com/modelcontextprotocol/go-sdk` | Official Go SDK with Streamable HTTP transport and typed tool schemas for the Data/Action Gate. |
| A2A task envelopes | `github.com/a2aproject/a2a-go` | Official Go types for Agent Cards, stateful Task envelopes, and typed Artifacts. |
| Container runtime | `github.com/moby/moby/client` | Moby Go SDK; one Docker Engine API client works across Docker Desktop, OrbStack, Colima, Podman sockets, and Linux Docker. |
| OS credential storage | `github.com/zalando/go-keyring` | Reads raw upstream keys from macOS Keychain or Linux Secret Service, paired with a `0600` file fallback for headless Linux and CI. |
| Observability | `go.opentelemetry.io/otel` + `log/slog` | Standard traces and metrics alongside structured JSON logs. |
| CLI framework | `github.com/spf13/cobra` | Subcommand routing, flag parsing, and shell completions for `sablectl` and `sablecourtd`. |
| Web UI | Go stdlib `html/template` + `embed.FS` | Server-rendered operational dashboard embedded in `sablecourtd` with no Node toolchain. |

## Git-tracked manifest schema

Declared intent lives in Git-tracked YAML files under `apiVersion: sablecourt.io/v1alpha1`. `sablecourtd` loads manifests read-only and rejects unknown fields (`DisallowUnknownFields`). Three resource kinds cover v0.1:

### Workload

```yaml
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: repo-triage-agent
  zone: runtime # runtime | sandbox
spec:
  class: long-lived-agent # coding-agent | long-lived-agent | deterministic-controller
  runtime:
    type: docker # docker | worktree-cli
    image: ghcr.io/shadewright-labs/triage-agent:0.1.0
    readOnlyRootfs: true
  coding: # used only when class == coding-agent
    harness: claude # claude | codex
    repoPath: /path/to/repo
    sandbox: seatbelt # seatbelt | bubblewrap | docker
  agentCard:
    description: Triages incoming repository issues and proposes bounded fixes.
    skills:
      - issue-triage
  grants:
    modelGate:
      allowed: true
      providers: [anthropic, openai, gemini]
      budgetRef: triage-daily
    mcpTools:
      - github.get_issue
      - github.list_files
    actions:
      - git.open_pr
    delegateTo:
      - go-coder
```

Deterministic controllers set `grants.modelGate.allowed: false`. Any attempt by a deterministic controller to call Model Gate is denied at authentication time and logged to the audit store.

Workloads with `metadata.zone: sandbox` are forbidden from holding production `actions` or `delegateTo` grants. Moving a workload from `sandbox` to `runtime` happens only by editing its manifest on a branch, passing CI checks, and merging via pull request.

### Policy

```yaml
apiVersion: sablecourt.io/v1alpha1
kind: Policy
metadata:
  name: default-court-policy
spec:
  defaultEffect: deny
  rules:
    - workload: repo-triage-agent
      capability: mcp:github.get_issue
      effect: allow
    - workload: repo-triage-agent
      capability: action:git.open_pr
      effect: approve
    - workload: "*"
      capability: policy:*
      effect: deny
```

Policy evaluation is pure, deterministic Go code with no LLM involvement. Outcomes are strictly `allow`, `approve`, or `deny`. Any evaluation error, unrecognized capability, stale heartbeat, or tripped circuit breaker fails closed to `deny`.

### Budget

```yaml
apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata:
  name: triage-daily
spec:
  perTask:
    maxInputTokens: 200000
    maxOutputTokens: 40000
    maxCostUSD: 2.50
  daily:
    maxCostUSD: 15.00
```

## Control plane state machine and hub-and-spoke dispatch

The control plane runs a single reconciler loop over durable facts:

1. OBSERVE: read live facts from runtime adapters (container state, exit code, detached coding session status, heartbeat timestamps) and pending approvals.
2. UPDATE: persist observed facts to SQLite with monotonic timestamps. If a heartbeat exceeds its staleness window, mark the task observation as stale.
3. DERIVE / ACT: compute the next legal transition from stored facts and policy, then issue idempotent adapter calls (`Start`, `Suspend`, `Resume`, `Stop`, `CollectArtifacts`).

Task states match the A2A lifecycle from `a2a-go`: `submitted`, `working`, `input-required`, `suspended`, `completed`, `failed`, and `cancelled`.

Workloads have no direct network route to one another. When workload A needs help from workload B, A submits a child task proposal to the control plane. The control plane checks A's `delegateTo` grant, carves a sub-budget from A's remaining ceiling, creates a child task row linked by `parent_task_id` and a deterministic idempotency key, and dispatches B through the same loop.

## SQLite schema (WAL mode)

`sablecourtd` opens SQLite with `PRAGMA journal_mode=WAL`, `PRAGMA foreign_keys=ON`, and `PRAGMA busy_timeout=5000`. All persistence sits behind `internal/store.Store`.

Core tables in v0.1:

- `workloads`: last loaded manifest digest, zone, class, and `AgentCard` JSON.
- `tasks`: `id`, `parent_task_id`, `idempotency_key` (unique), `workload_name`, `state`, `input_json`, `artifacts_json`, `budget_ceiling_json`, `last_heartbeat_at`, `created_at`, `updated_at`.
- `task_events`: append-only state transitions and observed runtime facts.
- `leases`: active short-lived gate tokens (stored as SHA-256 hash, never plaintext), bound to `(task_id, workload_name, expires_at, revoked_at)`.
- `budget_ledger`: immutable usage rows (`task_id`, `workload_name`, `provider`, `model`, `input_tokens`, `output_tokens`, `cost_microusd`, `created_at`) aggregated inside transactions before granting new requests.
- `approvals`: `id`, `task_id`, `workload_name`, `capability`, `payload_canonical_json`, `payload_sha256`, `status` (`pending`, `approved`, `denied`, `expired`), `decided_by`, `proof_json`, `expires_at`.
- `checkpoints`: opaque JSON or blob references persisted by long-lived AI agents before suspend and restored on resume.
- `audit_log`: append-only structured records covering policy evaluations, gate denials, budget trips, approvals, and redaction hits.

## Three Doors capability gates

Workloads never receive upstream API keys, database passwords, or Git tokens in environment variables or mounted files. At task start, `sablecourtd` mints a short-lived scoped bearer token (`sc_task_...`) bound to the task and workload identity, and injects only the gate base URLs and that task token.

### Model Gate (`internal/gate/model`)

Model Gate is a single HTTP/SSE reverse proxy listening on the internal container bridge and localhost. In v0.1 it supports three wire formats so existing SDKs and agents can point their base URL at Sablecourt with zero custom client code:

1. OpenAI-compatible: `/openai/v1/chat/completions` and `/openai/v1/responses` (backs OpenAI cloud as well as local Ollama and LM Studio endpoints).
2. Anthropic: `/anthropic/v1/messages`.
3. Google Gemini: `/gemini/v1beta/models/*:generateContent` and `:streamGenerateContent`.

For every request, Model Gate executes a strict pipeline:

1. Authenticate the `sc_task_...` bearer token against `leases` and verify `grants.modelGate.allowed == true` and provider allowlist membership.
2. Run a pre-flight budget check against `budget_ledger` for both task and daily caps; reject with `429` if exhausted.
3. Strip the task token, attach the real upstream credential fetched from `SecretStore`, and forward the request.
4. Stream SSE chunks through without buffering the whole response while parsing provider usage metadata from the final usage events (or non-streaming JSON body).
5. Record token counts and computed micro-USD cost atomically into `budget_ledger` and emit an OTel span plus an `audit_log` entry.

### Data and action gate (`internal/gate/mcp`)

Data and Action Gate combines two surfaces behind one policy and redaction boundary:

1. An MCP Streamable HTTP endpoint built on `github.com/modelcontextprotocol/go-sdk`. On `tools/list`, the gate filters the catalog to only the tools in the caller's `grants.mcpTools`. On `tools/call`, it evaluates `Policy`, executes allowed read tools, or pauses the task on `approve`.
2. A closed typed action catalog for mutating operations (starting with GitOps actions: `git.create_branch`, `git.open_pr`, and `sandbox.propose_promotion`). Every mutating action validates strict typed JSON inputs, checks policy and circuit breakers, and runs with the gate's scoped credentials rather than workload credentials.

Every tool response and external payload passes through `internal/redact` (a linear-time deterministic redactor modeled on Shadewright's `internal/redact`, avoiding backtracking regexes) before returning text to a workload.

## Runtime adapters and isolation

All three workload classes from ADR-0002 implement one `internal/runtime.Adapter` interface:

```go
type Adapter interface {
    Start(ctx context.Context, spec LaunchSpec) (Handle, error)
    Observe(ctx context.Context, handle Handle) (Observation, error)
    Suspend(ctx context.Context, handle Handle) (CheckpointRef, error)
    Resume(ctx context.Context, handle Handle, cp CheckpointRef) error
    Stop(ctx context.Context, handle Handle) error
    CollectArtifacts(ctx context.Context, handle Handle) ([]Artifact, error)
}
```

### Container runtime (`internal/runtime/docker`)

Used for long-lived AI agents, sandbox workloads, and deterministic controllers. Built on `github.com/docker/docker/client`:

- Creates a dedicated Docker bridge network with `Internal: true` (no internet egress).
- Today this runtime still gives workloads a direct HTTP gate URL. The per-launch council proxy (a fixed gate route allow-list over a per-launch Unix socket or pinned TLS, with no route from the internal network to the host) is built separately in `internal/gate/councilproxy` and is not yet wired into this runtime or the daemon launcher (issue 78). See the proxy to daemon hop in [ADR-0011](adr/0011-sanctum-scope-trust-and-cli-harnesses.md).
- Starts containers with `--cap-drop=ALL`, `no-new-privileges`, read-only root filesystem where supported by the workload image, and a single bind mount scoped to the task's working directory.
- Long-lived agents persist state via a mounted checkpoint directory or control-plane checkpoint endpoint before `Suspend`.

### Coding agent runtime (`internal/runtime/coding`)

Used for local `claude` (Claude Code) and `codex` (Codex) runs:

- Creates a dedicated Git worktree per task on a deterministic branch name (`sablecourt/<task-id>`).
- Launches the CLI inside a detached session host so restarting `sablecourtd` does not terminate active coding runs.
- Wraps execution in an OS sandbox (macOS Seatbelt profile restricting writes outside the worktree and temp dir, or Linux Bubblewrap) with a stripped environment allowlist.
- Sets a read-only starter marker (`SABLECOURT_MANAGED=1`, `SABLECOURT_TASK_ID=<id>`) so Nightloom can tag Sablecourt sessions cleanly without coupling the two codebases (ADR-0007).

### Deterministic controller runtime (`internal/runtime/controller`)

Runs non-LLM binaries or scripts inside the container runtime (or bounded local exec in tests) with `modelGate.allowed: false`, zero Model Gate token, and access only to explicitly granted read tools or typed actions.

## Dual-path human-in-the-loop approvals

1. GitOps PR path: code edits from coding agents, policy changes, and `sandbox` to `runtime` promotions go exclusively through branches, pull requests, required CI checks, and human merge.
2. Runtime approval queue (`internal/approval`): when policy returns `approve`, the gate canonicalizes the action payload, computes `payload_sha256`, writes a `pending` row to `approvals`, and transitions the task to `input-required`.
   - Approval requires out-of-band confirmation behind an `Approver` interface. In v0.1 on macOS, this uses a native OS confirmation dialog bridged over parent-child pipes or an OS Keychain-backed P-256 signature (triggering a Keychain/Touch ID prompt) over `payload_sha256`, so an arbitrary local process cannot self-approve by sending an HTTP POST to `127.0.0.1`.
   - Denial requires only a valid operator session token because denying cannot widen permissions.
   - Timeouts, closed pipes, or digest mismatches fail closed.

## Operator interface

- `sablectl`: talks to `sablecourtd` over Unix socket (`~/.local/state/sablecourt/sablecourtd.sock`) or `127.0.0.1`. Core commands: `apply` / `validate`, `task submit|list|watch|cancel`, `workload list|inspect`, `approval list|approve|deny`, `budget status`, `audit tail`.
- Embedded Web UI (`internal/ui`): Go `html/template` pages served by `sablecourtd` on `127.0.0.1` using the Sablecourt indigo-jade-bone palette (`#08060E`, `#111422`, `#4C1D95`, `#EFE9DC`, `#2DD4BF`, amber `#F0B86A` for waiting approvals). Displays derived status with observation timestamps and staleness badges. Web UI buttons can trigger denials directly and initiate out-of-band approval prompts, never bypassing `Approver`.

## Proposed Go package layout

```text
sablecourt/
  cmd/
    sablecourtd/        # daemon entrypoint
    sablectl/           # CLI entrypoint
  docs/
    design.md           # this document
    adr/                # accepted ADRs 0001-0007
  internal/
    approval/           # Approver interface, digest pinning, macOS OOB bridge
    config/             # sigs.k8s.io/yaml loader and strict schema validation
    controlplane/       # OBSERVE -> UPDATE -> DERIVE/ACT loop and hub-and-spoke dispatcher
    domain/             # core types, A2A envelope helpers, budget and policy models
    gate/
      mcp/              # Data/Action Gate (modelcontextprotocol/go-sdk + typed actions)
      model/            # Model Gate (OpenAI, Anthropic, Gemini HTTP/SSE proxy)
    policy/             # deterministic allow/approve/deny engine and circuit breakers
    redact/             # linear-time deterministic boundary redactor
    runtime/
      coding/           # Git worktree + OS sandbox + detached host for claude and codex
      controller/       # zero-LLM deterministic controller runner
      docker/           # Moby Go SDK container runtime on internal gate-only network
    secret/             # SecretStore interface (zalando/go-keyring + 0600 file fallback)
    store/
      sqlite/           # modernc.org/sqlite WAL implementation + migrations
    telemetry/          # OpenTelemetry + slog setup
    ui/                 # HTTP API + embedded html/template Web UI
```

## v0.1 vertical slice milestones

1. M1 (Core scaffold and store): `go.mod`, manifest loader (`sigs.k8s.io/yaml`), SQLite WAL store + migrations, `sablecourtd` and `sablectl` skeleton, `go test -race ./...` green.
2. M2 (Model Gate and budgets): OpenAI, Anthropic, and Gemini HTTP/SSE proxy, task token leases, atomic `budget_ledger` enforcement, deterministic controller zero-LLM denial tests.
3. M3 (Data/Action Gate and approvals): MCP Streamable HTTP server (`modelcontextprotocol/go-sdk`), linear-time redactor, typed GitOps PR action, runtime approval queue with pinned SHA-256 digest and macOS out-of-band `Approver`.
4. M4 (Three workload adapters and Web UI): Docker internal-network adapter (long-lived agent + deterministic controller), Git worktree + OS sandbox coding adapter (`claude` and `codex`), hub-and-spoke child delegation, and embedded `html/template` Web UI.
