# Sablecourt v0.1 design

This document lists the v0.1 interfaces, schemas, upstream libraries and package boundaries as the code has them. The architecture decisions behind them are the ADRs in [adr/](adr/README.md).

## Binaries and deployment shape

Sablecourt v0.1 builds three Go binaries from one module:

- `sablecourtd`: the control plane daemon. It runs the reconciler loop, hosts the Model Gate and Data/Action Gate, manages the approval queue, and serves the HTTP API and embedded Web UI. It stores state in SQLite (WAL mode), or in PostgreSQL in either mode when `--postgres-dsn` or `SABLECOURT_POSTGRES_DSN` is set. It listens on `127.0.0.1:7432` by default (`--listen`), and on a Unix domain socket only when `--socket` is given.
- `sablectl`: command-line client for submitting tasks, inspecting workloads and audit records, reloading Git-tracked manifests, and driving the approval queue.
- `sablecourt-agent`: a small agent program that runs in a container image (`make agent-image`).

Workload code does not run inside the daemon process: a coding agent is a child process of the daemon, and a container workload runs in the container engine. The store (`internal/store.Store`), the runtime (`internal/runtime.Adapter`), the secret store (`internal/secret.SecretStore`) and the out-of-band approver (`internal/approval.Approver`) are Go interfaces. SQLite and PostgreSQL implement the store, and the Docker, Kubernetes, coding and controller packages implement the runtime.

## Upstream Go library stack

Sablecourt avoids reimplementing standard protocols or low-level drivers in v0.1. Each upstream dependency is isolated behind an internal adapter package:

| Concern | Upstream library | Reason |
| --- | --- | --- |
| Manifest parsing | `sigs.k8s.io/yaml` | Parses Kubernetes-style `apiVersion`/`kind`/`metadata`/`spec` YAML cleanly and maps directly to future CRDs. |
| Embedded SQLite | `modernc.org/sqlite` | Pure Go SQLite driver with WAL support and zero CGO, keeping `go test -race ./...` and cross-compilation simple. |
| MCP server and client | `github.com/modelcontextprotocol/go-sdk` | Official Go SDK with Streamable HTTP transport and typed tool schemas for the Data/Action Gate. |
| A2A | none (`internal/a2a`) | `github.com/a2aproject/a2a-go` is not a dependency. `internal/a2a` is a hand-written JSON-RPC handler. |
| Container runtime | `github.com/moby/moby/client` | Moby Go SDK. The adapter looks for the Docker, Podman, OrbStack, Colima and Rancher Desktop sockets. |
| OS credential storage | `github.com/zalando/go-keyring` | Reads upstream keys from the OS keychain. The daemon adds a `0600` file store when `--secret-file` is set, and reads the keychain first. |
| Observability | `log/slog` and `go.opentelemetry.io/otel` | Structured JSON logs with `log/slog`. `internal/telemetry` defines an OpenTelemetry tracer, which no code calls today. `GET /metrics` serves Prometheus-format text. |
| Kubernetes runtime | `k8s.io/client-go` | The Citadel-mode adapter in `internal/runtime/k8s`. |
| CLI framework | `github.com/spf13/cobra` | Subcommands and flags for `sablectl` and `sablecourtd`. |
| Web UI | Go stdlib `html/template` + `embed.FS` | Server-rendered operational dashboard embedded in `sablecourtd` with no Node toolchain. |

## Git-tracked manifest schema

Declared intent lives in YAML files under `apiVersion: sablecourt.io/v1alpha1`. `sablecourtd` reads them, decodes each document with `yaml.UnmarshalStrict` so an unknown field is an error, and does not write them. There are six kinds: `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule` and `Conclave`. The first three are shown here, and all six are in the [manifest reference](reference/manifests.md).

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
    image: registry.example/triage-agent:0.1.0
    readOnlyRootfs: true
  coding: # used only when class == coding-agent
    harness: claude # claude | codex
    repoPath: /path/to/repo
    sandbox: seatbelt # only seatbelt is accepted today; bubblewrap and docker are not implemented for coding runs
  agentCard:
    description: Triages incoming repository issues and proposes bounded fixes.
    skills:
      - issue-triage
  grants:
    modelGate:
      allowed: true
      providers: [anthropic, openai, gemini] # also local
      budgetRef: triage-daily
    mcpTools:
      - memory.get
      - sablecourt.list_workloads
    actions:
      - git.open_pr
    delegateTo:
      - go-coder
```

Deterministic controllers set `grants.modelGate.allowed: false`, and the loader refuses a controller with `allowed: true`. The Model Gate answers a controller's call with 403 and writes a `model_gate` audit row with the reason `no_model_gate_grant`.

The loader refuses a workload with `metadata.zone: sandbox` that holds any `actions` or `delegateTo` grant. The daemon does not change a workload's zone. Moving it from `sandbox` to `runtime` is an edit of its manifest, and `sablectl promote` only asks the daemon to create the branch `sablecourt/promote-<name>`.

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
      capability: mcp:memory.get
      effect: allow
    - workload: repo-triage-agent
      capability: action:git.open_pr
      effect: approve
    - workload: "*"
      capability: policy:*
      effect: deny
```

