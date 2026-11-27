# ADR-0010: Agentic context assembly, scoped learnings, and verification pipeline

- Status: Accepted
- Date: 2026-11-24

## Context

Launching a coding or reasoning agent with only a raw user prompt leaves the agent cold: it misses team conventions, prior operator corrections, repository state, and earlier Conclave artifacts. Having the same agent verify its own output inherits the author's blind spots, and retrying a failed task without structured failure evidence repeats the same mistake.

## Decision

Sablecourt adopts a five-part context and verification pipeline inside the control plane.

### 1. Scoped learnings on Court Memory

Corrections and team rules are stored under reserved namespaces (`learning:global`, `learning:repo:<name>`, `learning:path:<glob>`). Operators can create learnings on `/memory` or save any Conclave message as a learning in one click.

### 2. Deterministic context assembler (`internal/contextpack`)

Before dispatching a non-deterministic workload, `Reconciler.launchTask` builds a bounded, redacted `context-packet.md` combining task intent, matching Scoped Learnings, recent Conclave Relics and Decrees, and an optional read-only repository pre-check (`git status --short`, `git diff --stat`, and `go vet ./...` when applicable, capped at 10 seconds and 16 KiB).

### 3. Independent verifier workflow steps

`WorkflowStep` accepts `role` (`author` | `verifier` | `fast`), `tier` (`fast` | `coding` | `critic`), and `maxRetries`. A `verifier` step receives the author's output and artifacts with an independent verification prompt requiring a `VERDICT: PASS` or `VERDICT: CHANGES_REQUESTED` line backed by evidence.

### 4. Closed-loop failure handoff

On task failure or verifier rejection, Sablecourt builds a standardized `failure-handoff.md` with failure reason, redacted artifacts, and recent events. Automatic remediation is opt-in (`maxRetries` or `auto_remediate`, defaulting to 1 retry when enabled). Operators can also trigger a handoff retry manually via `POST /api/v1/tasks/{id}/retry?handoff=true`, the Web UI ("Fix from failure"), or `sablectl task retry --handoff`.

### 5. Role-based model tiering

`/providers` and `GET/POST /api/v1/providers/tiers` map `fast`, `coding`, and `critic` roles to provider/model pairs persisted in `system:model-tiers`. Workflow steps that declare a `tier` without an explicit `provider` or `model` resolve from this table at dispatch time.

## Consequences

- Agents start with repository state, team rules, and prior artifacts without manual copy-paste from the operator.
- Authoring and verification can run on different models or CLI harnesses with deterministic pass/fail gating.
- All pre-checks remain read-only, time-bounded, and redacted through `internal/redact`, preserving Sablecourt's secret-isolation invariant.
- Automatic remediation stays opt-in and retry-capped, avoiding runaway loops or unexpected model spend.

## Amendment, 2026-11-27 (Proposed)

Status of this amendment: Proposed.

Title: coding court roles, operator role cards and author/verifier family separation.

This amendment adds to parts 3 and 5 above. The accepted text stays as written. On `main`, a workload's `spec.roleCard` is a string that the context assembler writes as a "Role Card" section above the packet header ([internal/domain/types.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/domain/types.go), [internal/contextpack/assembler.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/contextpack/assembler.go)). A `WorkflowStep` has a `role` (`author`, `verifier` or `fast`) and a `tier` (`fast`, `coding` or `critic`), and a tier resolves to a provider and model from `system:model-tiers` at dispatch. Nothing on `main` stops a verifier step from resolving to the same model family as the author it checks, which defeats the reason part 3 exists.

### First role set for the coding court

The coding court starts with five roles, each an operator role card:

| Card | Step role | Tier | Job |
| --- | --- | --- | --- |
| `developer-go` | `author` | `coding` | Writes Go changes and their table-driven tests in its assigned worktree. |
| `developer-docs` | `author` | `coding` | Writes docs and ADR text in its assigned worktree. |
| `reviewer` | `verifier` | `critic` | Reads the author's diff and evidence without writing, and ends with a `VERDICT:` line as part 3 requires. |
| `security` | `verifier` | `critic` | Read-only verifier for changes on invariant paths: gates, leases, redaction, policy, isolation and the audit chain. |
| `architect` | council seat | `critic` | Sits on a council ([ADR-0013](0013-bounded-council-rounds.md)) when the question is about design, not a diff. |

