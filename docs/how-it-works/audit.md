# Audit

The audit chain is the `audit_log` table, in which each row stores the SHA-256 of the row before it. It lets you check later what the gates, the approval queue and the control plane recorded, and it shows some edits to existing rows. It is a tamper-evident log with known holes, and it does not record everything: this page lists what it records, what it misses and what verification does not catch.

Status: built, with the gaps below. Detection of deleted last rows and signed heads are decided, not built (see [Not built yet](#not-built-yet)).

## How a row is chained

Each append reads the `row_hash` of the row with the highest ID, computes the hash of the new row, and inserts it in one transaction. When the table is empty, or the newest row has an empty hash, the literal `genesis` is used as the previous hash. The SQLite store keeps a single database connection, so appends do not interleave. The PostgreSQL store (parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)) locks the table for each append.

[![Each row stores the hash of the row before it, starting from genesis; audit verify reads the rows and recomputes the links; deleted newest rows leave no link to check.](../diagrams/audit-chain.svg)](../diagrams/audit-chain.svg "Open full size")

The row hash is the SHA-256 of eight fields joined with `|` in this order: previous hash, category, task ID, workload name, action, outcome, detail JSON, and the creation time as stored. The row ID is not part of the hash, and the fields are not escaped, so a `|` inside a field cannot be told apart from a separator. No key is involved.

Source: `ComputeAuditRowHash` in `internal/domain/types.go`; `AppendAudit` in `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go`; the columns come from migration `0005_audit_chain.sql`.

## What is recorded

These are all the code paths that append rows, with the category, action and outcome each writes. A row is written after the thing it describes.

| Category | Actions and outcomes | Written by |
| --- | --- | --- |
| `model_gate` | `model_gate:<provider>` with `allowed` (after a 2xx answer, with tokens and cost), `denied` (missing or unknown lease, undeclared workload, no grant, provider not granted, missing budget, budget exceeded, invalid child ceiling) or `error` (upstream request failed, upstream key unavailable, budget settlement or release failed); `model.fallback` with `ok` | `internal/gate/model` |
| `data_action_gate` | `mcp:<tool>` (Court Memory tools included) and `action:<name>` with `allowed`, `denied` (policy, policy engine unavailable, unknown action, approval does not match, tool refused) or `error` | `internal/gate/mcp` |
| `memory` | `memory.put` (`stored`, `denied`), `memory.delete` (`deleted`, `denied`), refused `memory.get` and `memory.search` (`denied`); `learning.propose` (`proposed`) from the web UI | `internal/gate/mcp`, `internal/ui` |
| `approval` | the capability as action, with `pending` when requested, `approved` (by dialog or by device signature) and `denied` | `internal/approval` |
| `device` | `device.paired` (`paired`) | `internal/approval` |
| `controlplane` | `task.submit`, `task.cancel`, `task.suspend`, `task.resume`, `task.auto_remediate`, and `delegate:<workload>` with `allowed` or `denied` | `internal/controlplane` |
| `task` | `task.retry` (`submitted`) from the retry route | `cmd/sablecourtd` |
| `workflow` | `workflow.advance`, and `workflow.verify` with `changes_requested` or `verdict_missing` | `internal/controlplane` |
| `conclave` | `conclave.open`, `conclave.close`, `conclave.ward` (`paused`), `conclave.relic_put`, `conclave.post` with `denied` (a role not allowed to message the target), and `conclave.run_window_reset` (after a Decree reopens a paused room) | `internal/conclave`, `internal/integrations`, `internal/store` |
| `integration` | `shadewright.incident_escalated`, `shadewright.verification_posted` | `internal/integrations` |
| `attach` | `task.scry` (`attached`), `task.takeover` (`acquired`), `task.detach` (`released`) | `cmd/sablecourtd` |
| `voice`, `gitops` | `voice.transcribe` (`ok`); `sandbox.propose_promotion` (`branch_created`) | `cmd/sablecourtd` |

Two places return the error when the append fails: the Shadewright integration (its `conclave.open` and `integration` rows) and the run-window reset, which is written in the same transaction as the Decree. Every other writer ignores a failed append.

## What is not recorded

- Ordinary task progress. A task moving from `submitted` to `working` to `completed` or `failed` writes a row in the `task_events` table, not in the chain.
- Operator sign-in, failed sign-in, sign-out, and every rejected operator or integration credential.
- MCP Gate requests refused for a missing, invalid or expired lease or an undeclared workload. (The Model Gate does record those.)
- An upstream model answer with a non-2xx status that is not answered by a fallback (for example an HTTP 400). Model Gate requests refused before a provider route is known, while no bundle is loaded, when the budget check itself fails, or when no upstream is configured.
- A Conclave post refused by a Ward or a limit (a newly tripped Ward writes one `conclave.ward` row, not one row per refused post), an allowed Conclave post, a Decree itself, and an approval that expires.
- Council launches and rounds, and proxy decisions. The council proxy and the council store write no chain rows.
- Anything that happens while the daemon is not running, and anything whose append failed.

## What verification checks

`sablectl audit verify`, `GET /api/v1/audit/verify`, `sablectl doctor` and the Audit page of the web UI all run the store's chain check. It reads every row in ID order. It skips a row whose stored hash is empty. For every other row it checks that the stored previous hash equals the hash of the last row it checked (`genesis` at the start) and that the stored hash equals the recomputed one. A pass reports the number of rows checked. A link or hash mismatch stops the walk and names the row; a database error stops it with that error.

This detects:

- a changed field in a hashed row, unless the row hash and every later hash were recomputed;
- a hashed row deleted from, or inserted into, the middle of the chain, because the next row's previous hash no longer matches;
- a blanked hash on a row that has a later hashed row, for the same reason.

## What verification misses

- Deletion of the last rows. Nothing follows them, so no link is left to check: any surviving prefix of the chain verifies.
- Rows with an empty hash are skipped. Blanking the hash of the newest row, or of every row, passes; the report then counts fewer rows, or zero.
- Renumbering. The row ID is not hashed, so changing IDs while keeping their order passes.
- A full rewrite. The hash has no key and no copy of the newest hash is kept elsewhere, so whoever can write the database can edit a row and recompute every row after it.
- Completeness. A pass says nothing about events that were never recorded; the previous section lists them.

A pass therefore means that the hashed rows present link up and match their fields. The how-to [verify the audit chain](../how-to/verify-the-audit-chain.md) describes a manual checkpoint for the first gap.

Dated note, 2026-11-25: [ADR-0009](../adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md) says verification detects any modified or deleted historical entry. The code does not do that for deleted last rows, blanked hashes or renumbered rows, and the ADR text is unchanged. The gaps are tracked in issue 10 ("Tracking: audit chain is weaker than ADR-0009 says").

## Reading and exporting

- `sablectl audit tail` and `GET /api/v1/audit` return the newest 50 rows, newest first.
- `sablectl audit export` and `GET /api/v1/audit/export` stream the newest 10000 rows as JSON lines, oldest first. Older rows are not exported.
- The Audit page in the web UI lists the newest 250 rows, filters them by category, workload and outcome, and runs the chain check each time it is opened.
- All of these need the operator credential; see [identity and access](identity-and-access.md).

## Not built yet

- Signed chain heads, decided in [ADR-0014](../adr/0014-identity-and-authentication.md) (Proposed): the daemon would sign the newest hash periodically and export it to a sink outside the database, so that cutting off the last rows becomes detectable.
- Authentication rows (allowed and denied credential checks) and enrolment and revocation rows, also in ADR-0014.
- Fixing the gaps above (hash every row and its ID, escape the fields, fail when an append fails) is parked with the audit chain (issue 10); the shared `armory` module is meant to hold the chain later.

## In depth

- [ADR-0009](../adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md), [ADR-0014](../adr/0014-identity-and-authentication.md), the audit limits note in [architecture](../architecture.md)
- [Threat model](../threat-model.md)
- Code: `internal/domain/types.go`, `internal/store/sqlite/sqlite.go`, `internal/store/postgres/postgres.go`, `cmd/sablecourtd/main.go` (audit routes), `cmd/sablectl/main.go` (`audit`)