Policy evaluation is Go code in `internal/policy` that calls no model. The outcome is `allow`, `approve` or `deny`. The engine returns `deny` for an empty workload or capability, for the prefixes `policy:`, `secret:` and `rbac:`, for an `action:` or `delegate:` capability of a sandbox workload, for a capability the workload holds no grant for (a capability with any prefix other than `mcp:`, `action:` or `delegate:` has no grant), for an `action:` capability whose circuit breaker is open, for a rule with an effect other than the three, and when no rule matches. A matching `deny` rule wins over `approve`, and `approve` wins over `allow`. Heartbeat staleness is not an input of the policy engine.

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
3. DERIVE / ACT: compute the next legal transition from stored facts and approval state, then issue adapter calls (`Start`, `Stop`, `CollectArtifacts`). `Suspend` and `Resume` are called only by the operator commands that suspend and resume a task.

Task states match the A2A lifecycle from `a2a-go`: `submitted`, `working`, `input-required`, `suspended`, `completed`, `failed`, and `cancelled`.

Workloads have no direct network route to one another. When workload A needs help from workload B, A submits a child task proposal to the control plane. The control plane function `ProposeChildTask` checks A's `delegateTo` grant through the policy engine, stores the caller-supplied sub-budget ceiling on the child task row without checking it against A's remaining ceiling, links the row to A by `parent_task_id`, and leaves B to start through the same loop. The idempotency key is the caller's, or a random one built from the parent ID, the target and a random task ID. No gate tool, action or HTTP route calls `ProposeChildTask` today, so a workload cannot trigger it yet.

## SQLite schema (WAL mode)

`sablecourtd` opens SQLite with `PRAGMA journal_mode=WAL`, `PRAGMA foreign_keys=ON`, and `PRAGMA busy_timeout=5000`. All persistence sits behind `internal/store.Store`.

Core tables in v0.1:

- `workloads`: last loaded manifest digest, zone, class, and `AgentCard` JSON.
- `tasks`: `id`, `parent_task_id`, `idempotency_key` (unique), `workload_name`, `state`, `input_json`, `artifacts_json`, `budget_ceiling_json`, `last_heartbeat_at`, `created_at`, `updated_at`.
- `task_events`: append-only state transitions and observed runtime facts.
- `leases`: active short-lived gate tokens (stored as SHA-256 hash, never plaintext), bound to `(task_id, workload_name, expires_at, revoked_at)`.
- `budget_ledger`: immutable usage rows (`task_id`, `workload_name`, `provider`, `model`, `input_tokens`, `cache_write_tokens`, `cache_read_tokens`, `output_tokens`, `cost_microusd`, `created_at`) aggregated inside transactions before granting new requests.
- `approvals`: `id`, `task_id`, `workload_name`, `capability`, `payload_canonical_json`, `payload_sha256`, `status` (`pending`, `approved`, `denied`, `expired`, `consumed`; an approved row becomes `consumed` at its one use), `decided_by`, `proof_json`, `expires_at`.
- `checkpoints`: opaque JSON or blob references persisted by long-lived AI agents before suspend and restored on resume.
- `audit_log`: append-only structured records covering policy evaluations, gate denials, budget trips, approvals, and redaction hits.

## Three Doors capability gates

Workloads never receive upstream API keys, database passwords, or Git tokens in environment variables or mounted files. At task start, `sablecourtd` mints a short-lived scoped bearer token (`sc_task_...`) bound to the task and workload identity, and injects only the gate base URLs and that task token.

### Model Gate (`internal/gate/model`)

Model Gate is a single HTTP/SSE reverse proxy listening on the internal container bridge and localhost. In v0.1 it supports three wire formats so existing SDKs and agents can point their base URL at Sablecourt with zero custom client code:

1. OpenAI-compatible: `/openai/v1/chat/completions` and `/openai/v1/responses` for OpenAI cloud, and `/local/v1/chat/completions` and `/local/v1/responses` for local OpenAI-compatible endpoints such as Ollama and LM Studio. A request on one of the two families falls back to the other (`openai` to `local` or `local` to `openai`) only on an upstream error or a 429, 502, 503 or 504, and only when the workload holds a grant for both providers.
2. Anthropic: `/anthropic/v1/messages`.
3. Google Gemini: `/gemini/v1beta/models/*:generateContent` and `:streamGenerateContent`.

For every request, Model Gate executes a strict pipeline:

