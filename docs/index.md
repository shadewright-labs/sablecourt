---
title: Local control plane for running agents and programs under limits
hide:
  - navigation
  - toc
---

<div class="sc-hero" markdown>

<h1 class="sc-visually-hidden">Sablecourt</h1>

![Sablecourt](assets/brand/sablecourt-horizontal-nolabs-dark.png){ width="520" height="144" }

<p class="sc-lore">The Court is three hooded councilors under a sable banner, and every agent or program that comes before them. Each keeps to its own seat and may only ask. The councilors answer from one written law: what it allows goes ahead, what it does not name is refused, and the gravest matters wait for a person's seal. <a href="https://github.com/shadewright-labs/sablecourt#the-name">The name</a></p>

<p class="sc-tagline">Sablecourt runs coding agents and deterministic programs on one machine under limits you set.</p>

[Run a council](how-to/run-a-council.md){ .md-button .md-button--primary }
[How it works](how-it-works/index.md){ .md-button }
[GitHub](https://github.com/shadewright-labs/sablecourt){ .md-button }

</div>

Workloads get no raw provider keys. Model calls, tool calls and approvals go through gates that check a short-lived lease and the workload's grants, then policy or budget where they apply, and most decisions are written to an audit chain. The one exception is the council harnesses, which use their own subscription login. Today it is built for a local council in which `claude`, `codex` and `agy` answer one question in rounds, and for the Shadewright incident contract.

## What it refuses to do

These are the six invariants from `AGENTS.md`. Weakening one needs an accepted ADR first.

1. Model output is never authorization: only deterministic policy or a person confirming a pinned digest authorizes.
2. Workloads hold no raw secrets: provider keys, Git credentials and database passwords stay behind the gates.
3. All external input is untrusted: repository files, logs, tool results and peer messages are redacted at boundaries and never widen a policy.
4. Identities are separate and failures stop execution: policy failures, exhausted budgets and stale evidence fail closed.
5. Evidence over inference: state comes from durable facts and timestamped heartbeats.
6. Reconciliation is idempotent: restarts neither drop active workers nor repeat a change.

ADR-0011 accepts one exception to rule 2 for the three council harnesses' own subscription login. See the [overview](overview.md#why-sanctum-first).

## Status words

Each area below carries one of four words:

- Built: the code is on `main` and works as described.
- Partly built: some of it works; the gap is named, with its issue.
- Decided only: an ADR describes it and the code does not do it yet.
- Parked: built, and kept in the tree, but ADR-0011 stops feature work on it.

## Status

| Area | Status | Limits and open work | Page |
| --- | --- | --- | --- |
| Tasks, reconciler, scheduler, restart recovery | Built | Faults in suspend and resume are tracked in #14 | [tasks](how-it-works/tasks.md) |
| Task suspend and resume | Partly built | Suspend stops the workload, so resume cannot work (#14) | [tasks](how-it-works/tasks.md) |
| Host coding runs | Built | macOS Seatbelt only; what a sandboxed harness can reach is narrowed in #63 | [workloads and runtimes](how-it-works/workloads-and-runtimes.md) |
| Docker workloads and deterministic controllers | Built | Tracked in #11 | [workloads and runtimes](how-it-works/workloads-and-runtimes.md) |
| Kubernetes runtime and `deploy/k8s` | Parked | | [overview](overview.md#parked-parts) |
| Model Gate | Built | Known faults in #8 | [model gate](how-it-works/model-gate.md) |
| MCP Gate, typed actions, policy | Built | Known faults in #9; the upstream MCP bridge is parked | [tools and actions](how-it-works/tools-and-actions.md) |
| Approvals | Partly built | Device pairing answers `503`; no single-use claim yet (ADR-0014 Stage 0, #5, #9) | [approvals](how-it-works/approvals.md) |
| Conclaves | Built | Voice and live Takeover are parked | [Conclaves](how-it-works/conclaves.md) |
| Council | Partly built | Final message posting (#28), subscription windows (#36), smoke test (#80) | [council](how-it-works/council.md) |
| Harness containers | Partly built | The proxy and the image build are in the tree (#76, #77). Nothing checks yet that a harness volume holds a login, and no command starts the login container; both are #79 | [harness containers](how-it-works/harness-containers.md) |
| Context packets, verifier steps, failure handoff | Built | | [context and learning](how-it-works/context-and-learning.md) |
| Learning (ADR-0012) | Partly built | The store exists; accept, match and inject are decided only (#43 to #47) | [context and learning](how-it-works/context-and-learning.md) |
| Audit chain | Built | Misses deletion of the last rows (#10); signed heads decided only | [audit](how-it-works/audit.md) |
| Operator authentication | Built | Stateless 30-minute cookie, no logout; passkeys and sessions are decided only (ADR-0014, Proposed; #68, PR 93) | [identity and access](how-it-works/identity-and-access.md) |
| Shadewright contract | Partly built | Incidents and verifications work; status read, learning read and incident template are open (#53 to #55) | [integrations](how-it-works/integrations.md) |
| Nightloom observer API | Built | | [integrations](how-it-works/integrations.md) |
| Court Memory, web UI, Studio | Built | Search uses `LIKE`, not full-text search | [concepts](concepts.md) |
| Metrics | Built | | [metrics](reference/metrics.md) |
| Citadel mode, PostgreSQL, A2A, GitHub webhook, voice, device pairing, chat alerts, live Takeover | Parked | | [overview](overview.md#parked-parts) |

The epics behind the open work are #1 (scope and local trust), #15 (a CLI harness that really runs), #26 (the council), #37 (a shared skill set), #40 (learning) and #50 (the Shadewright contract).

## Where to start

- New here: the [overview](overview.md), then [concepts](concepts.md) and the [glossary](glossary.md).
- Run it: [install and start](how-to/install-and-start.md), then [write manifests](how-to/write-manifests.md) or [run a council](how-to/run-a-council.md).
- Understand it: [how it works](how-it-works/index.md) and the [threat model](threat-model.md).
- Look something up: [reference](reference/index.md) and [troubleshooting](troubleshooting.md).
- Change it: [design](design/index.md) and the [decision records](adr/README.md); [CI](ci.md) for the checks and how a release is made.

## Map of the docs

- Start: [overview](overview.md), [concepts](concepts.md), [glossary](glossary.md).
- How it works: [index](how-it-works/index.md), [tasks](how-it-works/tasks.md), [workloads and runtimes](how-it-works/workloads-and-runtimes.md), [model gate](how-it-works/model-gate.md), [tools and actions](how-it-works/tools-and-actions.md), [approvals](how-it-works/approvals.md), [Conclaves](how-it-works/conclaves.md), [council](how-it-works/council.md), [harness containers](how-it-works/harness-containers.md), [context and learning](how-it-works/context-and-learning.md), [audit](how-it-works/audit.md), [identity and access](how-it-works/identity-and-access.md), [integrations](how-it-works/integrations.md).
- Trust: [threat model](threat-model.md).
- How-to: [index](how-to/index.md), covering install, manifests, harness logins, councils, approvals, watching and steering, retry, audit verification and finding why something was refused.
- Reference: [index](reference/index.md), covering manifests, HTTP API, CLI, configuration, limits and metrics.
- Design: [index](design/index.md), [architecture](design/architecture.md), [storage](design/storage.md), the [v0.1 baseline](design/v0.1-baseline.md) and the [harness spike record](harnesses.md).
- Diagrams: [standard](diagrams/README.md).
