# Overview

What Sablecourt is, what it is not, what is parked, and how far it has got. For the one-paragraph version and the status table, see the [home page](index.md).

## What it is

A local control plane for running agents and programs under limits. You write manifests that declare workloads, policies and budgets. The daemon `sablecourtd` launches those workloads, gives each launched task its own short-lived lease, and puts model calls, tool calls and approvals behind gates. The Model Gate checks the lease, the grant and the budget; the MCP Gate checks the lease, the grant and the policy. Most outcomes are recorded in an audit chain. Some are not: an upstream error status that the Model Gate passes through leaves no audit row, and a normal dispatch of a task is recorded as a task event, not as an audit row. You drive it with `sablectl` or the web UI.

The first use is the local council: you ask one question and `claude`, `codex` and `agy` answer in rounds on your own checkout, each in its own container when a Docker engine is configured. The second is the Shadewright incident escalation with learning. Both are decided in [ADR-0011](adr/0011-sanctum-scope-trust-and-cli-harnesses.md).

## What it is not

- Not a process watcher. [Nightloom](adr/0007-strict-boundary-with-nightloom.md) watches sessions you start by hand and launches nothing. Sablecourt manages the workloads it launches and does not discover other processes.
- Not a model that authorizes. A model only proposes; deterministic policy or a person confirming a pinned digest authorizes (invariant 1).
- Not a hosted service. Sanctum listens on `127.0.0.1:7432` and trusts you, the daemon and the host OS. Reaching it from elsewhere is parked.

## Why Sanctum first

The daemon runs as your user next to coding agents that run as your user. ADR-0011 therefore starts with trust on one machine: a random operator token in a state directory that must be owned by you with mode 0700, a login page that turns it into a 30-minute session cookie, scoped tokens for integrations, and leases for workloads. The state directory is `$SABLECOURT_STATE_DIR` or `$HOME/.local/share/sablecourt`. A process running as you can still read your files unless it is confined, so the harnesses run in containers or the macOS sandbox.

ADR-0011 also accepts one exception to the rule that workloads hold no raw secrets: the three council harnesses use their own subscription login for inference, kept in a volume of their own container. They get no provider API key, Git credential or operator token. This inference does not pass through the Model Gate, so its tokens and cost are not counted there; run, round and wall-time ceilings bound it instead.

## Parked parts

ADR-0011 stops feature work on the parts below. The code and its tests stay in the tree and some of it still runs, so the old README lists them as capabilities. Do not rely on them.

| Part | Where in the tree | What it means now |
| --- | --- | --- |
| Citadel mode | `--mode=citadel`, `deploy/k8s` | Built. Turns off the host coding adapter and councils, defaults the runtime to Kubernetes and the listen address to `0.0.0.0:7432`. |
| Kubernetes runtime | `internal/runtime/k8s` | Built, used with `--runtime=k8s`. |
| PostgreSQL | `internal/store/postgres` | Built, used when `SABLECOURT_POSTGRES_DSN` is set. It has fewer migrations than SQLite and no learning store. |
| A2A broker | `internal/a2a` | The `POST /a2a` route is registered and authenticates with its own secret. |
| GitHub webhook | `internal/webhook` | The `POST /api/v1/hooks/github` route is registered and checks a signing secret. |
| Upstream MCP bridge | `internal/gate/mcp/upstream.go` | Built, off unless a workload lists `mcpServers`. |
| Voice | `internal/voice` | Speech-to-text and read-back routes exist. |
| Device pairing | `internal/approval/device.go` | The pairing routes answer `503`. |
| Chat alerts | `internal/notify` | Outbound alerts exist. |
| Live Takeover | `internal/attach` | Exclusive input to a task exists. |

## Requirements

- The Go version named in `go.mod`, to build with `make install`.
- macOS or Linux. The host coding sandbox, used for `coding-agent` workloads outside councils, is macOS Seatbelt only; on Linux the daemon disables the host coding adapter. Host mode is unsupported: the [ADR-0011 amendment of 2026-11-27](adr/0011-sanctum-scope-trust-and-cli-harnesses.md#amendment-2026-11-27-container-only-harnesses-for-010) makes harness containers the only path for 0.1.0, and [issue 181](https://github.com/shadewright-labs/sablecourt/issues/181) moves coding tasks there.
- For councils, a Docker engine at version 28.0 or newer. The per-launch network needs its isolated gateway mode, and the daemon refuses to launch when the engine does not apply it. The harness images are built locally for `linux/arm64`.

## How it sits next to the other products

Nightloom reads a snapshot and stream of managed work through a read-only observer token. Sablecourt marks the sessions it launches with `SABLECOURT_MANAGED=1` in the environment and, in coding worktrees, the file `.sablecourt-session.json`, so managed sessions can be told from manual ones. Shadewright sends incidents and verification reports through a scoped token. The observer token opens only the observer and scry read routes, and the Shadewright token is rejected on other routes. See [integrations](how-it-works/integrations.md).

## Status

The [status table](index.md#status) lists every area. [ADR-0011](adr/0011-sanctum-scope-trust-and-cli-harnesses.md) is Accepted, but its second amendment (the hop from the proxy to the daemon) is Proposed, and [ADR-0014](adr/0014-identity-and-authentication.md) is Proposed. Neither is a decision in force.

## Where to go next

- To try it: [install and start](how-to/install-and-start.md).
- For the nouns: [concepts](concepts.md) and the [glossary](glossary.md).
- For the flow: [how it works](how-it-works/index.md).
