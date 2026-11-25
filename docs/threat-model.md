# Threat model

What Sablecourt protects, where its trust boundaries are, who it defends against, which control each defence rests on, and which threats have no control yet. A control is listed only if it exists in the code; the package that implements it is named. Anything decided but not built is in its own section, and nothing in the table below depends on it.

Status: partly built. Local trust (Sanctum) is built; the identity work of [ADR-0014](adr/0014-identity-and-authentication.md) is Proposed and not built; the audit chain has known holes.

## What is protected

- The provider logins of the three harnesses, each kept in a volume of its own container.
- Provider API keys, the Git token and the secret store behind the gates.
- The operator token and the session cookie.
- Approvals: the pinned payload digest and the decision.
- The audit chain.
- The repository checkouts the harnesses read, and the one writer worktree a council writes to.
- Budgets and the run, round and wall-time ceilings.

## Trust boundaries

[![The host holds the daemon and gates, a per-launch internal network with a gate proxy and a harness container, and a host-mode harness; providers, the Git host and integration callers are outside it.](diagrams/trust-boundaries.svg)](diagrams/trust-boundaries.svg "Open full size")

You, the daemon and the operating system are trusted (ADR-0011). Everything else is not.

- Host: the trusted zone. A process that runs as your user can read your files, including the operator token, unless it is confined. That is why harnesses are confined.
- Daemon and gates: `sablecourtd` with the Model Gate (holds provider keys), the MCP Gate and the lease-checked `/gate/*` routes (hold the Git token, run typed actions, serve Conclave rooms), the operator routes, the integration routes and the audit chain. Workloads never receive the keys or tokens these hold.
- Internal network: one per council launch. It contains the harness container and the gate proxy and nothing else. The proxy is the only member with a route out. The harness has no direct route to the internet or to the daemon: "direct route: never" in the diagram.
- Gate proxy: checks the lease, forwards only a fixed list of `/gate/` routes to the daemon, and tunnels HTTPS only to the provider hosts of that harness.
- Host-mode harness: a harness run on the host under the macOS Seatbelt sandbox, with a stripped environment. It reaches the gate over loopback with its lease. ADR-0011 calls this the weaker fallback.
- Outside the host: model providers (reached by the Model Gate with a key it holds), the Git host (reached by the MCP Gate with a token it holds), the provider hosts the proxy allows, and the integration callers Shadewright and Nightloom, each with a scoped token.

Container mode is covered in [harness containers](how-it-works/harness-containers.md) and [council](how-it-works/council.md); the credentials are in [identity and access](how-it-works/identity-and-access.md).

## Adversaries and the controls that exist

