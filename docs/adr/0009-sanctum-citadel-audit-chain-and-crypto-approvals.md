# ADR-0009: Sanctum and Citadel modes, audit hash chain, and cryptographic approvals

Status: Accepted (2026-11-23)

## Context

Sablecourt runs both on a single developer's laptop (macOS/Ubuntu with SQLite and local containers) and in enterprise Kubernetes clusters (PostgreSQL, `agentgateway`, gVisor, `NetworkPolicy`). In addition, operator approval logs and out-of-band mobile confirmations need cryptographic integrity that survives database tampering or replay attempts.

## Decision

| Component | Rule |
| :--- | :--- |
| Operating modes (`--mode=sanctum` and `--mode=citadel`) | `sanctum` (default) binds to `127.0.0.1:7432`, uses SQLite WAL, auto-discovers Podman/Docker/OrbStack/Colima/Rancher Desktop, and supports `sablectl service install` (`launchd`/`systemd`) and `sablectl tailscale expose`. `citadel` enforces `--operator-auth-required` (fail-closed `401` on empty or missing `operator_bearer_token`), targets PostgreSQL, supports `--model-gateway-url` for upstream `agentgateway`, and ships `deploy/k8s/overlays/citadel/`. Legacy `--mode=local` and `--mode=enterprise` flags remain accepted as aliases. |
| Tamper-evident SHA-256 audit chain (`0005_audit_chain.sql`) | Each `audit_log` row stores `prev_hash` and `row_hash = SHA-256(prev_hash \|\| record \|\| created_at)` anchored to `genesis`. `VerifyAuditChain()` (exposed via `/audit`, `GET /api/v1/audit/verify`, and `sablectl audit verify`) detects any modified or deleted historical entry. |
| ECDSA P-256 device pairing (`0006_devices.sql`) | Operators pair CLI or iOS Secure Enclave devices (`sablectl pair`). Approvals bind the signature to `"sablecourt-approve:" + approvalID + ":" + payloadSHA256`, preventing replay across approvals or payload substitution. |
| Scoped integration tokens | Read-only Nightloom observation uses `observer_bearer_token` (restricted to `/api/v1/observer/*` and `/scry`). Shadewright incident escalation uses `shadewright_bearer_token` (restricted to `/api/v1/integrations/shadewright/*`). |

## Diagram

[![Each audit row stores the previous row hash and its own SHA-256, and a verify call walks the chain.](../diagrams/audit-chain.svg)](../diagrams/audit-chain.svg "Open full size")

Figure: the audit hash chain and its verification; the dashed box is the signed audit heads proposed in ADR-0014, not built.

## Consequences

- Both workstation and enterprise Kubernetes deployments run the exact same Go binary and policy engine.
- Narrowly scoped bearer tokens isolate external integrations from full operator privileges.

## Amendment, 2026-11-27 (Accepted 2026-11-28)

Status of this amendment: Accepted (2026-11-28).

Title: audit chain row encoding, blank hashes and an exported head.

This amendment makes the audit chain row of the Decision table exact. The rest of the accepted text stays as written.

What `main` did before this amendment (issues #10, #104 and row 4 of #107): verification skipped rows with an empty `row_hash`, so blanking every hash verified clean. Fields were joined with an unescaped `|`, so moving a `|` between two fields kept the hash. The row ID was not hashed. Deleting the newest rows, or recomputing the whole chain after an edit, verified clean, so the claim above that any deleted entry is detected did not hold.

Decision:

- Each row stores `seq`, its 1-based position in the chain, and `hash_version`. Version 2 rows hash SHA-256 over these fields in order, each written as an 8-byte big-endian length followed by its bytes: the tag `sablecourt-audit-row-v2`, the row ID and `seq` as decimal text, `prev_hash`, `category`, `task_id`, `workload_name`, `action`, `outcome`, `detail_json` and `created_at`. The row is inserted and its hash written in the same transaction, once the ID is known.
- Verification walks rows in ID order and refuses a row with an empty `row_hash` or `prev_hash`, a `seq` that is not the next one, a `prev_hash` that is not the previous row hash, an unknown `hash_version`, a version 1 row after a version 2 row, and a hash that does not match. An append refuses to extend a chain whose newest row has no hash and returns an error without writing a row. A caller that checks that error fails its action. Many callers ignore audit write errors, so their action goes ahead without an audit row while the chain stays broken; verification keeps reporting it.
- A successful verification returns the chain head (`seq`, row ID and hash). An operator keeps the head outside the database and passes it back as `SEQ:HASH` (`sablectl audit verify --anchor`, `GET /api/v1/audit/verify?anchor=`). Verification then fails if the chain ends before that sequence or the row at it has another hash, and `sablectl` fails when the daemon does not confirm that it checked the anchor. This catches deletion of the newest rows and a chain recomputed after an edit at or before the anchor. Without an anchor, neither can be seen. A signed head is left to ADR-0014.
- Migration path: migration `0017_audit_chain_v2.sql` (SQLite) and `0013_audit_chain_v2.sql` (Postgres) number every existing row that has a hash in ID order from 1 and mark it version 1. Version 1 rows keep their stored hash and are verified with the old `|` joined rule, which still cannot see a moved `|` or an edited row ID in those rows; verification reports how many there are. Rows written before `0005_audit_chain.sql` have no hash, keep `seq` 0 and fail verification; no code can tell them from rows blanked later. The hashed rows after them were chained from genesis past them, so they verify once the rows without a hash are deleted. Rows appended after the migration are version 2 and chain onto the last older row.

### Consequences of this amendment

- A database holding rows without a hash reports a broken chain until those rows are removed by the operator, who then also loses their record. This restores verification only for rows written before `0005_audit_chain.sql`; a row blanked later, with rows after it, still breaks the link from the next row.
- Tamper evidence for the newest rows depends on the operator keeping a recent head outside the database.
