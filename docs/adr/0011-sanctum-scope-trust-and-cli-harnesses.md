# ADR-0011: Sanctum scope, trust, and CLI harnesses

- Date: 2026-11-24
- Status: Accepted (2026-11-24, by the owner; to be revised from what the first test runs show)

## Context

Issue #2 sets the build order and trust boundary for the owner's workstation. Sanctum currently leaves operator bearer authentication optional. The coding adapter conditionally exports `SABLECOURT_MODEL_GATE_URL`. Its macOS profile broadly permits reads, its Linux wrapper mounts the host root read-only, and an inapplicable sandbox choice can fall through to an unwrapped launch. The adapter starts the harness under the daemon's user identity. These behaviors leave operator state reachable to a same-user process. [Daemon startup](../../cmd/sablecourtd/main.go), [coding adapter](../../internal/runtime/coding/coding.go).

## Decision

Build the owner's local `sanctum` council with `claude`, `codex`, and `agy` first. Follow it with the Shadewright incident escalation and learning extension. Keep existing components and their regression coverage in the tree. Park feature work on Citadel, Kubernetes and PostgreSQL, remote access, A2A, GitHub integrations, upstream MCP, general workload orchestration, voice, device pairing, chat alerts, and live takeover. The council and learning decisions belong to issues #27 and #41. [ADR-0013](0013-bounded-council-rounds.md), [ADR-0012](0012-learning-evidence-and-expiry.md).

Trust the owner, daemon, and host OS. Require loopback operator authentication in Sanctum. The daemon uses a random `operator.token` in its owner-only `0700` state directory. `sablectl` reads the owner-owned `0600` file and sends a bearer token. A same-origin `/login` page exchanges that token for a short-lived `HttpOnly`, `SameSite=Strict` session cookie, with same-origin checks on browser writes. Missing credentials, unsafe file ownership or permissions, and invalid or wrong-class credentials fail closed. Workload routes use scoped, short-lived task leases. Integration routes use their own scoped credentials. When enabled, the existing optional socket enforces the same credential boundary.

File permissions protect the token from other users. A process running as the owner can still reach loopback and read owner files unless confined. An operator session may deny an approval or request confirmation. Approval requires independent human proof bound to the exact payload digest. Any enabled path for enrolling an approving device also requires independent human proof. Missing proof, a timeout, or a changed digest leaves the action blocked. Code publication still requires PR checks and human merge. [ADR-0005](0005-dual-path-human-in-the-loop.md), [ADR-0009](0009-sanctum-citadel-audit-chain-and-crypto-approvals.md).

Acceptance of this ADR would authorize a narrow exception to invariant 2 and the inference rule of ADR-0004. Only the three named Sanctum harnesses may use their own provider-managed subscription login for inference. This exception covers the credential and the provider-side identity only. Sablecourt task identities remain scoped and short-lived. The harness gets no raw provider API key, Git credential, database password, or operator token. In this mode omit the Model Gate URL and keep task lease authentication for permitted workload calls. Subscription inference runs outside Model Gate token and cost accounting. Run, round, and wall-time ceilings bound it. Whether each CLI can operate on its subscription login under these restrictions must be tested before enabling it. [ADR-0004](0004-in-house-capability-gates-three-doors.md).

Limit reads to the selected checkout, required runtime files, redacted task input, and that harness's own login. Restrict writes to private scratch and an explicitly assigned writer worktree.

## Alternatives considered

A Unix socket alone needs a browser bridge and still admits a same-user client. A second operator transport adds authentication plumbing without solving read isolation. A host-wide read mount with a deny list leaves credentials exposed when the list misses a location. A fixed token path outside the configured state directory would need separate ownership and lifecycle checks.

## Consequences

Build mandatory authentication first. Then test read isolation and process cancellation on every OS that enables these harnesses. Refuse launch when confinement or a ceiling cannot be enforced. Before enabling subscription inference, record the accepted exception in the invariant and verify that the harness cannot reach operator state, browser sessions, or unrelated credentials. Browser login adds a step, and the daemon cannot report subscription token or dollar usage.

## For the owner

- Accept the two-milestone scope and retained components to unblock #2.
- Accept mandatory loopback authentication and independent confirmation for approvals and approving-device enrollment to unblock #2 and operator acceptance in #41.
- Accept the conditional invariant 2 exception, required confinement, and finite subscription run limits to unblock the local harnesses in #2 and #27.
