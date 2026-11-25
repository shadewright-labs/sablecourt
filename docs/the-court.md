# The court

Sablecourt names its parts after a court. This page explains the convention, lists every court term the product uses and where you meet it, and says why each manifest kind and each option has the name it has. Plain definitions are in the [glossary](glossary.md), and types and defaults are in the [manifest reference](reference/manifests.md).

Status: built, apart from the items under [Not built yet](#not-built-yet).

## The convention

Sablecourt is the part of Shadewright Labs that acts. The strategy note of 23 November 2026 gives the three products a role each: Nightloom is the Watchman, Shadewright is the Craftsman and Sablecourt is the Court. The Watchman only observes, the Craftsman repairs clusters through reviewed pull requests, and the Court is the one that dispatches declared work and holds it to rules.

In that picture:

- The operator is the person the court answers to. There is one today.
- The daemon (`sablecourtd`) is the court itself. It reads the manifests, launches work, keeps the record and is the only place where the rules are applied.
- A Conclave is a room the court convenes. Its members, the Vassals, are workloads that each hold a role in it. A Council is a Conclave with a fixed panel that answers one question.
- Wards limit the room, a Decree is the operator speaking into it, and Relics are the files its members hand to one another.
- The gates are the doors every request passes through. The audit chain is the court's record.

The court fits the product because the product is about authority. Workloads do not talk to each other or to a provider on their own: a request goes to the court, the court checks it against a declared rule and writes down what it decided. The vocabulary of a court (who may speak, who may act, who must ask, what the record says) names those relationships without borrowing words from another tool.

The brand notes carry the same idea in a picture. The mark is a shield with three hooded councilors, a symbol of quorum and policy oversight, and the brand book asks for a voice that reads like a tribunal recording verifiable facts: counts, digests and limits before adjectives. The notes call the set of terms the dark-elven domain lexicon. They give each term its technical meaning. They do not record a separate etymology for each word, and this page does not invent one. Where a name has an origin in the notes or an ADR it is given. Otherwise the column says what the ordinary English word means, which is the reading the convention relies on.

### What the metaphor buys

- A short name for each role. "Vassal" says a member that serves a room and answers to it, which "agent" does not.
- Terms that cannot be confused with a provider's own words. A "session" or an "approval" means something different in every tool, and a Ward is a Ward only here.
- A consistent voice. A Ward trips, a Decree is issued, a Relic is put. The verbs are specific.

### Where it stops

- Only the terms in the table below are court terms. Everything else is plain: `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule`, task, lease, harness, gate, audit chain, approval.
- The metaphor does not change how anything behaves. A Ward is a counter with a limit, not a judge. The court has no discretion: the default policy is deny and the rules are the manifests.
- It is not complete. There is no court term for a task or a lease on purpose, because the plain word is the clearer one.
- Some terms are in the code only as labels in the web UI (Houses, Tribunal, Cockpit). They are listed below, and the CLI, the API and the manifests do not use them.
- The decided-but-not-built terms are kept out of the table. They are under [Not built yet](#not-built-yet).

![The court at a glance: an operator outside the court sends a Decree to a Conclave of Vassals with Wards and Relics, inside the court daemon. The Vassals reach the Model Gate and the MCP Gate, which reach a model provider, Court Memory and a Git host.](diagrams/court-at-a-glance.svg "Open full size")

The diagram shows who is who and who talks to whom. The operator speaks to a Conclave with a Decree. Inside the daemon, the Vassals of the Conclave message each other (only along the pairs their manifest allows), and they put and get Relics. A Vassal reaches the Model Gate for inference and the MCP Gate for tools and actions, always through the daemon and with its task lease. The Model Gate forwards to the provider. The MCP Gate runs tools, reads and writes Court Memory through the `memory.*` tools, and runs allowed actions such as opening a pull request on the Git host. A tripped Ward pauses the Conclave, and it waits for the operator. The Wards, the Relics and the room itself are shown as parts of the Conclave because they belong to one room.

## Court terms

"Where the name comes from" gives the source in the notes or an ADR where there is one. Otherwise it says what the word means in ordinary English. "Where it appears" lists the places in the product that use the name exactly.

| Term | In plain words | Where the name comes from | Where it appears |
| --- | --- | --- | --- |
| The Court | The product as a whole: the daemon and everything it governs. | The strategy note gives Sablecourt the role "The Court", next to the Watchman (Nightloom) and the Craftsman (Shadewright). | Web UI: the title of the top-bar logo is "Sablecourt · The Court". |
| Conclave | A room where several workloads work on one topic under limits. | The brand notes list it as part of the dark-elven lexicon. A conclave is a closed meeting, and ADR-0008 describes a closed room whose members talk only through the daemon. | Manifest kind `Conclave`. Routes `/api/v1/conclaves`, `/api/v1/conclaves/{id}` and its `close`, `decree`, `relics`, `stream` and `voice-decree` sub-routes, and the `/gate/conclave/{id}/...` routes the members use. CLI `sablectl conclave` (`list`, `open`, `decree`, `inspect`, `watch`, `relic`, `close`). Tools the members call through the MCP Gate: `conclave.post` and `conclave.read`. Tools of `sablectl mcp-server`: `sablecourt_open_conclave`, `sablecourt_inspect_conclave`. Web UI pages `/conclaves` and `/conclaves/{id}`, the nav label "Conclaves", the button "Convene Conclave". |
| Council | A Conclave of `claude`, `codex` and `agy` (or a subset) that answers one question in bounded rounds. | ADR-0013 names it. No note records why the word was chosen. The brand mark has three councilors and the default panel has three harnesses, but no note says one was picked for the other. | CLI `sablectl council`. Route `POST /api/v1/councils`. The `sablecourt_council_convene` tool of `sablectl mcp-server`. The built-in workload names `claude`, `codex` and `agy` are reserved for it. |
| Vassal | A member of a Conclave: a role bound to a workload. | The brand notes list it as part of the lexicon. A vassal is one who serves and answers to a higher authority, here the room and the operator. | Manifest field `spec.vassals` of a Conclave. Web UI heading "Vassals" on the Conclave page. The decision note of 23 November 2026 also uses the plural "Vassals". |
| Ward | A limit on a Conclave that pauses it when reached. | The brand notes list it as the "automated safety breaker" of a Conclave. A ward is a protective barrier, and ADR-0008 calls them deterministic loop guards. | Manifest field `spec.wards`. Conclave state `input-required` with a ward reason. Audit action `conclave.ward` with outcome `paused`, and the alert event `conclave.ward_paused`. |
| Decree | A message from the operator to a Conclave. It steers the room and can resume a paused one. | Brand notes: live operator intervention in a running Conclave. A decree is an authoritative order, and every message the operator posts to a Conclave is recorded as one. | CLI `sablectl conclave decree`, with the `--decree` flag of `sablectl conclave open`. Routes `POST /api/v1/conclaves/{id}/decree` and `.../voice-decree`. Web UI route `POST /ui/conclaves/{id}/decree`. Tool `sablecourt_conclave_decree` of `sablectl mcp-server`. Message kind `decree`. |
| Relic | A named, redacted file that members of a Conclave hand to one another. | Brand notes: a named, redacted artifact published by a Vassal for the others. A relic is an object kept and passed down, and the room passes these by reference. | CLI `sablectl conclave relic put` and `get`. Routes `POST /api/v1/conclaves/{id}/relics` and `GET .../relics/{name}`, and the `/gate/conclave/{id}/relics` routes. MCP tools `conclave.relic_put` and `conclave.relic_get`. Web UI heading "Relics" and the "Upload Relic" form. A council writes its closing digest as a Relic named `digest`. |
| Scry | A live, read-only, redacted view of a running task's output. | Brand notes: live read-only streaming of a running agent's output. To scry is to watch something from afar, without touching it. | CLI `sablectl task scry`. Route `GET /api/v1/tasks/{id}/scry`. Web UI task page, heading "Live console (Scry / Takeover)". |
| Takeover | Exclusive input to a running task's `stdin`, with an audit record. | Brand notes: exclusive operator attachment to a running task, logged automatically. To take over is to take the controls. | CLI `sablectl task takeover`. Route `POST /api/v1/tasks/{id}/takeover`, with `/stdin` and `/detach`. Web UI "Takeover" button on the task page. The [glossary](glossary.md) calls live Takeover parked: the code exists and ADR-0011 stopped further work on it. |
| Sanctum | The default mode: one user, one machine, loopback only. | Brand notes: the personal or workstation mode. A sanctum is a private, protected place. | `sablecourtd --mode=sanctum` (the default; `local` is an alias). Also in the launchd job written by `sablectl service install`, and in the error "council opening requires Sanctum". |
| Citadel | The Kubernetes mode. Built and parked by ADR-0011. | Brand notes: the enterprise Kubernetes mode. A citadel is the fortified core of a town. | `sablecourtd --mode=citadel` (`enterprise` is an alias). Log line "citadel mode active". Councils are turned off in this mode. |
| Court Memory | The key-value store the court keeps across tasks and Conclaves. Not a place for secrets. | Brand notes list it with the Conclave terms. It is the court's own record of what it has been told. | Web UI page `/memory`, title and heading "Court Memory", nav label "Memory". CLI `sablectl memory` (`list`, `set`, `delete`). Routes `/api/v1/memory`. MCP tools `memory.get`, `memory.put`, `memory.search` and `memory.delete`. Store table `court_memory`. |
| Houses | The web UI's name for the declared workloads. | UI label only. No note explains it. A house is a family or household under the court. | Web UI home page: panel "Houses (Workloads)" and nav item "Declared Houses". Never in a manifest, the CLI or the API. |
| Tribunal | The web UI's name for the audit view and the approval panel. | The brand mark is called the Tribunal Shield, and the brand book asks for a voice that records facts as a tribunal does. | Web UI: page title "Audit Tribunal" on `/audit`, panel "Tribunal · Approvals" on the home page. Never in the CLI or the API. |
| Cockpit | The web UI's home page. | UI label only. No note explains it. A plain word, not a court word. | Web UI nav label "Cockpit" and the palette entry "Cockpit Dashboard". |
| Studio | The web UI page where you define an agent, a workflow or a Conclave in the browser. | UI label only. A plain word. | Web UI route `/studio`, nav label "Studio". |
| Convene | The verb for opening a Conclave or a Council. | A conclave or council is convened. Used in command text. | Short text of `sablectl conclave open` and `sablectl council`, and the web UI button "Convene Conclave". |

Two points on the table.

1. The names in manifests, routes and commands are stable: `Conclave`, `vassals`, `wards`, `decree`, `relic`, `scry`, `takeover`. The UI labels (Houses, Tribunal, Cockpit, Studio) are labels only, and the label for a workload is "workload" everywhere else.
2. A few UI strings still say things the code no longer does. The command palette lists "Court Memory (FTS5)", but memory search uses SQL `LIKE`, not full-text search. The page text of this table follows the code.

## Manifest kinds and their options

A manifest is a YAML document with `apiVersion: sablecourt.io/v1alpha1`, a `kind` and a `metadata.name`. The loader in `internal/config` accepts six kinds. This section says why each kind and each field has its name. Types, defaults and limits are in the [manifest reference](reference/manifests.md).

### Common to every kind

| Field | Why it is named so |
| --- | --- |
| `apiVersion` | The version of the manifest language, written in the Kubernetes style. `v1alpha1` says it is early and may change. |
| `kind` | Which of the six declarations this document is. |
| `metadata.name` | The name other manifests use to refer to this one. It must be unique within its kind. |
| `metadata.zone` | The zone the object sits in, `runtime` or `sandbox`. A zone is a trust area: a `sandbox` workload may not hold actions or delegation, and may not message a `runtime` Vassal. Read by `Workload`, and checked on `Workflow` when set. |
| `spec` | What you want, as opposed to `metadata`, which says what the object is. |

### Workload

A workload is something the court may launch. The word is plain on purpose: it covers a coding agent, a long-lived agent and a program with no model.

| Field | Why it is named so |
| --- | --- |
| `spec.class` | What kind of workload it is: `coding-agent`, `long-lived-agent` or `deterministic-controller`. The last name says it runs fixed code and cannot hold a Model Gate grant. |
| `spec.runtime.type` | How the court launches it: `docker` or `worktree-cli`. |
| `spec.runtime.image` | The container image, needed for `docker`. |
| `spec.runtime.readOnlyRootfs` | Whether the container's root filesystem is read-only. |
| `spec.coding` | The block only a `coding-agent` has. |
| `spec.coding.harness` | The coding CLI that is launched: `claude`, `codex` or `agy`. A harness holds the agent on a lead, as in a climbing harness. |
| `spec.coding.repoPath` | The repository the agent works in. |
| `spec.coding.sandbox` | The host sandbox for the run, for example `seatbelt`. |
| `spec.agentCard` | A short description of the workload for other agents, in the style of an A2A Agent Card (ADR-0006). Holds `description` and `skills`. |
| `spec.roleCard` | The card that says what role the workload plays. It goes first in the context packet. |
| `spec.grants` | What the workload may ask for. A grant is permission given: nothing is allowed that is not listed. |
| `spec.grants.modelGate` | The grant for the Model Gate, the door for inference. `allowed` switches it on, `providers` limits which providers, and `budgetRef` names the `Budget` that caps the spend. |
| `spec.grants.mcpTools` | The tools it may call through the MCP Gate, such as `memory.get`. |
| `spec.grants.mcpServers` | Remote MCP servers the gate may proxy for it. Each has a `name`, a `url`, a `secretName` (the name of a stored secret, never the secret), an `authHeader` and `authPrefix` (how the credential is sent) and `allowedTools`. |
| `spec.grants.actions` | The typed actions it may ask for, such as `git.open_pr`. |
| `spec.grants.delegateTo` | The workloads it may hand a task to. |

### Policy

A policy is a set of rules about what a workload may do. It does not say who is right, only what is allowed.

| Field | Why it is named so |
| --- | --- |
| `spec.defaultEffect` | What happens when no rule matches. It must be `deny`: the court refuses by default. |
| `spec.rules` | The list of exceptions to that default. |
| `spec.rules[].workload` | The workload the rule is about. |
| `spec.rules[].capability` | What is being asked for, such as an action or a tool. |
| `spec.rules[].effect` | The outcome: `allow`, `approve` (wait for a person) or `deny`. |

### Budget

A budget is a spending limit. It is named as in accounting, not as a court term.

| Field | Why it is named so |
| --- | --- |
| `spec.perTask` | Limits for a single task. |
| `spec.perTask.maxInputTokens`, `maxOutputTokens` | The most tokens a task may send to and take from a model. |
| `spec.perTask.maxCostUSD` | The most a task may cost, in US dollars. |
| `spec.daily` | A limit counted per workload per day. |
| `spec.daily.maxCostUSD` | The most a workload may cost in a day. |

### Workflow

A workflow is a fixed order of steps, each run by a workload.

| Field | Why it is named so |
| --- | --- |
| `spec.description` | What the workflow is for. |
| `spec.steps` | The ordered steps. |
| `spec.steps[].name` | The name of the step. |
| `spec.steps[].workload` | Which workload runs it. |
| `spec.steps[].role` | The part the step plays: `author`, `verifier` or `fast`. A verifier checks what the author produced. |
| `spec.steps[].tier` | The class of model to use: `fast`, `coding` or `critic`. A tier is a rung, from cheap to careful. |
| `spec.steps[].maxRetries` | How many times a failed step may be tried again. |
| `spec.steps[].provider`, `model` | An explicit model route that overrides the tier. |
| `spec.steps[].prompt` | The text sent to the workload. |

### Schedule

A schedule starts work on a clock.

| Field | Why it is named so |
| --- | --- |
| `spec.cron` | The time pattern, in cron syntax. |
| `spec.targetKind` | What the schedule starts: a `workload` or a `workflow`. |
| `spec.targetName` | The name of that workload or workflow. |
| `spec.input` | The input given to the task it creates. |
| `spec.enabled` | Whether the schedule runs. |

The old [reference](reference.md) lists `spec.workload` for a schedule. The loader reads `targetKind` and `targetName`, and this page follows the code.

### Conclave

A **Conclave** is the room. It is the one kind that uses court words for its fields.

| Field | Why it is named so |
| --- | --- |
| `spec.description` | What the room is for. |
| `spec.budgetRef` | The `Budget` that caps the spend of the whole room, as a reference by name. This limit is also counted as a Ward. |
| `spec.wards` | The limits on the room. |
| `spec.wards.maxRuns` | The most launches in the room, with a hard ceiling of six. |
| `spec.wards.maxMessages` | The most messages the room may carry. |
| `spec.wards.maxPingPong` | The most turns that pass between members without the operator speaking. Named after the table-tennis game of messages back and forth. |
| `spec.vassals` | The members. At least one is required. |
| `spec.vassals[].role` | The name a member answers to in the room, such as `reviewer`. It is what others address as `@role`, so it may not be `operator`, `all` or `system`. |
| `spec.vassals[].workload` | The workload that holds the role. |
| `spec.vassals[].activation` | When the member is launched: `always-on` when the room opens, or `on-mention` when someone addresses it. |
| `spec.vassals[].canMessage` | The roles this member may speak to. Only these pairs may talk, and `operator` and `all` are valid targets. |

## Plain words

If you know another tool, these are the nearest court terms. They are close, not equal.

| In other tools | In Sablecourt | Note |
| --- | --- | --- |
| Agent | Workload, or Vassal when it holds a role in a Conclave | A coding agent is a `Workload` of class `coding-agent`, run by a harness. |
| Session, run | Task | One run of a workload. Plain word, not a court term. |
| Room, chat, team | Conclave | A Council is a Conclave with a fixed panel. |
| Participant, seat | Vassal | The role and the workload together. |
| Artifact, attachment | Relic, inside a Conclave | A task also has artifacts. They are not Relics. |
| Approval, confirmation | Approval | Plain word. A pending approval is held by the Approval Queue, and the web UI shows it in the panel "Tribunal · Approvals". |
| Memory, notes | Court Memory | Key-value, no secrets. Also the place learnings are kept today. |
| Guardrail, circuit breaker | Ward | A counter with a limit. |
| Human in the loop, intervention | Decree | A message to a Conclave. A person's answer to an approval is not a Decree. |
| Live logs, tail | Scry | Read-only and redacted. |
| Attach, take the controls | Takeover | Exclusive input. |
| Local mode, cluster mode | Sanctum, Citadel | The two values of `--mode`. |

## Not built yet

These names appear in the notes or the ADRs and have no counterpart in the code on `main`.

- Three Doors: ADR-0004 names the three capability gates after a layout in the reference architecture the notes call Agenteo: one door for models, one for data and one for mutations. The code has the gates but no identifier or label with this name. The docs use "Model Gate", "MCP Gate" and "Approval Queue".
- Tribunal Shield, councilors, quorum: Brand words for the mark. No code or manifest uses them.
- Barrier: ADR-0013 describes the rule that round two starts only when all recorded answers are in. The code has no name for it.
- Learning acceptance in the court's terms: ADR-0012 defines learnings with their own record. They are partly built and have no court name.
- Device pairing and push alerts for a tripped Ward: The brand notes expect a companion device. The pairing routes answer `503`.