1. Authenticate the `sc_task_...` bearer token against `leases` and verify `grants.modelGate.allowed == true` and provider allowlist membership.
2. Run a pre-flight budget check against `budget_ledger` for both task and daily caps; reject with `429` if exhausted.
3. Strip the caller's `Authorization`, `x-api-key` and `x-goog-api-key` headers, add the active provider's credential from `SecretStore` when there is one, and forward the request. The cloud providers need a stored key; `local` does not, and when a gateway URL is configured no provider does.
4. Stream SSE chunks through without buffering the whole response while parsing provider usage metadata from the final usage events (or non-streaming JSON body).
5. Record token counts and computed micro-USD cost atomically into `budget_ledger` and emit an OTel span plus an `audit_log` entry.

### Data and action gate (`internal/gate/mcp`)

Data and Action Gate combines two surfaces behind one policy and redaction boundary:

1. An MCP Streamable HTTP endpoint built on `github.com/modelcontextprotocol/go-sdk`. On `tools/list`, the gate filters the catalog to only the tools in the caller's `grants.mcpTools`. On `tools/call`, it evaluates `Policy`, executes allowed read tools, or pauses the task on `approve`.
2. A closed typed action catalog for mutating operations (starting with GitOps actions: `git.create_branch`, `git.open_pr`, and `sandbox.propose_promotion`). Every mutating action validates strict typed JSON inputs, checks policy and circuit breakers, and runs with the gate's scoped credentials rather than workload credentials.

Every tool response and external payload passes through `internal/redact` (a linear-time deterministic redactor modeled on Shadewright's `internal/redact`, avoiding backtracking regexes) before returning text to a workload. Per [ADR-0015](adr/0015-redaction-is-defence-in-depth.md), its pattern rules are best effort and defence in depth, not a security boundary; exact-match redaction replaces each secret the daemon holds in memory at those boundaries, Model Gate responses included ([issue 180](https://github.com/shadewright-labs/sablecourt/issues/180)). The Model Gate forwards request bodies as sent.

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
- Council turns launched in containers reach the gate only through the per-launch council proxy in `internal/gate/councilproxy` (a fixed gate route allow-list over a per-launch Unix socket or pinned TLS, with no route from the internal network to the host). The coding adapter provisions the proxy and checks its readiness before it starts the harness, and it fails the launch if either step fails. The harness receives only the proxy endpoint, and its lease is read from private scratch. Stop, retry, and daemon restart remove the container, the proxy, and the network. Before a council is created, the daemon checks that each role has a local image, a labelled login volume and a credential file in it, and refuses with the command to run otherwise ([council harnesses](council-harnesses.md)). See the proxy to daemon hop in [ADR-0011](adr/0011-sanctum-scope-trust-and-cli-harnesses.md).
- Starts containers with `--cap-drop=ALL`, `no-new-privileges`, read-only root filesystem where supported by the workload image, and a single bind mount scoped to the task's working directory.
- Long-lived agents persist state via a mounted checkpoint directory or control-plane checkpoint endpoint before `Suspend`.

### Coding agent runtime (`internal/runtime/coding`)

Used for local `claude` (Claude Code) and `codex` (Codex) runs. Status: this adapter also launches the supported council containers; its non-container host path is unsupported for 0.1.0. The [ADR-0011 amendment of 2026-11-27](adr/0011-sanctum-scope-trust-and-cli-harnesses.md#amendment-2026-11-27-container-only-harnesses-for-010) runs coding tasks in harness containers only, and [issue 181](https://github.com/shadewright-labs/sablecourt/issues/181) moves them there; until it lands, the list below is what the code does today, on macOS only.

- Creates a dedicated Git worktree per task on a deterministic branch name (`sablecourt/<task-id>`).
- Launches the CLI inside a detached session host so restarting `sablecourtd` does not terminate active coding runs.
- Wraps execution in the macOS Seatbelt profile (restricting writes outside the worktree and temp dir) with a stripped environment allowlist. No Linux Bubblewrap exists, and [ADR-0003](adr/0003-hybrid-workload-isolation.md) on this point is superseded by the amendment above.
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
    domain/             # core types, budget and policy models
    gate/
      mcp/              # Data/Action Gate (modelcontextprotocol/go-sdk + typed actions)
      model/            # Model Gate (OpenAI, Anthropic, Gemini HTTP/SSE proxy)
    policy/             # deterministic allow/approve/deny engine and circuit breakers
    redact/             # linear-time deterministic boundary redactor
    runtime/
      coding/           # Git worktree + Seatbelt + detached host for claude and codex (host path, unsupported for 0.1.0)
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
4. M4 (Three workload adapters and Web UI): Docker internal-network adapter (long-lived agent + deterministic controller), Git worktree + Seatbelt coding adapter (`claude` and `codex`; superseded for 0.1.0 by harness containers, issue 181), hub-and-spoke child delegation, and embedded `html/template` Web UI.
