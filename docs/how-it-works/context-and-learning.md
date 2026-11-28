# Context and learning

Before a task starts, the daemon builds a redacted Markdown packet with the task intent, any failure handoff, matching standing rules and, for a workload with a repository, a read-only look at it, and puts it in the launch input of the workload. Learnings are the standing rules: advice that later packets carry when its scope matches. This page covers the packet, what is stored today, what the record of ADR-0012 adds, the verifier step and model tiers.

Status: partly built. The packet, the verifier step, the failure handoff and the tiers are built. The learning record exists in the SQLite store, and the packet reads its accepted records for a local destination only. Apart from the migration that imports old entries, nothing proposes, accepts, confirms or retires through it yet (issues #43 and #46), so in practice no packet carries a learning today.

## What goes into a packet

The flow below shows how a task gets its packet: a deterministic controller outside a council gets none, a council turn gets the packet reserved for it, and every other task gets the required part plus optional parts, which are dropped whole, oldest first, while the packet is over 32 KiB.

[![Packet assembly: a deterministic controller outside a council gets no packet, a council turn gets the packet reserved for it, and any other task gets the required part and optional parts, with the oldest optional entry dropped while the packet is over 32 KiB.](../diagrams/context-packet.svg)](../diagrams/context-packet.svg "Open full size")

The second picture shows where each part comes from and what stays out. The size fit never drops a required part, so a packet whose required text alone is over 32 KiB stays over it. Optional entries are dropped whole, oldest first, while the packet is over 32 KiB. A caller's `context_packet` and `repo_path` fields are not used to build it, and the assembled packet replaces a caller's `context_packet` in the launch input. Learnings enter only when the destination check below allows them, and a council turn gets the packet its reservation holds. A deterministic controller task that is not a council turn gets no packet: its submitted input reaches launch unchanged, a caller's `context_packet` included, unless its `conclaveId` names an existing council, in which case the caller's `context_packet` is removed and nothing replaces it.

[![Where the packet parts come from: required parts (role card, task header and input, peer mention) and optional entries (Conclave Decrees and Relics, learnings, pre-check) feed the redacted packet, which goes in the launch input and can exceed 32 KiB when the required text alone does; a caller's context_packet and repo_path, learnings the destination check refuses, and council turns are kept out of an assembled packet, and a deterministic controller that is not a council task gets no packet and keeps a context_packet it was submitted with unless its conclaveId names an existing council.](../diagrams/context-packet-sources.svg)](../diagrams/context-packet-sources.svg "Open full size")

`internal/contextpack` assembles the packet each time a task launches, apart from the two cases above. For a task that gets an assembled packet, launch preparation deletes a `context_packet` field sent by the caller and puts the assembled packet in its place, and it hides a caller's `repo_path` from the assembler, so the pre-check runs in the workload's `coding.repoPath`. A deterministic controller task that is not a council task and names no council keeps its submitted input, so a `context_packet` field in it is not removed. Launch preparation builds a separate launch input and does not change the stored submission. The submitted input is kept as a task message, and the built-in agent sends the task messages to the Model Gate along with the packet, so a `context_packet` or `repo_path` in the submitted input can reach a model provider. Before launch, and when a task message is read back, a record that shows a registered secret value is replaced whole by a refusal marker. The parts, in order:

1. The role card of the workload, if it has one.
2. A header with the task and workload, and the workflow step, role and Conclave when the task has them.
3. The intent: the `prompt` and `input` of the task, or the raw input when both are empty.
4. For a Vassal woken by a peer's mention, that message, wrapped as untrusted peer data. The prompt is then replaced by a fixed sentence.
5. The failure handoff, when the task is a retry with one.
6. Learnings (at most ten, in a section of at most 4 KiB, and only for a local destination, see below), the last 5 Decrees and decisions that the operator posted in the room, the last 4 Relics of the room in name order (each cut at 2048 bytes, wrapped as untrusted data) and the pre-check. Decrees, decisions and Relics appear only for a task that belongs to a Conclave.

Parts 1 to 5 are required and are not cut. Part 6 is optional: while the packet is over 32 KiB, the oldest optional entry is removed whole and the size is checked again, the pre-check going last because it is stamped with the current time. When the required part leaves room, the packet ends with a marker that older context was omitted. A packet can exceed 32 KiB if the required part does. The required part and each optional entry pass through the redactor. Council turns use another packet, described in [council](council.md).

A deterministic controller outside a council gets no assembled packet (a `context_packet` it was submitted with stays, as described above), and the controller adapter refuses a model grant for it; a [command step](command-steps.md) gets none either. Every other task carries the packet in the `context_packet` field of its launch input. The coding adapter uses it as the harness prompt and, on a host run (only with `--unsafe-host-harnesses`) outside a council checkout, also writes it to a session file in the worktree; harness containers do not get that file. The built-in `sablecourt-agent` container sends it as the first message to the Model Gate, together with the task history. Docker and Kubernetes workloads receive it in `SABLECOURT_INPUT_JSON` and use it only if their own code reads it.

## The repository pre-check

The pre-check runs only when the workload has a `coding.repoPath` that names an existing directory. It has 10 seconds in total and is cut at 16 KiB after redaction.

| Step | Runs when | Appears in the packet |
| --- | --- | --- |
| `git status --short` and `git diff --stat` | read-only, without filters or external diff tools; skipped when repository discovery or filter setup fails | when not empty |
| `go vet ./...` | the repository has a `go.mod` and `go` is on the path | only when it reports issues |
| `sg scan` | the repository has an `sgconfig.yml` and `sg` is on the path | only when it reports issues |

The paths from `git status` are what path-scoped learnings match against, but no packet that may carry learnings has them today: the pre-check needs a `coding.repoPath`, the destination check refuses learnings to a workload with a `coding` block, and the control plane drops `repo_path` from the task input of an ordinary task. The daemon runs these commands itself in the repository directory, not inside the harness sandbox.

## Learnings in the packet

The matcher reads only the learning record of [ADR-0012](../adr/0012-learning-evidence-and-expiry.md). It never reads `learning:*` rows in Court Memory, so those legacy rows reach no packet.

### Destination check

[ADR-0012](../adr/0012-learning-evidence-and-expiry.md) keeps learnings out of remote model prompts and out of any packet whose destination is unknown. The decision of 2026-11-27 on issue #99 sets the rule: a packet carries learnings only when its model destination is local. A destination is local when all of these hold:

- The daemon runs long-lived agents in the Docker adapter, whose internal network has no egress. Kubernetes, or no runtime for long-lived agents, makes the destination unknown.
- No model gateway URL is set (`--model-gateway-url` or `SABLECOURT_MODEL_GATEWAY_URL`). A gateway receives requests for every provider, `local` included.
- The workload is a `long-lived-agent` and has no `coding` block. A CLI harness prompt goes to a remote model, and the Docker adapter places no other class, so an empty or unknown class counts as unknown.
- `grants.modelGate` is allowed and lists exactly one provider, `local`. An empty list allows every provider, and a list with any other provider can route a request to a remote one, including through fallback.

The daemon fixes the first two facts once at startup and logs them in the line "context packet learning destination fixed at startup". Every other packet gets no learnings, and the store is not read for it.

A reload can widen the grant of a task that is already running. So when a packet carries learnings, the reconciler pins the task's lease to the providers that packet was admitted under (`local`), and the Model Gate refuses a request from that lease to any other provider (403, `provider_not_admitted_with_learnings`), whatever the current grant says. Fallback from `local` stays inside the pinned set too. A task launched after the reload is checked against the new grant, so it gets no learnings and an unpinned lease. Council turns use their own packet, which never carries learnings. The `local` provider is assumed to stay on this machine: its default upstream is loopback, and a local upstream configured elsewhere would break that assumption.

### Matching

For a local destination the assembler asks the store for records that are `accepted`, not expired and not contradicted, with every scope selector matching:

- A global record matches every task.
- A path-scoped record matches when one of its globs fits a path from `git status` of the pre-check. A `**` segment matches zero or more path components. A packet that passes the destination check has no pre-check paths today, so a path-scoped record never matches yet.
- A record with a repository remote URL never matches yet: the assembler derives no remote URL from the checkout, and a missing selector fails the match.
- A record with incident selectors never matches an ordinary task, for the same reason.

The most specific records come first. At most ten enter the packet, as `- [id] statement` lines under "Scoped Learnings", and the section stops before a line that would take it past 4 KiB. The 4 KiB covers the rendered section, the heading and its blank lines included. Each line passes through the redactor. The 32 KiB packet trim can still drop learning entries, oldest first. The injection count of a record is not updated yet.

`GET /api/v1/learnings?repo=&path=` (operator credential) lists up to 100 such records as `domain.Learning` objects, where `repo` is a repository remote URL; an invalid `repo` gets 400. It does not apply the destination check, because it answers the operator and builds no prompt.

The Conclave page has a "Save as learning" button next to each message. It writes a proposal as a Court Memory entry under `proposed:learning:...` with the redacted rule and its source message, and an audit row `learning.propose`. Nothing acts on these proposals, and they never reach a packet.

Migration 0012 imports each existing `learning:*` row once into the learning table as a `proposed` record with legacy provenance. Only `learning:global` keeps a scope. Every other scope is marked as needing an operator mapping. A row with an invalid timestamp, author, key, namespace or value is marked malformed and also needs a mapping, whatever its scope; its statement is replaced by a placeholder only when the statement text itself is invalid. The original rows stay in Court Memory, but the matcher no longer reads them, and no route writes new ones: `memory.put` and `memory.delete` refuse the namespace with a `denied` audit row (reason `reserved_namespace`), `POST /api/v1/memory` and the `/memory` forms answer 403, and `POST /api/v1/learnings` answers 410.

## The learning record of ADR-0012

[ADR-0012](../adr/0012-learning-evidence-and-expiry.md) replaces these entries with a versioned record. The store (`internal/store/learning.go`, `internal/store/sqlite/learning.go`, SQLite only) implements it and enforces the rules below. The packet matcher and `GET /api/v1/learnings` read it; no route, page or command changes it.

[![Learning states: proposed, accepted and retired, with accept, confirm, contradiction or 90 days, and retire as dashed planned transitions.](../diagrams/learning-states.svg)](../diagrams/learning-states.svg "Open full size")

Every line is dashed because the store enforces the transitions but no route, page or command calls them yet (planned). Only the migration creates `proposed` records today.

- A record holds an immutable redacted statement (up to 4 KiB), a scope, a source kind (`operator`, `agent`, `verifier`, `shadewright`, `task`, `council` or `incident`, plus `legacy` for imported rows), an evidence reference such as `artifact:<name>` or `relic:<name>`, an author and a SHA-256 digest of the immutable fields (statement, scope, source, evidence reference, predecessor).
- Acceptance takes the expected digest and an operator in the request context. It needs evidence that resolves to a real artifact (sources `task`, `agent` and `verifier`) or Relic (sources `council`, `incident` and `shadewright`) of the source, so a record with source `operator` or `legacy` cannot be accepted yet. It also refuses a record whose scope still needs mapping. It starts a 90-day clock. Accepting a replacement retires its predecessor in the same transaction.
- A supporting outcome that the operator confirms restarts the clock. A contradiction with an evidence reference, or the 90 days, returns the record to `proposed` and the record is withheld from then on. An injection or an inconclusive outcome leaves the clock alone.
- Every transition is appended to a history table. The learning, history, outcome and injection tables refuse deletes.
- A scope is either global or a combination of narrower selectors: a repository remote URL, up to 16 path globs and incident selectors (cluster, namespace, workload kind, symptom). Global cannot be combined with any of them.

## Not built yet

- Proposing, accepting, confirming and retiring through a route, page or command. ADR-0014 would give acceptance to the `approver` role; it is Proposed.
- Proposing candidates when a run ends.
- Matching repository-scoped, path-scoped and incident-scoped records in the packet: the assembler has no repository remote URL or incident selectors, and a packet that passes the destination check has no pre-check paths.
- Counting each learning once per confirmed packet delivery, keyed by packet ID.
- PostgreSQL has no learning table.

## Verifier steps

A workflow step with `role: verifier` is an independent run that checks the work of the author step before it. The example passes `sablectl validate`.

```yaml
apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata:
  name: daily
spec:
  perTask:
    maxCostUSD: 2.50
  daily:
    maxCostUSD: 15.00
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: go-coder
  zone: runtime
spec:
  class: coding-agent
  runtime:
    type: worktree-cli
  coding:
    harness: claude
    repoPath: .
    sandbox: seatbelt
  grants:
    modelGate:
      allowed: true
      providers: [anthropic]
      budgetRef: daily
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: go-reviewer
  zone: runtime
spec:
  class: coding-agent
  runtime:
    type: worktree-cli
  coding:
    harness: codex
    repoPath: .
    sandbox: seatbelt
---
apiVersion: sablecourt.io/v1alpha1
kind: Workflow
metadata:
  name: fix-and-verify
  zone: runtime
spec:
  description: Fix a bug, then have a second run check the result.
  steps:
    - name: fix
      workload: go-coder
      role: author
      tier: coding
      prompt: Fix the bug described in the task input and run the tests.
    - name: check
      workload: go-reviewer
      role: verifier
      tier: critic
      maxRetries: 2
      prompt: Check the change for regressions and missing tests.
```

[![The verifier loop: the author step's artifacts go to a verifier whose checkout is pinned to the author's commit; PASS moves to the next step, a missing verdict halts the workflow with verdict_missing, and CHANGES_REQUESTED runs the author step again with the findings as its failure handoff while retries are left, and otherwise starts no further step.](../diagrams/verifier-loop.svg)](../diagrams/verifier-loop.svg "Open full size")

- The verifier runs on its own workload. The manifest loader refuses a `role: verifier` step whose workload holds any grant other than the read-only `mcpTools` `memory.get`, `memory.search`, `conclave.read`, `conclave.relic_get` and `sablecourt.list_workloads`: `actions`, `delegateTo`, `mcpServers`, `repos`, any other tool and any `modelGate` setting are refused, and an upstream server cannot register one of these names, because the loader refuses an `mcpServers` name in a built-in namespace. It also refuses a verifier whose workload is also used by a step before it that is not a verifier, and a verifier on a model workload when no earlier step runs a model. Here `go-reviewer` runs the `codex` harness because the default `critic` route is `cli:codex` with `gpt-6.2-astra`, a model family other than the `coding` route's.
- The verifier gets a prompt that tells it not to trust the author, with the text of the author's artifacts (each cut at 4096 bytes). It must run on a `coding-agent` workload. Its checkout is read-only and pinned to the author's commit, recorded as a `verifier_checkout_pinned` event. If the workload is not a coding agent, the runtime cannot pin a commit, or the author recorded no commit, the launch fails with "runtime cannot guarantee the author's checkout for this step".
- Only an author step sets the commit a verifier reviews. A verifier that follows another verifier (for example review, then security) is pinned to the same commit as the first one, even if the first verifier recorded a commit of its own, and its `CHANGES_REQUESTED` runs that same author step again. Its prompt carries the artifacts of the step before it, the first verifier. After a remediation run of the author, the next verifier is pinned to the commit that run recorded.
- The verdict is the last line of the single `final-message` artifact (plain text, at most 64 KiB), exactly `VERDICT: PASS` or `VERDICT: CHANGES_REQUESTED`. A message that repeats the prompt does not count.
- `PASS` moves the workflow to the next step. With no valid verdict, the workflow halts with reason `verdict_missing` and an audit row `workflow.verify`. `CHANGES_REQUESTED` writes a `workflow.verify` audit row and runs the author step again with the redacted findings as its handoff while `retry_count` is below `maxRetries`; at the limit no further step starts. The `maxRetries` that counts is the one on the verifier step, or the author step's own when the verifier step sets none.
- A task of a step that has `maxRetries` set, or a task whose input sets `auto_remediate`, is retried with a handoff when it fails, up to that number (one retry if only `auto_remediate` is set). The retry happens only when the daemon saw the runtime exit (`runtime_exit_observed`). A task that fails to launch, is lost on restart, or fails because a runtime output log could not close or finalize (`log_close: true` or exit code `-2`) is not retried. In the example above `maxRetries` is on the `check` step, so it limits the verdict loop and the `check` run itself, not a failure of the `fix` step. See [retry with handoff](../how-to/retry-with-handoff.md).

## Failure handoff

`contextpack.BuildFailureHandoff` writes the block that a retry carries: the failed task, workload and state, the reason, the last 6 lifecycle events (with their facts wrapped in `<untrusted-event-facts>` as untrusted runtime data), each captured artifact with text (the last 4096 bytes of it), the last 5 task messages (each cut at 1024 bytes) and a fixed instruction to fix the root cause and verify with tests. It is redacted. In the packet of a task that gets one it sits under "Failure Handoff" and is not cut. A deterministic controller gets no packet, so a retried controller receives the block only as the `failure_handoff` field of its input.

## Model tiers

A step may name a tier, `fast`, `coding` or `critic`, as shorthand for a provider and model. A provider or model set on the step itself replaces the route of the tier. The loader checks the tier name and route against the defaults, and the daemon resolves it at dispatch from Court Memory (`system:model-tiers`), which `GET` and `POST /api/v1/providers/tiers` and the `/providers` page edit. The defaults are `cli:agy` with `gemini-3.8-flash-high`, `cli:claude` with `claude-sonnet-5-5` and `cli:codex` with `gpt-6.2-astra`; [configuration](../reference/configuration.md#model-tiers-and-families) lists their model families. A route must suit the workload: a deterministic controller cannot use a tier, and a `coding-agent` workload needs a `cli:` provider of its own harness and a model that harness accepts. Other classes get no further check.

## In depth

- [ADR-0010](../adr/0010-agentic-context-learnings-and-verification-pipeline.md) and [ADR-0012](../adr/0012-learning-evidence-and-expiry.md)
- [Concepts: Court Memory, context packet and learning](../concepts.md#court-memory-context-packet-and-learning)
- Code: `internal/contextpack/`, `internal/controlplane/workflow_evidence.go`, `internal/store/learning.go`, `internal/store/sqlite/learning.go`, `internal/store/sqlite/migrations/0012_learnings.sql`
