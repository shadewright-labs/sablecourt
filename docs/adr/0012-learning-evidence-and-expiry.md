# ADR-0012: Learning evidence, scope, and expiry

- Date: 2026-11-24
- Status: Accepted (2026-11-25; to be revised from what the first test runs show)

## Context

Issue #41 extends the scoped learnings in ADR-0010. The context assembler derives repository identity from the checkout basename, and the matcher pulls `learning:*` entries from Court Memory. A workload granted `memory.put` can supply a learning namespace, and the legacy learning API accepts an author label from the request body. A matching row therefore supplies no reliable record of operator acceptance. [ADR-0010](0010-agentic-context-learnings-and-verification-pipeline.md), [assembler](https://github.com/shadewright-labs/sablecourt/blob/main/internal/contextpack/assembler.go), [matcher](https://github.com/shadewright-labs/sablecourt/blob/main/internal/contextpack/learnings.go), [memory tool](https://github.com/shadewright-labs/sablecourt/blob/main/internal/gate/mcp/memory.go), [daemon routes](https://github.com/shadewright-labs/sablecourt/blob/main/cmd/sablecourtd/main.go).

## Decision

Give learnings dedicated storage. Store an ID, immutable redacted statement and scope, source task, council, or incident ID, evidence reference, authenticated author, and optional predecessor. Track `proposed`, `accepted`, or `retired` status, accepting operator, creation, last-confirmed and expiry times, review reason, injection count, and linked outcomes. Keep acceptance and status history so a past packet can be traced to the version it received.

An agent, verifier, Shadewright integration, or operator may propose a learning. Derive the author from the authenticated caller. A proposal without verifiable source or evidence stays proposed. Only an authenticated operator may accept the exact version or retire it. A correction creates a linked proposal. Acceptance of a replacement retires the predecessor in the same transaction. No state or scope grants execution authority. Operator authentication from #2 must be in place before acceptance is enabled. [ADR-0011](0011-sanctum-scope-trust-and-cli-harnesses.md).

Acceptance starts a 90-day clock. Link subsequent outcomes to the specific learning. When the operator confirms that linked evidence supports it, restart the 90-day clock from that confirmation. A recorded contradiction backed by evidence, or expiry, returns it to `proposed` with its history and review reason. Withhold it as soon as either condition applies, even if a background job has yet to update the state. An injection, generic pass status, or inconclusive outcome leaves the clock alone.

Match global scope, a canonical credential-free repository remote URL, repository-relative path globs, and incident selectors for cluster, namespace, workload kind, and symptom. Require every specified selector to match. Treat missing or ambiguous selectors as a failed match for that scope. Select the most specific eligible statements first and break ties deterministically. A packet may carry up to ten statements in a redacted advisory section of at most 4 KiB. Omit overflow and count each learning once per confirmed packet delivery, keyed by packet ID.

Keep learnings out of remote model prompts, including subscription CLI prompts, and withhold injection when the destination is unknown. The only off-machine export is a redacted read of matching, accepted, unexpired advice over a scoped Shadewright Contract 3 learning route. The analyzer can supply repository and incident selectors. Grant the integration credential access to that exact read route. The daemon currently implements the incident and verification POST routes under that contract. [Daemon routes](https://github.com/shadewright-labs/sablecourt/blob/main/cmd/sablecourtd/main.go).

## Alternatives considered

Automatically renewing on `PASS` or `Cleared` treats a task verdict as evidence that a particular learning still applies. Waiting for two consecutive contradictions requires a counter and keeps disputed advice active. Rejecting incomplete proposals at intake would require a separate rule for migrated entries. Keep all unverified material subject to the same acceptance check. Perpetual acceptance leaves stale advice available. Editing in place or deleting expired rows breaks the link to past packets and outcomes. A 512-byte limit per statement adds a separate rejection rule to the packet cap. Injection without a destination check risks sending advice to a remote provider through the ordinary context path.

## Consequences

Reserve `learning:*` across generic memory routes before switching the matcher. Reject writes to those namespaces and exclude them from generic reads and searches. Import legacy entries once as proposals, keyed by original namespace and key. Preserve the original timestamp and author label as legacy metadata, mark unverified provenance, and queue ambiguous repository or path scopes for operator mapping. Roll out storage and migration first, then operator review, scoped matching and injection, and the Shadewright read. The user takes on review of imported, disputed, and expired advice.

## Summary

- Accept the dedicated, versioned record and operator-only acceptance to unblock #41.
- Accept the 90-day review clock, withholding on the first evidenced contradiction, and proposal-only migration to unblock safe injection in #41.
- Accept conjunctive scope matching, bounded local injection, and the scoped Shadewright read to unblock #41 and the incident and learning milestone in #2.
