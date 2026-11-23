# Security policy

## Security model and invariants

Sablecourt is designed as a policy-governed control plane that isolates untrusted workloads from raw credentials, external networks, and each other:

- Workloads never receive upstream API keys, database passwords, or Git tokens. They receive only short-lived, task-scoped bearer tokens (`sc_task_...`) bound to a single task and revoked on completion, failure, cancellation, or suspension.
- Deterministic controllers (`class: deterministic-controller`) are blocked from Model Gate at authentication time.
- Sandbox-zone workloads (`zone: sandbox`) cannot hold mutating action grants, delegation grants, or address runtime-zone Vassals inside a Conclave.
- All Model Gate, MCP, Conclave, Relic, Court Memory, Scry, Voice, and outbound notification boundaries apply linear-time RE2 secret redaction (`internal/redact`).
- Every audit record is linked into a tamper-evident SHA-256 hash chain verifiable via `sablectl audit verify` and `/audit`.
- Browser state-changing requests enforce same-origin CSRF checks (`Sec-Fetch-Site` and `Origin`), `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, and bounded request body limits.
- In `--mode=citadel`, `sablecourtd` enforces fail-closed bearer authentication (`--operator-auth-required=true`), disables host-level process execution, and requires cryptographic ECDSA P-256 device signatures for approvals.

## Reporting a vulnerability

If you discover a security issue in Sablecourt, do not open a public issue with exploit details. Report it privately through GitHub Security Advisories for this repository, including:

- Affected component and commit or release version.
- Minimal reproduction steps or failing test case.
- Impact assessment (which trust boundary or invariant is violated).
