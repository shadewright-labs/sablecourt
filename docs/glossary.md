# Glossary

Short definitions, with the spelling used in code, manifests and commands. Longer explanations are in [concepts](concepts.md). Terms from the court vocabulary are glossed in plain words.

Action
:   One entry of the closed catalog of typed changes the MCP Gate can run: `git.create_branch`, `git.push`, `git.open_pr`, `git.pr_checks`, `git.merge_pr` and `sandbox.propose_promotion`. There is no free-form command. See [tools and actions](how-it-works/tools-and-actions.md).

Approval
:   A pending request to run an action whose policy effect is `approve`. Its payload is pinned by a SHA-256 digest and its status is `pending`, `approved`, `denied`, `expired` or `consumed` (used once, never usable again). See [approvals](how-it-works/approvals.md).

Approval Queue
:   The part of the daemon that holds approvals and asks a person to confirm them. On macOS the confirmation is a native dialog. The third of the capability gates.

Audit chain
:   The `audit_log` table in which each new row holds the SHA-256 of the row before it. See [audit](how-it-works/audit.md).

Barrier
:   In a council, the rule that round two starts only when every participating role has a completed answer for round one and not all of them said they have nothing to add. See [council](how-it-works/council.md).

Budget
:   A `Budget` manifest: per-task token and cost limits and a daily cost limit. Enforced by the Model Gate.

Citadel
:   The Kubernetes operating mode (`--mode=citadel`, alias `enterprise`): the Kubernetes runtime, listen address `0.0.0.0:7432` by default, and no host coding adapter or councils. See the [overview](overview.md#parked-parts).

Class
:   What kind of workload it is: `coding-agent`, `long-lived-agent` or `deterministic-controller`.

Conclave
:   A room where several workloads, the Vassals, exchange messages and files on one topic under limits. Manifest kind `Conclave`; states `active`, `input-required`, `completed`, `cancelled`. See [Conclaves](how-it-works/conclaves.md).

Council
:   A Conclave of `claude`, `codex` and `agy` (`--with` must name all three) that answers one question in bounded rounds. Started with `sablectl council`.

Court Memory
:   The key-value memory shown on `/memory` and read through `memory.*` MCP tools. Not a place for secrets.

Data and Action Gate
:   The MCP Gate: serves allowed tools over MCP Streamable HTTP and runs typed actions after policy. Route `/gate/mcp`, with actions under `/gate/actions/`.

Decree
:   A message from the operator to a Conclave, issued with `sablectl conclave decree` or the web UI. It steers the room and can resume one a Ward paused, except one paused at its deadline.

Deterministic controller
:   A workload class that runs a non-model program. It cannot hold a Model Gate grant.

Digest
:   Two uses. The payload digest is the SHA-256 of the canonical JSON of an action, pinned in an approval. The council digest is a Relic the daemon writes at the end of a council from the recorded answers.

Grant
:   What a workload may ask for, listed in `spec.grants`: Model Gate access, `mcpTools`, `mcpServers`, `actions`, `delegateTo`.

Harness
:   A coding CLI that Sablecourt launches: `claude`, `codex` or `agy`. In a council each runs in its own container when the Docker council runtime is configured.

Idempotency key
:   A key stored with a task. Submitting a second task with a key that already exists returns the existing task when the workload and the parent task are the same, instead of creating another; otherwise the submission is refused.

Lease
:   The scoped, short-lived credential of a task: `sc_task_` plus 64 hex characters, stored only as a hash. See [identity and access](how-it-works/identity-and-access.md).

Learning
:   A rule for future runs. Today a `learning:*` entry in Court Memory; the ADR-0012 record is partly built. See [context and learning](how-it-works/context-and-learning.md).

Manifest
:   A YAML document of `apiVersion: sablecourt.io/v1alpha1`. See [manifests](reference/manifests.md).

MCP Gate
:   Short name for the Data and Action Gate.

Mode
:   `sanctum` (default) or `citadel`.

Model Gate
:   The HTTP and SSE proxy under `/gate/model/` that forwards inference to a provider, checking lease, grant and budget. See [model gate](how-it-works/model-gate.md).

Operator
:   The person who runs the daemon. The role appears in the code as the caller `operator` and in the `operator.token` file.

Policy
:   A `Policy` manifest of rules that allow, approve or deny a capability for a workload. The default is deny.

Promotion
:   Moving a workload from the sandbox zone to the runtime zone by a reviewed change to its manifest. `sablectl promote` asks the daemon to create the branch.

Proxy
:   The per-launch container that is the only member of a harness container's internal network with egress. See [harness containers](how-it-works/harness-containers.md).

Reconciler
:   The loop in the daemon that observes tasks, launches `submitted` ones and records the outcome. See [tasks](how-it-works/tasks.md).

Redaction
:   Removal of secrets at a boundary, done by `internal/redact` in linear time: exact match of the secrets the daemon holds (literal bytes and a fixed list of spellings) and best-effort pattern rules, both matched on the same text and merged before anything is replaced, so neither leaves a fragment of the other. The Model Gate forwards request bodies unchanged and runs exact match on response headers and bodies.

Relic
:   A named, redacted file shared in a Conclave, put with `sablectl conclave relic put`.

Round
:   One pass in which each council role takes a turn. The default is two rounds.

Sanctum
:   The default operating mode: one user, one machine, listening on `127.0.0.1:7432` unless `--listen` says otherwise.

Sandbox zone
:   The zone (`zone: sandbox`) of a workload that holds no mutating action or delegation grant. The other zone is `runtime`.

Scry
:   Read-only, redacted live view of a task's output (`sablectl task scry`). Takeover is exclusive input to a task, through `POST /api/v1/tasks/{id}/takeover`.

Session marker
:   The environment variable `SABLECOURT_MANAGED=1` and, in coding worktrees, the file `.sablecourt-session.json` that mark a session as managed by Sablecourt.

Task
:   One run of a workload. States: `submitted`, `working`, `input-required`, `suspended`, `completed`, `failed`, `cancelled`.

Three Doors
:   The name ADR-0004 gives the capability gates: model inference, data and tools, and mutations with approval.

Turn
:   One launch of one harness in one council round.

Vassal
:   A member of a Conclave: a role bound to a workload, with an activation (`always-on` or `on-mention`) and the roles it may message.

Ward
:   A limit on a Conclave (`maxRuns`, `maxMessages`, `maxPingPong`, the budget). Tripping one pauses the room in `input-required`.

Workflow
:   A `Workflow` manifest: ordered, named steps, each naming a declared workload, optionally a role (free text; `verifier` has special handling) and a model tier.

Workload
:   A `Workload` manifest: something Sablecourt may launch, with a class, zone, runtime and grants.

Zone
:   `runtime` or `sandbox`. A sandbox workload gets no mutating grants.
