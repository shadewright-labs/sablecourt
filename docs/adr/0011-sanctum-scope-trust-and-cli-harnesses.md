# ADR-0011: Sanctum scope, trust, and CLI harnesses

- Date: 2026-11-24
- Status: Accepted (2026-11-24; to be revised from what the first test runs show)

## Context

Issue #2 sets the build order and trust boundary for the user's workstation. Sanctum currently leaves operator bearer authentication optional. The coding adapter conditionally exports `SABLECOURT_MODEL_GATE_URL`. Its macOS profile broadly permits reads, its Linux wrapper mounts the host root read-only, and an inapplicable sandbox choice can fall through to an unwrapped launch. The adapter starts the harness under the daemon's user identity. These behaviors leave operator state reachable to a same-user process. [Daemon startup](../../cmd/sablecourtd/main.go), [coding adapter](../../internal/runtime/coding/coding.go).

## Decision

Build the local `sanctum` council with `claude`, `codex`, and `agy` first. Follow it with the Shadewright incident escalation and learning extension. Keep existing components and their regression coverage in the tree. Park feature work on Citadel, Kubernetes and PostgreSQL, remote access, A2A, GitHub integrations, upstream MCP, general workload orchestration, voice, device pairing, chat alerts, and live takeover. The council and learning decisions belong to issues #27 and #41. [ADR-0013](0013-bounded-council-rounds.md), [ADR-0012](0012-learning-evidence-and-expiry.md).

Trust the user, daemon, and host OS. Require loopback operator authentication in Sanctum. The daemon uses a random `operator.token` in its owner-only `0700` state directory. `sablectl` reads the owner-owned `0600` file and sends a bearer token. A same-origin `/login` page exchanges that token for a short-lived `HttpOnly`, `SameSite=Strict` session cookie, with same-origin checks on browser writes. Missing credentials, unsafe file ownership or permissions, and invalid or wrong-class credentials fail closed. Workload routes use scoped, short-lived task leases. Integration routes use their own scoped credentials. When enabled, the existing optional socket enforces the same credential boundary.

File permissions protect the token from other users. A process running as the same user can still reach loopback and read that user's files unless confined. An operator session may deny an approval or request confirmation. Approval requires independent human proof bound to the exact payload digest. Any enabled path for enrolling an approving device also requires independent human proof. Missing proof, a timeout, or a changed digest leaves the action blocked. Code publication still requires PR checks and human merge. [ADR-0005](0005-dual-path-human-in-the-loop.md), [ADR-0009](0009-sanctum-citadel-audit-chain-and-crypto-approvals.md).

Acceptance of this ADR would authorize a narrow exception to invariant 2 and the inference rule of ADR-0004. Only the three named Sanctum harnesses may use their own provider-managed subscription login for inference. This exception covers the credential and the provider-side identity only. Sablecourt task identities remain scoped and short-lived. The harness gets no raw provider API key, Git credential, database password, or operator token. In this mode omit the Model Gate URL and keep task lease authentication for permitted workload calls. Subscription inference runs outside Model Gate token and cost accounting. Run, round, and wall-time ceilings bound it. Whether each CLI can operate on its subscription login under these restrictions must be tested before enabling it. [ADR-0004](0004-in-house-capability-gates-three-doors.md).

Limit reads to the selected checkout, required runtime files, redacted task input, and that harness's own login. Restrict writes to private scratch and an explicitly assigned writer worktree.

## Alternatives considered

A Unix socket alone needs a browser bridge and still admits a same-user client. A second operator transport adds authentication plumbing without solving read isolation. A host-wide read mount with a deny list leaves credentials exposed when the list misses a location. A fixed token path outside the configured state directory would need separate ownership and lifecycle checks.

## Consequences

Build mandatory authentication first. Then test read isolation and process cancellation on every OS that enables these harnesses. Refuse launch when confinement or a ceiling cannot be enforced. Before enabling subscription inference, record the accepted exception in the invariant and verify that the harness cannot reach operator state, browser sessions, or unrelated credentials. Browser login adds a step, and the daemon cannot report subscription token or dollar usage.

## Amendment (2026-11-25)