| Threat | Control in code | Package |
| --- | --- | --- |
| Repository files, logs or a peer harness's text steer a model into an unwanted action (invariants 1 and 3) | Policy default is deny and a manifest with another default is rejected; the action catalog is closed and an unknown action is denied; an action whose effect is `approve` waits for a person to confirm the exact payload digest; secrets and tokens in gate output, Conclave messages, Relics, Court Memory writes and council context are redacted | `internal/policy`, `internal/config`, `internal/gate/mcp`, `internal/approval`, `internal/redact`, `internal/conclave` |
| A sandbox-zone workload mutates or delegates | The loader rejects a sandbox workload that holds `action:` or delegation grants; the policy engine denies those capabilities for the zone; a sandbox workload's Conclave message to a runtime-zone workload is refused | `internal/config`, `internal/policy`, `internal/conclave` |
| A compromised harness reads host files or another harness's login | Container turns run as an unprivileged user with a read-only root, all capabilities dropped, `no-new-privileges`, a size-limited scratch tmpfs, CPU, memory and process limits, a wall-time kill, only that harness's own login volume, and a checkout mounted read-only (writable only for the council's writer); a checkout that contains your home directory or the Docker socket is refused. Host mode runs the harness under Seatbelt with an allow-listed environment, and checks at launch that a probe profile really denies reads and writes of a private file | `internal/runtime/docker`, `internal/runtime/coding` |
| A harness sends data to arbitrary hosts or reaches the daemon directly | Internal bridge network without a route out; the per-launch proxy allows only the harness's provider hosts on port 443 at public addresses, at most 16 tunnels of 15 minutes each, and a fixed list of gate routes; requests that do not match are refused | `internal/runtime/docker`, `internal/gate/councilproxy` |
| A workload or harness uses another task's lease, or one after the task ended | Leases are random, stored as SHA-256 only, expire after one hour at most, and are revoked on cancel, suspend and completion; a gate checks the lease, then the workload's grants, on every call; the council proxy accepts only the lease it was started for | `internal/gate/model`, `internal/gate/mcp`, `internal/controlplane`, `internal/gate/councilproxy` |
| A web page or another host reaches the daemon (cross-site writes, DNS rebinding) | Host allow-list (loopback names on the listen port plus `--allow-host`); same-origin checks on writes; `SameSite=Strict`, `Secure`, `HttpOnly` cookie; `X-Frame-Options: DENY`, `nosniff`, a content security policy; request body limits | `cmd/sablecourtd` |
| Guessing the operator token | 256 bits of randomness, a rate limit on `/login` (10 failures a minute per client, 1024 in all), constant-time comparison | `internal/operatortoken`, `cmd/sablecourtd` |
| Another local user reads the token | State directory `0700` and token file `0600`, owner checked, symlinks refused, one daemon per directory by lock | `internal/operatortoken` |
| A rogue local script approves its own request (invariant 1) | An approval needs a native macOS dialog showing the digest, or a signature from a paired device over the approval ID and digest; the dialog fails closed off macOS, on cancel and on timeout; a changed digest is refused. Device enrolment is closed (503) | `internal/approval`, `cmd/sablecourtd` |
| A leaked integration token is used on operator routes | Each token reaches only its own routes; the Shadewright token is refused elsewhere and operator tokens and leases are refused on it; the observer token is read-only on three routes | `internal/integrations`, `cmd/sablecourtd` |
| A workload runs up cost | Per-task and daily budgets with a reservation before each call, settled afterwards (the reserved estimate is charged when a response reports no usage); an exhausted budget is a 429 | `internal/gate/model` |
| A stolen cookie | It lasts 30 minutes and dies on daemon restart and on `POST /logout`, which invalidates every cookie | `cmd/sablecourtd` |
| Someone edits the audit log | A hash chain that `audit verify` recomputes; it detects edits and deletions of hashed rows that have a successor, and it has holes (below) | `internal/domain`, `internal/store` |

## Accepted exceptions

- Invariant 2 has one exception, accepted in ADR-0011: the three Sanctum harnesses (`claude`, `codex`, `agy`) use their own provider subscription login for inference. It covers that login only, kept in the harness's own volume. They get no API key, Git credential, database password or operator token. This inference does not pass the Model Gate, so the gate does not count its tokens or cost; the run, round and wall-time ceilings bound it.
- The lease crosses plain HTTP between the harness and its proxy, inside a per-launch network that nothing else joins. The amendment that records this is Proposed; the code works this way.
- Host mode gives weaker isolation than the container. ADR-0011 records that `codex` cannot run inside Seatbelt (its own sandbox is refused), so on the host it would have to rely on its own sandbox, which Sablecourt cannot verify.
- A process that runs as your user is inside the trust boundary and can read the token file.

## Known gaps

These have no control, or a control with a hole. Each names the tracking issue or ADR.

| Gap | What happens today | Tracked in |
| --- | --- | --- |
| Deleting the newest audit rows is not detected | Verification only checks rows that exist; it also skips rows with an empty hash, has no key and no outside copy of the newest hash, and almost every caller ignores a failed append | issue 10 |
| An approved approval can be reused | The gate does not mark it used or check its expiry when the action runs, so a task may repeat the approved action while its lease lives. The dialog path also accepts an empty expected digest, and the dialog shows only the first 220 characters of the payload | issue 9 |
| Approver identity is a label | `decidedBy` comes from the request body and is written to the audit row as sent | ADR-0014 |
| Sandbox-zone workloads may hold mutating `mcp:` tools | The zone rule covers `action:` and `delegate:` only | issue 9 |
| Device enrolment is closed | `pair/start` and `pair/complete` return 503; the enrolment flow with independent confirmation does not exist | issue 5 |
| Stolen session cookie cannot be revoked alone, and cookies are shared across ports on `localhost` | The cookie is stateless; only a restart or `POST /logout` ends it, for every browser | ADR-0014 |
| No authentication events in the audit chain | Sign-ins, failures and logouts leave no row | ADR-0014 |
| Leases are not bound to a channel and travel in the environment (host and generic container runs) | A copied lease works from anywhere that reaches a gate route until it expires | ADR-0014 |
| Integration tokens never expire or rotate | They are plain secrets in the keychain or in an optional `0600` JSON file | ADR-0014 |
| Host-mode sandbox is wider than needed | The Seatbelt profile exposes the shared Git object store and refs and wide read paths (reported, not reproduced) | issue 63 |
| `codex` in host mode is not refused | ADR-0011 says it stays refused until a test shows its own sandbox denies reads of the token and other logins; no code enforces that | ADR-0011 |
| Redaction is pattern-based | It covers private keys, `sc_task_` and `op_` tokens, Anthropic, OpenAI, GitHub, Google and AWS keys, bearer headers and `key = value` pairs with names like `password` or `api_key`; other secrets pass. The Model Gate does not redact request or response bodies | issue 8 |
| Parked endpoints are reachable | The GitHub webhook and A2A routes exist and are registered; they refuse requests unless their secret is set, but have no allow-list or scoping | issue 13 |

## Not built yet

[ADR-0014](adr/0014-identity-and-authentication.md) is Proposed. Its passkeys, server-side sessions, roles, per-launch channel-bound leases, mutual TLS, OIDC sign-in, signed audit heads and recovery commands close several gaps above if they are built. None of them is a protection today.

## In depth

- [Overview](overview.md), [identity and access](how-it-works/identity-and-access.md), [audit](how-it-works/audit.md), [approvals](how-it-works/approvals.md), [harness containers](how-it-works/harness-containers.md)
- [ADR-0003](adr/0003-hybrid-workload-isolation.md), [ADR-0004](adr/0004-in-house-capability-gates-three-doors.md), [ADR-0005](adr/0005-dual-path-human-in-the-loop.md), [ADR-0009](adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md), [ADR-0011](adr/0011-sanctum-scope-trust-and-cli-harnesses.md)
- Hard invariants: `AGENTS.md`. Reporting a vulnerability: `SECURITY.md`.
