# Context and learning

Before a task starts, the daemon builds a short, redacted Markdown packet with the task intent, any failure handoff, standing rules and a read-only look at the repository, and passes it to the workload. Learnings are the standing rules: advice the operator wants every later run to follow. This page covers the packet, what is stored today, what the record of ADR-0012 adds, the verifier step and model tiers.

Status: partly built. The packet, the verifier step, the failure handoff and the tiers are built. The learning record exists in the SQLite store, but nothing proposes, accepts, matches or injects through it yet, and no route, page or command reaches it.

## What goes into a packet

The flow below shows how a task gets its packet: a deterministic controller gets none, a council turn gets the scheduler's packet, and every other task gets the required part plus optional parts trimmed to 32 KiB.

[![Packet assembly: a deterministic controller gets no packet, a council turn gets the scheduler's packet, and any other task gets the required part and optional parts, with the oldest optional entry dropped until the packet is 32 KiB or less.](../diagrams/context-packet.svg)](../diagrams/context-packet.svg "Open full size")

`internal/contextpack` assembles the packet each time a task launches. A `context_packet` field sent by a caller is deleted and rebuilt, and so is a `repo_path` field: the repository is the `coding.repoPath` of the workload. The parts, in order:

1. The role card of the workload, if it has one.
2. A header with the task, workload, workflow step, role and Conclave.
3. The intent: the `prompt` and `input` of the task.
4. For a Vassal woken by a peer's mention, that message, wrapped as untrusted peer data. The prompt is then replaced by a fixed sentence.
5. The failure handoff, when the task is a retry with one.
6. Scoped learnings (up to 20), the last 5 operator Decrees and decisions of the room, the last 4 Relics (each cut at 2048 bytes, wrapped as untrusted data) and the pre-check.

Parts 1 to 5 are required and never cut. Part 6 is optional: while the packet is over 32 KiB, the oldest optional entry is removed whole and the size is checked again, the pre-check going last, and the packet ends with a marker that older context was omitted. A packet can therefore exceed 32 KiB if the required part does. The whole packet and each optional entry pass through the redactor. Council turns use another packet, described in [council](council.md).

A deterministic controller never gets a packet, since it holds no model grant. Today the packet reaches two kinds of runtime: the host coding adapter, which writes it to a session file in the worktree and uses it as the harness prompt, and the built-in `sablecourt-agent` container, which sends it as the first message to the Model Gate.

## The repository pre-check

The pre-check runs only when the workload has a repository directory. It has 10 seconds in total and is cut at 16 KiB after redaction.

| Step | Runs when | Appears in the packet |
| --- | --- | --- |
| `git status --short` and `git diff --stat` | always, read-only, without filters or external diff tools | when not empty |
| `go vet ./...` | the repository has a `go.mod` and `go` is on the path | only when it reports issues |
| `sg scan` | the repository has an `sgconfig.yml` and `sg` is on the path | only when it reports issues |

The paths from `git status` also decide which path-scoped learnings match. The daemon runs these commands itself in the repository directory, not inside the harness sandbox.

## Learnings today

A learning today is a Court Memory entry whose namespace starts with `learning:`. The matcher reads the 200 most recently updated memory rows, keeps those with such a namespace, and takes up to 20:

- `learning:global` always matches.
- `learning:repo:<name>` matches when `<name>` is the last element of the repository path.
- `learning:path:<glob>` matches when the glob fits a changed path. `dir/**` matches everything under `dir`; other globs match the whole path or its last element.

An entry appears in the packet as `[namespace / key] text`. Nothing can create such an entry any more. `memory.put` and `memory.delete` refuse the namespace and write a `denied` audit row with reason `reserved_namespace`. `POST /api/v1/memory`, `DELETE /api/v1/memory/{namespace}/{key}` and the `/memory` page answer 403, and `POST /api/v1/learnings` answers 410. The memory tools, the page and `GET /api/v1/memory` hide these namespaces. `GET /api/v1/learnings?repo=&path=` (operator credential) still lists the matches. So only rows written before the namespace was closed reach packets.

The Conclave page has a "Save as learning" button. It writes a proposal as a Court Memory entry under `proposed:learning:...` with the redacted rule and its source message, and an audit row `learning.propose`. No code reads these proposals back.

Migration 0012 copied every existing `learning:*` row into the learning table as a `proposed` record with legacy provenance. The original rows stay in Court Memory and keep matching.

## The learning record of ADR-0012

[ADR-0012](../adr/0012-learning-evidence-and-expiry.md) replaces these entries with a versioned record. The store (`internal/store/learning.go`, `internal/store/sqlite/learning.go`, SQLite only) implements it and enforces the rules below. No daemon route, page or command calls it.

[![Learning states: proposed, accepted and retired, with accept, confirm, contradiction or 90 days, and retire as dashed planned transitions.](../diagrams/learning-states.svg)](../diagrams/learning-states.svg "Open full size")

Every line is dashed because the store enforces the transitions but nothing calls them yet (planned).

- A record holds an immutable redacted statement (up to 4 KiB), a scope, a source kind (`operator`, `agent`, `verifier`, `shadewright`, `task`, `council` or `incident`), an evidence reference such as `artifact:<name>` or `relic:<name>`, an author and a SHA-256 digest of those fields.
- Acceptance takes the expected digest and an operator in the request context. It needs evidence that resolves to a real artifact or Relic of the source, and it refuses a record whose scope still needs mapping. It starts a 90-day clock. Accepting a replacement retires its predecessor in the same transaction.
- A supporting outcome that the operator confirms restarts the clock. A contradiction with an evidence reference, or the 90 days, returns the record to `proposed` and the record is withheld from then on. An injection, a plain pass or an inconclusive outcome leaves the clock alone.
- Every transition is appended to a history table, and these tables refuse deletes.
- Scopes combine a global flag, a repository remote URL, up to 16 path globs and incident selectors (cluster, namespace, workload kind, symptom).

## Not built yet

- Proposing, accepting, confirming and retiring through a route, page or command. ADR-0014 would give acceptance to the `approver` role; it is Proposed.
- Proposing candidates when a run ends.
- Matching and injecting from the record: at most ten statements in an advisory section of at most 4 KiB, most specific first. The packet today takes up to 20 entries and only the 32 KiB cap applies. The 4 KiB in the store limits one statement.
- Keeping learnings out of remote model prompts, which the ADR decides. Today the packet, with any matching entry, goes to the harness or to the Model Gate.
- PostgreSQL has no learning table.

## Verifier steps

A workflow step with `role: verifier` is checked by an independent run. The example loads with the current loader (checked with `sablectl validate`).

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
      workload: go-coder
      role: verifier
      tier: critic
      maxRetries: 2
      prompt: Check the change for regressions and missing tests.
```

- The verifier gets a prompt that tells it not to trust the author, with the author's artifacts (each cut at 4096 bytes). It must run on a `coding-agent` workload. Its checkout is read-only and pinned to the author's commit, recorded as a `verifier_checkout_pinned` event. Otherwise the launch fails with "runtime cannot guarantee the author's checkout for this step".
- The verdict is the last line of the single `final-message` artifact (plain text, at most 64 KiB), exactly `VERDICT: PASS` or `VERDICT: CHANGES_REQUESTED`. A message that repeats the prompt does not count.
- `PASS` moves the workflow to the next step. With no valid verdict, the workflow halts with reason `verdict_missing` and an audit row `workflow.verify`. `CHANGES_REQUESTED` runs the author step again with the redacted findings as its handoff while `retry_count` is below `maxRetries`; at the limit the workflow stops.
- If an author step fails and has `maxRetries` set, the daemon retries it with a handoff, up to that number (one retry if the value is unset but auto remediation is on). See [retry with handoff](../how-to/retry-with-handoff.md).

## Failure handoff

`contextpack.BuildFailureHandoff` writes the block that a retry carries: the failed task, workload and state, the reason, the last 6 lifecycle events, each captured artifact with text (the last 4096 bytes of it), the last 5 task messages (each cut at 1024 bytes) and a fixed instruction to fix the root cause and verify with tests. It is redacted and sits in the packet under "Failure Handoff".

## Model tiers

A step may name a tier, `fast`, `coding` or `critic`, instead of a provider and model. The loader checks the tier against the defaults, and the daemon resolves it at dispatch from Court Memory (`system:model-tiers`), which `GET` and `POST /api/v1/providers/tiers` and the `/providers` page edit. The defaults are `cli:agy` with `gemini-3.8-flash-high`, `cli:claude` with `claude-sonnet-5-5` and `cli:claude` with `claude-opus-5-5`. A route must suit the workload: a deterministic controller cannot use a tier, and a coding workload needs a `cli:` provider of its own harness and a model that harness accepts.

## In depth

- [ADR-0010](../adr/0010-agentic-context-learnings-and-verification-pipeline.md) and [ADR-0012](../adr/0012-learning-evidence-and-expiry.md)
- [Concepts: Court Memory, context packet and learning](../concepts.md#court-memory-context-packet-and-learning)
- Code: `internal/contextpack/`, `internal/controlplane/workflow_evidence.go`, `internal/store/learning.go`, `internal/store/sqlite/learning.go`, `internal/store/sqlite/migrations/0012_learnings.sql`
