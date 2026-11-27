# Security policy

## Security model and invariants

Sablecourt is designed as a policy-governed control plane that isolates untrusted workloads from raw credentials, external networks, and each other:

- Workloads never receive upstream API keys, database passwords, or Git tokens. They receive only short-lived, task-scoped bearer tokens (`sc_task_...`) bound to a single task and revoked on completion, failure, cancellation, or suspension.
- Deterministic controllers (`class: deterministic-controller`) are blocked from Model Gate at authentication time.
- Sandbox-zone workloads (`zone: sandbox`) cannot hold mutating action grants, delegation grants, or address runtime-zone Vassals inside a Conclave.
- Data and Action Gate handler results and handler errors (not the approval-required reply or an approval queue error, which are returned without redaction), Conclave messages and Relics, Court Memory writes, Scry output, voice transcripts and speech, and outbound notifications pass through `internal/redact`, a best-effort pattern redactor that is defence in depth and not a boundary ([ADR-0015](docs/adr/0015-redaction-is-defence-in-depth.md)); exact-match redaction of secrets the daemon holds is decided and not built yet ([issue 180](https://github.com/shadewright-labs/sablecourt/issues/180)). Model Gate does not redact request or response content, so content a harness reads can reach its provider.
- Audit verification checks hashes and previous-row links for hashed rows through the last row present in the database. It skips legacy rows without hashes and cannot detect deletion of the final row without an external checkpoint. External anchoring is tracked by [epic #1](https://github.com/shadewright-labs/sablecourt/issues/1); ADR-0009's broader wording remains unchanged.
- Browser state-changing requests enforce same-origin CSRF checks (`Sec-Fetch-Site` and `Origin`), `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, and bounded request body limits.
- In `--mode=citadel`, `sablecourtd` enforces fail-closed bearer authentication (`--operator-auth-required=true`), disables host-level process execution, and requires cryptographic ECDSA P-256 device signatures for approvals.

## Known redaction gaps

Pattern redaction is best effort and not a boundary (AGENTS.md invariant 3, [ADR-0015](docs/adr/0015-redaction-is-defence-in-depth.md)). Inputs that `internal/redact` is known to miss, such as some URL userinfo, JWT, truncated PEM and cookie shapes, are pinned by the corpus tests and listed by class in the generated table in the Redaction section of [Tools and actions](docs/how-it-works/tools-and-actions.md). A secret in one of those shapes can reach model context, logs and Conclave messages.

## Verifying a release

[Verify a release](docs/how-to/verify-a-release.md) lists the commands that check the signatures and provenance of a release.

## Reporting a vulnerability

If you discover a security issue in Sablecourt, do not open a public issue with exploit details. Report it privately through GitHub Security Advisories for this repository, including:

- Affected component and commit or release version.
- Minimal reproduction steps or failing test case.
- Impact assessment (which trust boundary or invariant is violated).