A fix round is not a separate role. It is the developer card again, given the failure handoff from part 4. This keeps one card per kind of work and reuses a mechanism that already exists.

A card names a tier and never a model. The operator maps tiers to models in `system:model-tiers`, so changing a model is one table edit and no card has to change. Example cards ship in `examples/coding-court/roles/` ([issue 208](https://github.com/shadewright-labs/sablecourt/issues/208)).

### Only the operator authors role cards

A card comes from the manifest the operator applies, in one of two forms: inline `spec.roleCard`, or `spec.roleCardFile`, a path resolved relative to the manifest file. The daemon refuses a `roleCardFile` that resolves inside a task checkout, after following symbolic links. The SHA-256 of the card text is recorded on the task and printed in the packet header, so a reviewer can tell which card version a run received ([issue 207](https://github.com/shadewright-labs/sablecourt/issues/207)).

The reason is invariant 3. Files in a task checkout come from the repository and from earlier agent runs, so they are untrusted input. They reach an agent only in two ways: as text the harness reads itself while it works, or as a learning the operator accepted under [ADR-0012](0012-learning-evidence-and-expiry.md). A repository file never becomes a role card, because the card sits above everything else in the packet and an agent reads it as its instructions.

A card sets the prose an agent reads, a checklist, the verdict format and a tier name. It confers nothing. A card never grants a permission, a zone, a policy, an action, a budget, a Ward, a mount, a network host, a credential or an approval. Those come from the workload's grants, the workflow and the daemon's own checks, exactly as they do without a card. A card that asks for more access changes nothing the daemon enforces.

### Author and verifier come from different model families

When the daemon dispatches a verifier step, it reads the model family of the author from the durable record of the author's run (invariant 5). It takes the family from the model the run resolved to, not from the harness name, because one harness can run models of more than one family and two harnesses can run models of the same family. If the verifier's resolved model is of the same family as the author's, or either family is unknown, the daemon refuses the step and records why (invariant 4). After a fix round, the next verifier must also differ in family from the model that ran the fix, not only from the first author ([issue 206](https://github.com/shadewright-labs/sablecourt/issues/206)).

The refusal is the point of the rule. A verifier from the same family tends to share the author's blind spots, and the accepted Consequences above already promise that authoring and verification can run on different models. Today that depends on how the operator filled the tier table. With this amendment the daemon checks it at every verifier dispatch.

### Who orchestrates in 0.1.0

In 0.1.0 the orchestrator is the daemon's fixed workflow together with the operator. The workflow sets the order of steps, the retry caps and the handoffs, and the operator applies manifests and approves. No model decides which step runs next. A model orchestrator seat that only proposes the next step, with the operator or deterministic policy deciding, comes after 0.1.0 and needs its own ADR ([issue 209](https://github.com/shadewright-labs/sablecourt/issues/209)). This keeps invariant 1 simple for the first release: no model output picks what runs.

### Open questions for the user

- Should a single-provider machine get a named override of the family refusal, recorded on each step it lets through, or does such a machine simply run without verifier steps?
- Is the `security` role mandatory for every change on an invariant path, or does each workflow opt in?
- May the orchestrator seat ever dispatch a step without the operator confirming a plan first?
- May a repository ever propose roles, for example as a card the operator reviews and accepts, or do cards stay out of repositories for good?

### Consequences of this amendment

- A machine that resolves every tier to one model family cannot run a workflow with a verifier step until the operator adds a second family or the first open question is settled. Unknown families are refused, so a newly added model needs a family entry before it can verify.
- Each verifier dispatch costs one read of the author's run record. A run without a recorded resolved model blocks the verifier that follows it.
- The operator writes and keeps the cards. A card change is a manifest change, and the digest on each task shows which version ran.
- Moving a card into a repository, or letting an agent edit one, is out. So is any card field that grants access, and a model that dispatches steps on its own in 0.1.0.
- The family rule covers workflow verifier steps. Council seats are outside it; ADR-0013 already convenes three different harnesses, and a family check for councils would need its own decision.
