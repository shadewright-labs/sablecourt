# ADR-0013: Bounded council rounds in Conclaves

- Date: 2026-11-24
- Status: Proposed

## Context

Issue #27 adds a round-governed council mode to the Conclave in ADR-0008. The broker dispatches always-on roles at opening and can wake on-mention roles as messages arrive. The assembler selects operator Decrees, decisions, recent Relics, and the addressed message on a peer wake. The coding adapter creates a branch and worktree per task, and zero disables the broker's message and ping-pong checks. [ADR-0008](0008-conclaves-wards-and-live-attach.md), [broker](../../internal/conclave/broker.go), [assembler](../../internal/contextpack/assembler.go), [coding adapter](../../internal/runtime/coding/coding.go).

## Decision

With no flags, `sablectl council` asks the operator for a question and convenes `claude`, `codex`, and `agy` through the existing Conclave. A missing harness or a missing question prevents launch. The default checkout is a read-only view of the current working tree, including uncommitted changes. This command requires the authentication and confinement in ADR-0011. [ADR-0011](0011-sanctum-scope-trust-and-cli-harnesses.md).

The default is parallel review. In round one, each role answers independently. If the council stays open, it requires all three recorded answers before round two, when each role reviews the others. A missing or failed turn holds the barrier, with any retry subject to the remaining limits. With `--sequential`, roles run in declared order within each round. An unfinished predecessor blocks the next role. Mentions and Decrees do not dispatch extra council turns.

Each turn gets the question, applicable operator Decrees, and redacted, role-labeled excerpts from permitted completed earlier rounds. Sequential turns can also see permitted completed predecessors in their own round. Cap peer excerpts at 16 KiB total within the packet budget and mark omissions. Apply the same visibility cutoff to tool reads of messages and Relics. Existing routing and zone restrictions still apply. Treat repository data, peer text, and Relics as untrusted. They confer no permissions.

A named ref resolves once to a pinned commit and produces a read-only checkout. An explicit single-writer mode gives one selected role a fresh council worktree at that commit for the life of the council. Peers keep their read-only view and may receive the writer's redacted diff as a Relic after its turn becomes visible. The daemon refuses an invalid ref, a second writer, or a launch without enforceable isolation. Publication requires PR checks and human merge.

The default ceilings are two rounds, six starts including retries, and 15 minutes from opening. Apply defaults to omitted values and reject explicit zero or negative ceilings. Persist the ceilings for each council. Check existing Wards before dispatch and reserve each start atomically against its `(council, round, role)` turn record. Reconcile existing attempts on restart before dispatching again. Decrees cannot reset hard ceilings. Finish at the round cap or when every role explicitly reports nothing to add in one completed round. Silence never completes a turn. Stop new starts when a run limit or Ward trips. Owner stop or deadline cancels active runs. Record the reason and preserve partial results.

The daemon writes an attributed digest Relic from recorded conclusions, objections, and evidence references in round and role order. It preserves disagreements and marks missing turns. The CLI prints the digest without launching a chair.

## Alternatives considered

A sequential default anchors later answers, and mention-driven dispatch cannot guarantee review turns. Proceeding with two available or successful CLIs changes the promised panel. Choosing between piped input and `git diff HEAD` adds input rules and rejects design questions at a terminal on a clean tree. A 24 KiB peer allowance with an 8 KiB per-turn cap adds a truncation rule. Eight starts, a 10-minute deadline, a four-minute per-run timer, and a chair add scheduling and synthesis machinery. New message and ping-pong defaults add limits to a mode that already counts starts. Treating explicit zero as a default hides a configuration error. Restricting unanimous stops to review rounds can spend three more starts after everyone has finished. A writable worktree per turn adds branch cleanup and loses uncommitted changes.

## Consequences

Build read-only checkout isolation, durable turn records and reservations, visibility checks, and deadline cancellation before enabling the command. Parallel review waits for the slowest participant. Failed attempts can consume starts needed to finish.

## For the owner

- Accept the three-harness, question-led Conclave with parallel review and opt-in sequential turns to unblock #27 and the local council milestone in #2.
- Accept bounded peer visibility, read-only defaults, and one explicit writer with GitOps publication to unblock checkout and context work in #27.
- Accept the ceilings, stop rules, and daemon-written digest to unblock council scheduling and reporting in #27.
