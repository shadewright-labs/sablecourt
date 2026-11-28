# Concepts

The nouns Sablecourt uses, where each one lives (a manifest, the store, a runtime) and how they connect. Each section says whether the concept is built. The statuses are defined on the [home page](index.md#status-words). Short definitions are in the [glossary](glossary.md).

[![Manifests and operator calls enter sablecourtd; the control plane launches workloads through runtimes with a task lease, workloads call the in-process gates with that lease, and both the control plane and gates record to the store and its SHA-256 audit chain.](diagrams/concepts-map.svg)](diagrams/concepts-map.svg "Open full size")

The daemon (`sablecourtd`) reads manifests, keeps tasks in the store and launches workloads through a runtime. A container workload reaches models, tools and approvals through a gate, with a lease the daemon minted for that task. A coding agent that runs on the host (only with `--unsafe-host-harnesses` and a working host sandbox; otherwise coding tasks run in harness containers, and without a container runtime they are refused) gets the lease and the Data Gate address in its environment, but nothing forces its model traffic through the Model Gate: its sandbox profile allows outbound network, and the harness calls its provider itself. Council inference runs on each harness's own subscription login (see below). The gates run inside the daemon process. The gates, the reconciler, the Conclave broker and the approval queue append most decisions to the audit chain, but not all: an ordinary dispatch of a task is recorded as a task event only. The whole picture is the [system context diagram](how-it-works/index.md).

## Manifests

Status: built.

Declared intent is written as YAML manifests with `apiVersion: sablecourt.io/v1alpha1` and one of six kinds: `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule` and `Conclave`. `sablecourtd --manifests` takes a file or a directory, or `--manifests-git-repo` syncs a Git repository (branch `main`, every 60 seconds by default). Runtime state (tasks, leases, budget entries, approvals, audit rows) is not in the manifests: it is in the store. The loader decodes each document strictly, so an unknown field or kind is an error, and validates the whole bundle before the daemon uses it, so a bad manifest never loads partly. Among other checks it rejects a missing budget, workload or delegation target that a workload, workflow, schedule or Conclave refers to, a workload that breaks the zone and grant rules below, and a Policy whose `defaultEffect` is not `deny`. It does not check that a policy rule names a declared workload. Fields are in the [manifest reference](reference/manifests.md).

## Workload, class and zone

Status: built. The Kubernetes runtime is used only with `--runtime k8s` or `--mode citadel`.

[![Matrix of the three workload classes and the sandbox zone showing each adapter, Model Gate access (never for deterministic controllers, not injected by the coding adapter), MCP tool access, and runtime-only action and delegation grants.](diagrams/workload-class-zone.svg)](diagrams/workload-class-zone.svg "Open full size")

A workload is a unit that Sablecourt may launch. Its `class` is one of three:

- `coding-agent`: a coding CLI harness (`claude`, `codex` or `agy`) in a Git worktree. It needs a `coding` block with the harness and `repoPath`.
- `long-lived-agent`: a stateful agent in a container.
- `deterministic-controller`: a non-model program in a container. The loader refuses a workload of this class whose `modelGate.allowed` is true.

Its `runtime.type` is `docker` (which needs an image) or `worktree-cli`. The coding adapter passes the MCP Gate address and task lease in the environment, not the Model Gate address; the Docker adapter passes both when a Model Gate grant is declared, and the controller adapter refuses a launch with any Model Gate grant or address. Its `zone` is `runtime` or `sandbox`. A sandbox workload cannot hold `actions` or `delegateTo` grants, and the policy engine denies it any `action:` or `delegate:` capability. It also cannot message a runtime-zone Vassal. Moving a workload from sandbox to runtime is a change to its manifest that goes through Git review; `sablectl promote` asks the daemon to create the Git branch `sablecourt/promote-<name>` and does not change the zone. The names `claude`, `codex` and `agy` are reserved for the built-in council panel. See [workloads and runtimes](how-it-works/workloads-and-runtimes.md).

## Grant and policy

Status: built.

[![One tool or action call: fixed checks for forbidden prefixes, sandbox zone, workload grant and circuit breaker, then policy rules leading to the handler, the Approval Queue or a denial.](diagrams/policy-decision.svg)](diagrams/policy-decision.svg "Open full size")

A grant on a workload names what it may ask for: the Model Gate (`modelGate.allowed`, `providers`, and a `budgetRef` that is required when the grant is allowed), MCP tools, typed actions and delegation targets. A policy decides each MCP tool call, typed action and delegation request against those grants; the Model Gate checks the grant and the budget itself and does not evaluate policy rules. A `Policy` has rules of `workload`, `capability` and `effect` (`allow`, `approve` or `deny`), and its `defaultEffect` must be `deny`. Evaluation is deterministic: a capability the workload has no grant for is denied, an explicit `deny` wins over `approve`, `approve` wins over `allow`, and no matching rule means deny. Capabilities starting with `policy:`, `secret:` or `rbac:` are always denied, and a tripped circuit breaker on a mutating action denies the call. Model output never changes any of this (invariant 1). See [tools and actions](how-it-works/tools-and-actions.md).

## Task, state and reconciler

Status: built. Suspend and resume are partly built (#14).

[![Task states: submitted moves to working when claimed, working moves to input-required or suspended and back, and tasks end in completed, failed or cancelled.](diagrams/task-states.svg)](diagrams/task-states.svg "Open full size")

A task is one run of a workload, with an idempotency key and a parent task if it is a child. A task proposed as a child carries a budget ceiling. The launch records a heartbeat, and the reconciler refreshes it from observed runtime state. Its states are `submitted`, `working`, `input-required`, `suspended`, `completed`, `failed` and `cancelled`. The reconciler observes tasks and launches `submitted` ones up to `--max-concurrent` (4 by default); the daemon runs a pass every 2 seconds and recovers running tasks after a restart. A scheduler starts tasks from `Schedule` manifests and workflows run steps in order. See [tasks](how-it-works/tasks.md).

## Lease

Status: built.

[![Lifecycle of a task lease: the reconciler mints a one-hour token and stores its SHA-256 hash, the workload presents it to the Model Gate and MCP Gate, heartbeats renew it while running, and revocation or expiry causes gates to deny calls with 401.](diagrams/lease-lifecycle.svg)](diagrams/lease-lifecycle.svg "Open full size")

A lease is the identity a workload holds: a bearer token `sc_task_` plus 64 hex characters, bound to one task and workload. The store keeps only its SHA-256 hash. It lasts one hour (shorter near a council deadline), is renewed to one hour past each fresh observation of the running workload (capped at any council deadline), and is revoked when the task ends or is suspended. The runtime hands the token to the workload in an environment variable or on standard input; both the Model Gate and the MCP Gate hash the bearer token and check for an active lease in the store, refusing missing, expired or revoked tokens with 401. The workload never holds a provider key, a Git credential or the operator token (invariant 2); the one exception is in ADR-0011, below. See [tasks](how-it-works/tasks.md) and [model gate](how-it-works/model-gate.md).

## Gates and approvals

Status: Model Gate and MCP Gate built, with known faults tracked in #8 and #9. Approval Queue partly built (device pairing disabled).

[![States of an approval: pending moves to approved after confirmation in the native dialog, to denied when the operator denies it with the deny command, the web UI or the API (declining the native dialog leaves it pending), or to expired when the approve call or the reconciler finds it past its 15-minute deadline; a matching use before the expiry moves approved to consumed.](diagrams/approval-states.svg)](diagrams/approval-states.svg "Open full size")

The Model Gate forwards inference to a provider with the key it holds, after checking the lease, the grant and the budget. The Data and Action Gate, called the MCP Gate in this documentation, serves allowed tools over MCP and runs the closed set of typed actions after policy. An action whose effect is `approve` goes to the Approval Queue: the task waits in `input-required`, the payload is pinned by a SHA-256 digest, and a person confirms through a native macOS dialog. Pending approvals expire after 15 minutes. The repeated call must name an approval that is approved and not past its expiry, for the same task, capability and payload digest; it spends the approval, which moves to `consumed` and cannot be used again. The approval is spent before the action runs, so if the action then fails or is stopped, the caller needs a new approval. Approving needs the native dialog, which exists on macOS only, and fails closed elsewhere. Denying needs no dialog. See [model gate](how-it-works/model-gate.md) and [approvals](how-it-works/approvals.md). ADR-0004 calls the gates the Three Doors.

## Budget

Status: built (faults in #8).

[![Model Gate budget flow: estimate and reserve tokens and cost in the store for 15 minutes, deny with 429 or 403 if exhausted or invalid, otherwise forward to the provider and settle on 2xx or release on non-2xx.](diagrams/budget-flow.svg)](diagrams/budget-flow.svg "Open full size")

A `Budget` has per-task limits (`maxInputTokens`, `maxOutputTokens`, `maxCostUSD`) and a daily `maxCostUSD`, and at least one limit must be positive. `maxInputTokens` counts uncached input, cache write and cache read tokens together; cost includes provider cache write and cache read rates, falling back to the input rate when no separate cache rate is configured. The Model Gate reserves an estimate of tokens and cost from the request for 15 minutes before forwarding it, refusing the call with 429 when a limit is reached or exceeded. It settles a 2xx response with the usage the provider reports, and at no less than the reserved estimate when the usage is missing, unreadable, all zero or only partial. It releases the reservation when the upstream status is not 2xx or the call fails; if recording a 2xx settlement fails, the reservation stays until its 15 minutes pass. A task proposed as a child carries its own ceiling of the three per-task limits, which the Model Gate checks as well, and a child without a valid ceiling is denied with 403. The daily limit is counted per workload. Inference that a subscription harness runs on its own login is outside this accounting (ADR-0011). See [model gate](how-it-works/model-gate.md).

## Conclave and the terms around it

Status: built.

[![States of a Conclave: active moves to input-required when a Ward trips, a Decree resumes it, and close or stop ends the room in completed or cancelled.](diagrams/conclave-states.svg)](diagrams/conclave-states.svg "Open full size")

A Conclave is a room where several workloads, called Vassals, work on one topic. Each Vassal has a `role`, a `workload`, an `activation` and a `canMessage` list. An `always-on` Vassal is launched once when the room opens; an `on-mention` Vassal is launched when another member addresses it or addresses `all`. Wards are limits: `maxRuns`, `maxMessages` and `maxPingPong` (the last two default to twice the number of Vassals when unset, `maxRuns` to 6 and never above 6), plus the budget in `spec.budgetRef`. A room opened from a template also has a 15-minute wall time. A tripped Ward moves the room to `input-required`, and a Decree cannot resume a room paused at that deadline. The operator steers with a Decree, and Vassals and the operator share named, redacted Relics. A room is `active`, `input-required`, `completed` or `cancelled`. See [Conclaves](how-it-works/conclaves.md).

## Council and harness

Status: partly built (#28, #36, #80 open).

[![States of one council turn: reserved moves to running when the harness starts, to recorded on a clean exit with a final message, or to failed with retries back to reserved while starts remain.](diagrams/council-turn-states.svg)](diagrams/council-turn-states.svg "Open full size")

A council is a Conclave of the three harnesses `claude`, `codex` and `agy` (`--with` must name all three, once each) that answer one question in bounded rounds (two by default, `--rounds`). `sablectl council` opens it through the daemon. A harness is the coding CLI itself. In Sanctum a council turn runs in its own container when a Docker engine is configured, on a login the user made once inside that harness's container. See [council](how-it-works/council.md) and [harness containers](how-it-works/harness-containers.md).

## Court Memory, context packet and learning

Status: Court Memory and context packets built. Learning partly built.

[![Context packet assembly: a deterministic controller outside a council gets no packet, a council turn gets its reserved packet, and other tasks combine required and redacted optional parts, dropping the oldest optional entry while over 32 KiB.](diagrams/context-packet.svg)](diagrams/context-packet.svg "Open full size")

Court Memory is a key-value store in the store, shown on `/memory`, with MCP tools `memory.get`, `memory.put`, `memory.search` and `memory.delete`. Before a task starts, the daemon assembles a redacted context packet, except for tasks of a deterministic controller, which get none. Optional sections are dropped to keep it within 32 KiB; the required sections stay whole and can take it past that size. A learning is a rule the operator wants future runs to follow. Learnings come only from the dedicated record of ADR-0012 (states `proposed`, `accepted`, `retired`, a 90-day review clock) in the SQLite store, and enter a packet only when its model destination is local. Legacy `learning:*` memory rows no longer reach packets. Nothing proposes or accepts through the record yet (#43 to #47), so no packet carries a learning today. See [context and learning](how-it-works/context-and-learning.md).

## Audit chain

Status: built, with limits. Signed heads are decided only.

[![Audit hash chain anchored at genesis and linked by SHA-256 across rows, with signed heads marked planned, and sablectl audit verify walking audit_log through GET /api/v1/audit/verify to report valid (200, with the chain head) or a failed verification (409: a missing or mismatched hash, an unmet anchor, or a read error).](diagrams/audit-chain.svg)](diagrams/audit-chain.svg "Open full size")

Each audit row stores its sequence number and the hash of the previous row, anchored at the word `genesis`. `sablectl audit verify` calls `GET /api/v1/audit/verify` on the daemon, which has the store walk `audit_log` in id order and recompute each hash over length-prefixed fields that include the row ID. It answers 200 with the verified count and the chain head, or 409 when a row has no hash, a sequence or `prev_hash` does not follow, or a `row_hash` does not match. Deleting the newest rows is seen only when the operator passes back a head recorded earlier (`--anchor SEQ:HASH`). Rows written before the row ID was hashed keep their older hash and are counted as version 1 rows. See [audit](how-it-works/audit.md).

### When an audit row cannot be written

Almost every audit write goes through one of three functions in `internal/store`: `RecordAudit` returns the failure to the caller, and `LogAudit` logs it at error level. `LogClosingAudit` also logs a failure and writes the rows that close an earlier row after the step it named failed (`not_created`, `error`): they keep the request's values but not its cancellation, so a client that went away still gets them written, all closing rows of one failure share one 5 s budget, and none is written while a lock is held. The rows a store writes inside its own transaction (closing a Conclave or council, a Ward pausing a room, a decree reopening its run window) fail the same way: the error wraps `store.ErrAuditWrite`, the store error goes to the log only, and the transaction rolls back. A failure to commit such a transaction is returned as a plain store error, and the row and the change are lost together. The drift rows of the MCP Gate (the origin of the repository changed since start) call the store directly and keep a row that cannot be written owed for the next reload; at most 16 are kept, and past that the oldest is dropped, while each reload still logs the drift it sees.

Every HTTP route, web UI form, A2A call and MCP tool call that meets a failed audit write answers with the text of one function, `store.AuditFailureText`: HTTP 500 (an error result for a tool, an error redirect in the web UI whose target page shows it in its error banner, a JSON-RPC error for A2A). A test fails when a string literal in production code outside `internal/store/audit.go` reads like such a claim; it matches a pattern of the usual wordings, not every possible one. The text comes from an effects recorder that each request carries in its context (each MCP tool call gets its own). Every store method that writes appends one line to it once its change is durable, for example `task t1 was created`, `approval a1 was spent`, `conclave c1 was opened` or `relic r was written to conclave c1`, and so do the runtime and gate steps that change state outside the store: a runtime started, stopped, suspended or resumed, a takeover confirmed or released, a branch created, a commit pushed, a pull request opened or merged, a tool or action run. A write inside a transaction holds its lines until the transaction commits (`store.HoldEffects` and `store.Commit`, the only place a store transaction commits), so a rollback adds no line, and neither does a replay that commits nothing, also when the write runs inside lines a caller already holds: an identical report relic, a repeated decree whose wake is still pending, a recorded council answer posted again, closing a room that is already closed, or an update that matched no row (revoking leases a task does not hold). A guarded Conclave transaction adds a line for each change it commits: the message or answer, a report relic, a Ward pausing the room, a decree moving it back to active. Audit rows themselves are not listed. When the request recorded nothing, the answer says it was refused and nothing was changed; otherwise it lists every recorded effect in the order they happened, followed by "but an audit record could not be written". A cleanup that fails to record a cancellation therefore lists the room it opened, the task it created and the cancellations it made. A test reads the source of both backends and fails when a store method that is not listed as a read calls nothing that appends a line. Another makes every commit fail, with the write running inside lines a caller already holds, and checks that no write records a line for a change it did not commit; it always runs against SQLite, and against PostgreSQL only when `SABLECOURT_TEST_POSTGRES_DSN` is set. Which one a call site uses depends on what the row records (issue 107, row 3):

| Row records | On a failed write | Examples |
| --- | --- | --- |
| A refusal | The refusal stands and the failure is logged. | A policy denial at the gates, a refused delegation, a Conclave post a Vassal may not send, a Model Gate refusal. |
| A change about to happen | The row is written before the step it records changes anything. A failed write refuses that step (HTTP 500, an error result for an MCP tool), and the step makes no change. What the same request changed before that row stays, and the answer lists each change it recorded: an approved action has spent its approval before its `consumed` row, and the check `git.merge_pr` runs before it asks for an approval has stored its check evidence before the approval's `pending` row. If the change then fails, a second row follows with outcome `error`, `not_created`, or `denied` for an action or tool refused when it runs. | Task submission, a task retry (its `task.retry` row follows the new task's `task.submit` row, so a failed row creates no task and a repeated retry creates it), delegation, resume, an approval request, approve and deny, device pairing (its one-time code is used up before the row and stays used up, which the answer does not list; pairing is switched off over HTTP, issue #5), a typed action (its `started` row, or `consumed` for an approved run), an MCP tool call (`started`, answered with an error result), opening a Conclave, storing a Relic, a learning proposal, takeover, a promotion branch, the `evidence:git.pr_checks` row (500 for an action whether it fails in the check `git.merge_pr` runs before asking for an approval or in its run step, an error result when `git.pr_checks` is called as an MCP tool). A takeover is only reserved until its row is written: it holds off other operators but takes no stdin, and stdin never takes over by itself. A repeated takeover by the operator who already holds it reserves nothing, so a failed row leaves that takeover in place. A resume writes its row before it mints a lease or touches the runtime. |
| A change that already happened | The change stays and the caller is told its row is missing (HTTP 500, an error result for a tool, an error redirect in the web UI). | The `allowed` row after an action or tool ran, cancel, suspend, detach, the `stored` and `deleted` rows of the memory tools, a wake of Vassals refused after its message was stored, the cancellation of a task an opening Conclave had already submitted, the Shadewright intake rows written after the room or message exists, any row an approved action fails to write after its approval was spent (the text lists the spent approval and whatever else the request changed). |
| A read or a background step | The work goes on and the failure is logged. | Scry, voice transcription, workflow steps, auto-remediation, Model Gate rows written after an upstream call. Some of those come before the client gets its answer: `model.fallback`, written once the fallback provider has answered and before that answer is relayed, and the rows of an upstream answer the gate refuses to relay. |

Stopping work (cancel, suspend, detach) is never refused for a missing row, because refusing it would keep work running. An MCP tool declares no effect yet, so every tool call is treated as one that may change state.

## Mode

Status: Sanctum built. Citadel built, parked.

`--mode` is `sanctum` (the default: one user, loopback `127.0.0.1:7432`, SQLite) or `citadel` (Kubernetes runtime, listen address `0.0.0.0:7432`, PostgreSQL when `SABLECOURT_POSTGRES_DSN` is set, SQLite otherwise). `local` and `enterprise` are aliases. In Citadel mode the host coding adapter and councils are off. Outside Citadel, coding tasks run in harness containers; host mode is off unless forced with `--unsafe-host-harnesses` and unsupported for 0.1.0 (see [workloads and runtimes](how-it-works/workloads-and-runtimes.md#host-mode-unsafe-off-by-default)). ADR-0011 limits current work to Sanctum; see the [overview](overview.md#parked-parts).

## Not built yet

- Learning acceptance through the ADR-0012 record, and repository, path and incident scope matching in the packet (decided in [ADR-0012](adr/0012-learning-evidence-and-expiry.md), not built).
- Passkeys, server-side sessions and roles (decided in [ADR-0014](adr/0014-identity-and-authentication.md), Proposed, not built).
- Signed audit heads (ADR-0014 Stage 3, Proposed).

## In depth

- [ADR-0002](adr/0002-heterogeneous-mvp-workload-adapters.md), [ADR-0004](adr/0004-in-house-capability-gates-three-doors.md), [ADR-0006](adr/0006-hub-and-spoke-a2a-task-envelope.md), [ADR-0008](adr/0008-conclaves-wards-and-live-attach.md), [ADR-0010](adr/0010-agentic-context-learnings-and-verification-pipeline.md), [ADR-0013](adr/0013-bounded-council-rounds.md)
- Code: `internal/domain/types.go`, `internal/config/loader.go`, `internal/policy/policy.go`