Sanctum uses container mode for local councils once the Docker runtime supports it. The 2026-11-25 spike ran all three CLIs up to login. [Harnesses in containers](../harnesses.md#harnesses-in-containers).

Each harness runs in its own container as a non-root user with a read-only root filesystem, all capabilities dropped, and `no-new-privileges` set. The only mounts are a writable Sablecourt-owned named volume for that harness's state and login, the selected council checkout, and writable private scratch. Live checkouts and reviewer checkouts are read-only. The writer's own assigned worktree is the only writable checkout. Containers never run privileged and have no access to the host home, host CLI state, Keychain, or Docker socket.

The user logs in once per harness inside that harness's container using the device or URL flow. The login credential stays in the named volume. Sablecourt never copies a credential from the host. A harness without a login fails preflight before launch, and the error gives the user the login command for its container and volume. [Container login commands](../harnesses.md#summary-table-1).

An agent can read only its own CLI's provider login, from its own volume. For all three CLIs, that login is separate from the user's normal CLI login. The subscription login remains the only credential covered by the invariant 2 exception.

Harness containers join an internal network with no direct egress. An allow-list proxy lets each harness reach only its provider's hosts for login and inference. Access to the Sablecourt gate on the host requires either a proxy pinned to that gate or a per-workload Unix socket. The daemon keeps its TCP listener on loopback. Gate calls still require a scoped, short-lived task lease. [Network and gate reachability](../harnesses.md#network-allow-list-and-sablecourt-host-gate-reachability).

The `codex` CLI reached authentication, but its inner bubblewrap sandbox failed when invoked under the container restrictions with all capabilities dropped. Docker's default seccomp profile also blocked the user namespaces bubblewrap needs. This is the Linux counterpart of the nested Seatbelt failure on macOS. In container mode, the container is the isolation boundary for `codex`. [Codex inner sandbox](../harnesses.md#codex-inner-sandbox-inside-the-container).

### Host fallback

Build container mode first. Host mode is the weaker fallback for machines without a container runtime. It keeps separate harness directories under the Sablecourt state directory. Set `CLAUDE_CONFIG_DIR` to the absolute path to `harness/claude`, `CODEX_HOME` to the absolute path to `harness/codex`, and `HOME` for `agy` to the absolute path to `harness/agy`. When a login is missing, host preflight gives the command for the configured state directory. [Separate state directories](../harnesses.md#separate-state-directories).

On macOS, the Sablecourt Seatbelt profile confines `claude` and `agy` to their own state, assigned checkout, and private scratch, with the runtime and credential access they need. It denies access to the user's normal CLI state, other harness directories, and `operator.token`. The writer's assigned worktree is the only writable checkout.

`claude` keeps its subscription credential in the user's login Keychain under a service name derived from `CLAUDE_CONFIG_DIR`. The profile must allow access to that item, and another process running as the same user can still read it. The CLI still uses its cache under the user's `Library/Caches`. For `agy`, the `HOME` override breaks default Keychain lookup, so the CLI falls back to a credential file in its relocated state. Its child processes inherit the shifted `HOME`.

On macOS, the outer Seatbelt profile causes the `codex` inner sandbox to fail with `Operation not permitted` (exit `71`). Host mode runs `codex` under its own sandbox with `CODEX_HOME` set, in `workspace-write` for the writer and `read-only` otherwise. The default credential store is `auth.json` in `CODEX_HOME`. With no outer profile, Sablecourt cannot independently enforce filesystem read or write limits, including access to normal CLI state, other harness state, `operator.token`, browser sessions, and unrelated credentials. The outer restrictions on executable paths and macOS services also cease to apply. The daemon still strips the environment, assigns the worktree, enforces council run, round, and wall-time ceilings, redacts council posts, and limits task lease lifetime.

### Follow-up

- Extend the Docker runtime for named volumes, checkout and scratch mounts, per-harness commands and proxy allow-lists, closed stdin for headless runs, and an interactive TTY for login. Separate stdout from stderr and collect the final answer before container removal. [internal/runtime/docker/docker.go](../../internal/runtime/docker/docker.go), [runtime gaps from the spike](../harnesses.md#changes-needed-in-internalruntimedocker).
- Replace the Docker runtime's host-gateway assumption with the selected gate transport. The host-gateway address did not route from the internal network in the spike. Pass only a scoped, short-lived task lease for permitted gate calls and keep `SABLECOURT_TASK_TOKEN` out of logs, artifacts, and persistent state. Omit `SABLECOURT_MODEL_GATE_URL` for subscription runs and keep provider credentials in their own volumes. [internal/runtime/docker/docker.go](../../internal/runtime/docker/docker.go), [cmd/sablecourtd/host.go](../../cmd/sablecourtd/host.go).
- Map the council driver's `live` and `ref` modes to read-only mounts. In `writer` mode, mount the pinned review checkout read-only for reviewers and the assigned writer worktree read-write for the writer. [internal/conclave/council_driver.go](../../internal/conclave/council_driver.go), [internal/runtime/coding/council.go](../../internal/runtime/coding/council.go).
- Wire the Docker harness runtime into Sanctum and add the login preflight for each harness volume. Return the matching one-time container login command when preflight fails for a missing login. [cmd/sablecourtd/main.go](../../cmd/sablecourtd/main.go).
- Build the three harness images from pinned base images and keep them local. [Spike Dockerfiles](../harnesses.md#dockerfiles).

### Open checks

- Choose between the pinned gate proxy and the per-workload Unix socket. The spike verified proxy forwarding to a loopback test server. Verify the chosen transport against the Sablecourt gate with task lease authentication.
- Run `codex` after login and verify tool execution under the container restrictions. Check whether the Landlock fallback works. [Codex inner sandbox](../harnesses.md#codex-inner-sandbox-inside-the-container).
- Host-mode `codex` is refused until a test shows that its own sandbox, for the pinned `codex` version, denies reading `operator.token`, the other harness directories and the normal CLI state. Record the result here. If the policy cannot be confirmed, host mode runs without `codex`.
- The user checks whether each provider's terms allow use of a subscription login from a container on the same machine.
- Measure image sizes and repeat startup measurements after login. In the unauthenticated spike, first output took `365.8 ms` for `claude`, `175.6 ms` cold and `123.3 ms` warm for `codex`, and `204.7 ms` for `agy`. [Container startup measurements](../harnesses.md#summary-table-1).

## Amendment (2026-11-25): proxy to daemon hop

Status of this amendment: Proposed.

This records how a council harness reaches the daemon gate now that the per-launch proxy is in the tree. If accepted, it closes the first open check above (choose between the pinned gate proxy and the per-workload Unix socket): both are built, and a launch probe picks one. The production launcher in the coding adapter uses the proxy (issue 78) and starts the harness only after the proxy is provisioned and verified ready; this amendment does not change that wiring. [council proxy](../../internal/gate/councilproxy), [daemon host routing](../../cmd/sablecourtd/host.go).

Each launch gets its own internal network and its own proxy container. The harness joins only the internal network. The proxy is the only other member of that network and the only container with egress. The network is created with the isolated bridge gateway mode for IPv4 and IPv6, which needs Docker Engine 28.0 or newer. The daemon reads the network back and refuses to publish an endpoint unless the engine applied that mode and assigned no gateway.

The proxy reaches the daemon gate by one of two transports, chosen by a probe before the proxy is created:

- **Native Linux: per-launch Unix socket.** The daemon listens on a socket inside a fresh 0700 directory that it owns, and bind-mounts only that socket into the proxy. Every ancestor of the socket root must be owned by root or the daemon user and must not be replaceable by anyone else; a shared sticky directory such as a temporary directory is accepted only when the entry beneath it belongs to a trusted user. The mount source is the resolved path and is checked again just before the mount. Closing the listener does not unlink by pathname, and cleanup goes through a directory handle opened and verified at creation. The proxy never falls back to TCP at run time.
- **Docker Desktop: per-launch pinned TLS 1.3.** A host Unix socket cannot be connected from a container there, so the daemon serves the gate on a loopback port over TLS 1.3 with a fresh P-256 key. The proxy receives the SHA-256 digest of that public key as an argument and rejects any other peer before it writes a byte of HTTP. Session resumption is off. The certificate is reissued with the same key at half its lifetime so the pin stays valid for the life of the lease.

The probe fails closed. A Docker error, an accept error on the probe listener, or a drain marker that the daemon did not consume is an error, never a result that selects TLS.

Gate requests are dispatched to the gate handler in process. No network listener serves the council virtual host, so a client that selects that authority on the plain loopback listener gets a refusal before any body is read. The proxy and the per-launch gate each check the task lease, a fixed route allow-list, and the request size before the daemon's own gate routes run.

Limits on this hop:

- The per-launch gate accepts at most 32 connections, 2 MiB request bodies, and 30 seconds for reading or writing a request.
- The proxy allows at most 16 concurrent provider tunnels, so tunnels cannot take every listener slot from gate calls. A tunnel lasts at most 15 minutes. It reaches only the provider hosts for that harness, on port 443, at public addresses.
- The daemon's own servers cap connections at 256, give request headers 5 seconds, close idle connections after 60 seconds, and read a whole request within 60 seconds. They set no write deadline, so a streaming response is not cut.

Stated limit: the hop from the harness to the proxy is plain HTTP inside the per-launch internal network, so the lease crosses it unencrypted. This is accepted because the proxy is the only other member of that network and nothing else can join it during the launch. It stops being acceptable if the network ever gains another member or is shared between launches, and a change like that needs a new amendment that serves the proxy over TLS as well.

## Summary

- Accept the two-milestone scope and retained components to unblock #2.
- Accept mandatory loopback authentication and independent confirmation for approvals and approving-device enrollment to unblock #2 and operator acceptance in #41.
- Accept the invariant 2 exception for the three CLI subscription logins, with a separate container and login volume per harness and finite subscription run limits, to unblock #2 and #27.
- Build container mode first for local councils. The container confines `codex` in this mode. Keep host mode as the weaker fallback for machines without a container runtime.
