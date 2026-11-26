<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/brand/sablecourt-horizontal-nolabs-dark.png">
    <img src="docs/assets/brand/sablecourt-horizontal-nolabs-light.png" alt="Sablecourt" width="520">
  </picture>
</h1>

<p align="center">
  A control plane that runs coding agents and deterministic programs on one machine under limits you set. Workloads get a short-lived lease instead of provider keys, their model calls, tool calls and approvals go through gates that check the lease, the grant and then the budget or the policy, and most decisions are written to a hash-chained audit log.
</p>

<p align="center">
  <a href="https://github.com/shadewright-labs/sablecourt/actions/workflows/ci.yml?query=branch%3Amain"><img src="https://github.com/shadewright-labs/sablecourt/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI status on main"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/shadewright-labs/sablecourt" alt="Go version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="License: Apache-2.0"></a>
</p>

> [!WARNING]
> Nothing is released: there are no tags, binaries or images, and the source is public so you can build it and read it. It runs as one user on one machine and listens on loopback. Host coding runs and the approval dialog work on macOS only. Citadel mode, the Kubernetes runtime, voice and live Takeover are in the tree but parked, and the audit chain is not anchored outside its database, so deleting the last rows goes unnoticed. [SECURITY.md](SECURITY.md) states what the security model holds today.

## What it does

1. **Launches** the workloads your manifests declare: a coding CLI on a Git worktree under the macOS Seatbelt sandbox, and long-lived agents and deterministic controllers in Docker containers. Each launched task gets its own lease, revoked when the task ends.
2. **Gates** model calls: the Model Gate checks the lease, the grant and the budget, then forwards the call to OpenAI, Anthropic, Gemini or a local OpenAI-compatible server with the key it holds. Deterministic controllers are refused there.
3. **Checks** tools and actions: the MCP Gate serves the tools a workload is granted and a closed set of typed actions. Deterministic policy decides each call: `allow`, `approve` or `deny`, and no matching rule means deny.
4. **Asks you** before an action whose effect is `approve`: the payload is pinned by its SHA-256 digest and you confirm it in a native macOS dialog. On other systems approving fails closed and you can only deny.
5. **Convenes** councils: one question to `claude`, `codex` and `agy`, which answer in rounds on your checkout, each in its own container when a Docker engine is configured.
6. **Records** policy decisions, approvals, task submission and cancellation, and most model calls in an audit chain that `sablectl audit verify` checks.

A model's answer is never authorization: only deterministic policy, or a person confirming a pinned digest, lets an action run.

[![Callers reach the sablecourtd control plane; it launches workloads with leases; workloads reach the Model Gate, MCP Gate and Approval Queue, which reach providers and the Git host.](docs/diagrams/system-context.svg)](docs/diagrams/system-context.svg)

## State of the areas

The full table, with the open issues behind each gap, is on the [docs home page](docs/index.md#status).

| Area | State | Limits |
| --- | --- | --- |
| Tasks, reconciler, scheduler | Built | Suspend stops the workload, so resume does not work yet |
| Host coding runs | Built | macOS Seatbelt only |
| Docker workloads and deterministic controllers | Built | |
| Model Gate, MCP Gate, typed actions, policy | Built | The upstream MCP bridge is parked |
| Approvals | Partly built | Device pairing answers `503`; approving needs the macOS dialog |
| Conclaves | Built | Voice and live Takeover are parked |
| Council | Partly built | The room does not post a harness's final message by itself |
| Learning | Partly built | Old `learning:` entries still reach context packets; the reviewed learning record is stored but nothing accepts or injects it |
| Audit chain | Built | Misses deletion of the last rows; not anchored outside the database |
| Shadewright incident intake | Partly built | Incidents and verification reports are accepted; the caller cannot read a status back |
| Court Memory search | Built | `LIKE` matching, no full-text search |
| Citadel mode, Kubernetes, PostgreSQL, A2A, GitHub webhook, chat alerts | Parked | |

## Install

Nothing is released, so build it from source. You need the Go version named in [go.mod](go.mod), macOS or Linux, and for councils a macOS host with Docker Engine 28.0 or newer.

```bash
git clone https://github.com/shadewright-labs/sablecourt.git
cd sablecourt
make install                                       # sablecourtd and sablectl into ~/.local/bin
sablecourtd --manifests=examples/local-demo.yaml   # listens on 127.0.0.1:7432
```

The daemon keeps its state, including the operator token, in `$SABLECOURT_STATE_DIR` or `~/.local/share/sablecourt`. Open the web UI at `http://127.0.0.1:7432` and sign in with that token, or use `sablectl`, which reads the token from the same directory:

```bash
sablectl workload list
sablectl conclave open feature-conclave --title="Fix retry timeout"
sablectl conclave list
```

A council (`sablectl council "<question>"`) needs more setup: the harness images and logins in the [council guide](docs/how-to/run-a-council.md).

## Guides

- [Overview](docs/overview.md): what Sablecourt is and is not, why it starts on one machine, and the parked parts.
- [Concepts](docs/concepts.md) and the [glossary](docs/glossary.md): workloads, leases, gates, Conclaves and the court vocabulary.
- [How it works](docs/how-it-works/index.md): the path of a request and where each invariant is enforced.
- [Run a council](docs/how-to/run-a-council.md) and [log in to harnesses](docs/how-to/log-in-harnesses.md).
- [Review an approval](docs/how-to/review-an-approval.md): read the pinned payload, then approve or deny.
- [Watch and steer](docs/how-to/watch-and-steer.md) and [retry with a handoff](docs/how-to/retry-with-handoff.md).
- [Reference](docs/reference/index.md): manifests, HTTP API, CLI, configuration, limits and metrics, checked against the code by tests.
- [Decision records](docs/adr/README.md) and the [diagram standard](docs/diagrams/README.md).

## Repositories

| Repository | Contents |
| --- | --- |
| [sablecourt](https://github.com/shadewright-labs/sablecourt) | The daemon `sablecourtd`, the CLI `sablectl` and these docs |
| [shadewright](https://github.com/shadewright-labs/shadewright) | The Kubernetes remediation operator. Its one-way handoff of incidents to Sablecourt is decided in its ADR-0049 and not built yet |
| [shadewright-labs.github.io](https://github.com/shadewright-labs/shadewright-labs.github.io) | The organisation's site, [shadewrightlabs.com](https://shadewrightlabs.com) |

## Development

```bash
make test         # go test -race ./...
make vet          # go vet ./...
make build        # bin/sablecourtd and bin/sablectl
make docs         # regenerate the generated blocks of docs/reference/cli.md
make docs-check   # fail when a reference page no longer matches the code
make agent-image  # the built-in agent container image
make demo         # run the daemon with examples/local-demo.yaml
```

On macOS, `make service-install` runs the daemon as a launchd job and `make service-uninstall` removes it. CI runs two jobs on every pull request: `verify` (vet, race tests, a short fuzz run, the binaries and the example manifests) and `security` (govulncheck, gosec and gitleaks).

Contributor and coding-agent guide: [AGENTS.md](AGENTS.md).

## The name

*Sable* is heraldic black, the colour of the Shadewright Labs line. A *court* is both a council of separate houses and a tribunal: here, agents and programs of different kinds working side by side under one set of rules, enforced by the policy, the budgets and the gates.

## License

Apache License 2.0, see [LICENSE](LICENSE).
