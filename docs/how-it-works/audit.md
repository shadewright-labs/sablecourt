# Audit

The audit chain is the `audit_log` table in which each row stores the SHA-256 of the row before it. It exists so that you can check later what the gates, the approval queue and the control plane decided, and so that an edit to an old row shows up. It is a tamper-evident log, not a tamper-proof one, and it does not record everything: this page lists what it records and what it misses.

Status: built. Detection of deleted last rows and signed heads are decided, not built (see [Not built yet](#not-built-yet)).

## How a row is chained

Every append reads the hash of the newest row, computes the hash of the new one, and inserts both in one transaction. The first row uses the literal `genesis` as its previous hash. SQLite keeps a single database connection, so appends do not interleave. The PostgreSQL store (parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)) locks the table for each append.

[![Each row stores the hash of the row before it, starting from genesis; Audit verify reads the rows and recomputes the links; deleted newest rows leave no link to check.](../diagrams/audit-chain.svg)](../diagrams/audit-chain.svg "Open full size")

The row hash is the SHA-256 of eight fields joined with `|` in this order: previous hash, category, task ID, workload name, action, outcome, detail JSON, and the creation time as stored. The row ID is not part of the hash and the fields are not escaped, so a `|` inside a field is not told apart from a separator. Nothing in the hash is secret: no key signs it.

Source: `ComputeAuditRowHash` in `internal/domain/types.go`; `AppendAudit` in `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go`; the columns come from migration `0005_audit_chain.sql`.

## What is recorded

Rows are written by the code paths below, and only by them. Each writes the category, action and outcome shown. A row is written after the thing it describes, and in most paths a failed write is ignored (see [what is missed](#what-verification-misses-and-what-is-not-recorded)).

| Category | Actions and outcomes | Written by |
| --- | --- | --- |
| `model_gate` | `model_gate:<provider>` with `allowed` (after a 2xx answer, with tokens and cost), `denied` (missing or expired lease, undeclared workload, no grant, provider not granted, missing budget, budget exhausted, invalid child ceiling) or `error` (upstream request failed, upstream key missing, budget settlement failed); `model.fallback` with `ok` | `internal/gate/model` |
| `data_action_gate` | `mcp:<tool>` and `action:<name>` with `allowed`, `denied` (policy, unknown action, approval does not match) or `error` | `internal/gate/mcp` |
| `memory` | `memory.put` (`stored`, `denied`), `memory.delete` (`deleted`, `denied`), refused `memory.get` and `memory.search` (`denied`); `learning.propose` (`proposed`) from the web UI | `internal/gate/mcp`, `internal/ui` |
| `approval` | the approval request (`pending`), `approved` (by dialog or by device signature), `denied` | `internal/approval` |
| `device` | `device.paired` | `internal/approval` |
| `controlplane` | `task.submit`, `task.cancel`, `task.suspend`, `task.resume`, `task.auto_remediate`, and `delegate:<workload>` with `allowed` or `denied` | `internal/controlplane` |
| `task` | `task.retry` | `cmd/sablecourtd` |
| `workflow` | `workflow.advance`, and `workflow.verify` with `changes_requested` or `verdict_missing` | `internal/controlplane` |
| `conclave` | `conclave.open`, `conclave.close`, `conclave.ward` (`paused`), `conclave.relic_put`, `conclave.run_window_reset` (on a Decree), and `conclave.post` with `denied` | `internal/conclave`, `internal/store` |
| `integration` | `shadewright.incident_escalated`, `shadewright.verification_posted` | `internal/integrations` |
| `attach` | `task.scry` (`attached`), `task.takeover` (`acquired`), `task.detach` (`released`) | `cmd/sablecourtd` |
| `voice`, `gitops` | `voice.transcribe`; `sandbox.propose_promotion` (`branch_created`) | `cmd/sablecourtd` |

The Conclave post that a Ward or a limit refuses writes its `denied` row in the same transaction as the check. The Shadewright integration is the one writer that returns the error when the append fails.

## What is not recorded

- Ordinary task progress. A task moving from `submitted` to `working` to `completed` or `failed` writes a row in the `task_events` table, not in the chain. Only submit, cancel, suspend, resume, retry and the workflow and remediation steps above write chain rows.
- Operator sign-in, failed sign-in, sign-out, and every rejected operator or integration credential. No authentication row exists.
- An upstream model answer with a non-2xx status that is not a fallback case (for example an HTTP 400) passes through the Model Gate with no row. Requests refused before a route is known, and the cases where no bundle is loaded or no upstream is configured, write none either.
- A successful `memory.get` or `memory.search`, an allowed Conclave post, a Decree itself (only a run-window reset it causes), and an approval that expires.
- Council launch, round and proxy decisions. The council proxy and the council store write no chain rows.
- Anything that happens while the daemon was not running, and anything whose append failed.

## What verification checks

`sablectl audit verify`, `GET /api/v1/audit/verify`, `sablectl doctor` and the Audit page of the web UI each ask the daemon, which recomputes the chain through the store. Starting from `genesis`, for every row in ID order that has a hash it checks that the stored previous hash equals the hash of the last hashed row, and that the stored hash equals the recomputed one. A pass reports the number of rows verified. The first failure stops the walk and names the row.

This detects:

- a changed field in a hashed row, if the row hash was not recomputed;
- a deleted or inserted row in the middle of the chain, because the next row's previous hash no longer matches;
- a row whose hash was blanked, if a later hashed row exists, for the same reason.

## What verification misses and what is not recorded

- Deletion of the last rows. Nothing follows them, so no link is left to check. A chain cut back to any earlier row still verifies.
- Rows without a hash are skipped. Blanking the hash of the newest row, or of every row, passes; the report then counts fewer rows or zero.
- A full rewrite. The hash has no key and no copy of the newest hash is kept elsewhere, so someone who can write the database can edit a row and recompute every row after it.
- Completeness. The chain proves that the rows present were not changed in the ways above. It does not prove that every event was recorded, and the previous section lists events that never are.
- A write that failed. Almost every caller discards the append error, so an action can happen with no row.

A pass therefore means "the rows that are here link up", nothing more. The how-to [verify the audit chain](../how-to/verify-the-audit-chain.md) says what to do with that.

Dated note, 2026-11-25: [ADR-0009](../adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md) says verification detects any modified or deleted historical entry. The code does not do that for deleted last rows or blanked hashes, and the ADR text is unchanged. The gaps are tracked in issue 10 ("Tracking: audit chain is weaker than ADR-0009 says").

## Reading and exporting

- `sablectl audit tail` shows the newest 50 rows. `GET /api/v1/audit` returns the same 50.
- `sablectl audit export` and `GET /api/v1/audit/export` stream the newest 10000 rows as JSON lines, oldest first. A larger log is exported only in part.
- The Audit page in the web UI filters by category, workload and outcome and verifies the chain each time it is opened.
- All of these need the operator credential; see [identity and access](identity-and-access.md).

## Not built yet

- Signed chain heads, decided in [ADR-0014](../adr/0014-identity-and-authentication.md) (Proposed): the daemon would sign the newest hash periodically and export it to a sink outside the database, so that cutting off the last rows is detectable.
- Authentication rows (every allowed and denied credential check) and enrolment and revocation rows, also in ADR-0014.
- Fixing the gaps listed above (hash every row, include the row ID, escape the fields, fail when an append fails) is parked with the audit chain itself (issue 10); the shared `armory` module is meant to hold the chain later.

## In depth

- [ADR-0009](../adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md), [ADR-0014](../adr/0014-identity-and-authentication.md), the audit limits note in [architecture](../architecture.md)
- [Threat model](../threat-model.md)
- Code: `internal/domain/types.go`, `internal/store/sqlite/sqlite.go`, `cmd/sablecourtd/main.go` (audit routes), `cmd/sablectl/main.go` (`audit`)
