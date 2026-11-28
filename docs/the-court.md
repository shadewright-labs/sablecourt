# The court

Sablecourt names some of its parts after a court. This page says what each court name means, why it fits, and where it appears in the product. It does not describe how anything behaves: where a term has its own behaviour page, the row links to it. Plain definitions are in the [glossary](glossary.md), and types and defaults are in the [manifest reference](reference/manifests.md).

Status: built (with Citadel, Voice Decree, Takeover and upstream `mcpServers` parked by [ADR-0011](adr/0011-sanctum-scope-trust-and-cli-harnesses.md)), apart from the items under [Not built yet](#not-built-yet). The location lists below were taken by searching the web UI templates and scripts, the HTTP routes, the `sablectl` and `sablecourtd` commands, the audit actions and the manifest loader in the source tree. A term can also appear in places that search did not reach, such as free text in an error message.

## The convention

Sablecourt is the Court among the Shadewright Labs products. ADR-0007 describes Nightloom as the local, read-only watchman, and Sablecourt is the one that runs declared workloads under rules.

In the court picture:

- The operator is the person the court answers to.
- The daemon (`sablecourtd`) is the court itself.
- A Conclave is a room the court convenes. Its members, the Vassals, are workloads that each hold a role in it. A Council is a Conclave with a fixed panel of three: `claude`, `codex` and `agy`. The panel is all three or nothing, not a subset.
- Wards limit the room, a Decree is the operator speaking into it, and Relics are the files its members hand to one another.
- The gates are the doors a workload's requests go through. The audit chain is the court's record.

How these parts behave is in [how it works](how-it-works/index.md).

### What the metaphor buys

- A short name for each role. "Vassal" says a member that serves a room and answers to it, which "agent" does not.
- Terms that other tools do not use in the same sense. A "session" or an "approval" means something different from one tool to the next, and a Ward is a Ward only here.
- Specific verbs. A Ward trips, a Decree is issued, a Relic is put.

### Where it stops

- The terms in the table below are the court terms the code uses. Other names are plain: `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule`, task, lease, harness, gate, audit chain, approval.
- There is no court term for a task or a lease, because the plain word is the clearer one.
- Some terms appear in the code only in the web UI (Houses, Tribunal, Cockpit, Studio). The table says where each appears.
- Terms that appear only in ADRs or have not been built yet are kept out of the table. They are under [Not built yet](#not-built-yet).

[![The court at a glance: an operator outside the court sends a Decree to a Conclave of Vassals with Wards and Relics inside sablecourtd, where Vassals reach the Model Gate and MCP Gate to reach a model provider, Court Memory and a Git host.](diagrams/court-at-a-glance.svg)](diagrams/court-at-a-glance.svg "Open full size")

The diagram shows who is who and who talks to whom: the operator, a Conclave of Vassals with its Wards and Relics, the Model Gate, the MCP Gate, and behind them a model provider, a Git host and Court Memory. For how requests move, see [how it works](how-it-works/index.md).

## Court terms

"Where the name comes from" gives the ADR that uses the word, where there is one. "Where it appears" lists the places found in the source tree and the kind of element each one is. The next section, [Why the names fit](#why-the-names-fit), gives the reasons for each name. Behaviour is in the linked pages.

| Term | In plain words | Where the name comes from | Where it appears |
| --- | --- | --- | --- |
| The Court | The role Sablecourt has among the Shadewright Labs products. | No ADR; used on the [home page](index.md) and in the repository README. | Web UI: the `title` attribute of the logo link on the home page, "Sablecourt · The Court". |
| Conclave | A room where several workloads work on one topic. See [Conclaves](how-it-works/conclaves.md). | ADR-0008 ("Multi-agent Conclaves, Wards, Decrees, Relics, and live Scry/Takeover"). | Manifest kind `Conclave`. HTTP routes under `/api/v1/conclaves` and `/gate/conclave/{id}`. CLI group `sablectl conclave`. MCP gate tools `conclave.post`, `conclave.read`, `conclave.relic_put`, `conclave.relic_get`. Tools of `sablectl mcp-server`: `sablecourt_open_conclave`, `sablecourt_inspect_conclave`, `sablecourt_conclave_decree`. Audit actions `conclave.open`, `conclave.post`, `conclave.ward`, `conclave.relic_put`, `conclave.close` and `conclave.run_window_reset`. Web UI: nav item "Conclaves", pages `/conclaves` and `/conclaves/{id}`, the form headed "Convene a Conclave" with its "Conclave template" select, the heading "Active and recent Conclaves", the "Close Conclave" form, and the heading "Define Conclave (multi-agent room)" on the Studio page. |
| Council | A Conclave of `claude`, `codex` and `agy` that answers one question. See [Council](how-it-works/council.md). | ADR-0013 ("Bounded council rounds in Conclaves"). | CLI `sablectl council`, which prompts "Council question: " on standard error when no question is given. The short text of `sablectl harness` ("Build local council harness images and log in to them"). Route `POST /api/v1/councils`. Tools `sablecourt_council_convene`, `sablecourt_council_read` and `sablecourt_council_say` of `sablectl mcp-server`. The error "council opening requires Sanctum". No web UI page or label uses the word. |
| Vassal | A member of a Conclave: a role bound to a workload. | ADR-0008. | Manifest field `spec.vassals` of a Conclave. Web UI: the heading "Vassals" on the room page, the "Vassal 1" to "Vassal 3" labels, the "+ Add vassal" button and the `vassal_role`, `vassal_workload` and `vassal_activation` fields of the Studio Conclave form, and the "(N vassals)" suffix in the template select of the Convene form. |
| Ward | A limit on a Conclave. See [Conclaves](how-it-works/conclaves.md). | ADR-0008 ("Deterministic `Wards`"). | Manifest field `spec.wards`. Conclave state `input-required`. Audit action `conclave.ward`, the alert event `conclave.ward_paused` and the metric `sablecourt_conclave_ward_trips_recent`. Web UI: the line "Wards: maxMessages=..., maxPingPong=..." and a "ward:" reason line on the room page, a `ward=` suffix in the Conclave list, the placeholder "Direct the Conclave or resume after a Ward pause..." in the Decree field, the "Ward: maxMessages" and "Ward: maxPingPong" fields of the Studio Conclave form, and the description text of the alerts card on the Providers page. |
| Decree | A message from the operator to a Conclave. See [Conclaves](how-it-works/conclaves.md). | ADR-0008. | CLI `sablectl conclave decree`, and the `--decree` flag of `sablectl conclave open`. Routes `POST /api/v1/conclaves/{id}/decree`, `POST /api/v1/conclaves/{id}/voice-decree` and `POST /ui/conclaves/{id}/decree`. Tool `sablecourt_conclave_decree`, and the `decree` field of `sablecourt_open_conclave`, of `sablectl mcp-server`. Message kind `decree`. Web UI: the "Opening Decree (optional, sent to @all)" field of the Convene form, the "Issue operator Decree to" select, the "Decree" field and the "Issue Decree" button on the room page, and the "Voice Decree (local STT)" button. |
| Relic | A named file that members of a Conclave hand to one another. See [Conclaves](how-it-works/conclaves.md) and [context and learning](how-it-works/context-and-learning.md). | ADR-0008. | CLI `sablectl conclave relic put` and `get`. Routes under `/api/v1/conclaves/{id}/relics`, `/gate/conclave/{id}/relics` and `POST /ui/conclaves/{id}/relics`. MCP gate tools `conclave.relic_put` and `conclave.relic_get`. Audit action `conclave.relic_put`. Web UI: the heading "Relics (N)" and the "Upload Relic" form on the room page. The context packet section "Conclave Relics". |
| Scry | A live, read-only view of a running task's output. See [tasks](how-it-works/tasks.md). | ADR-0008. | CLI `sablectl task scry`. Route `GET /api/v1/tasks/{id}/scry`. Audit action `task.scry`. Web UI: on the task page, the heading "Live console (Scry / Takeover)" and the status text "Connecting to Scry stream..." and "Scry live (read-only)". |
| Takeover | Exclusive input to a running task. See [tasks](how-it-works/tasks.md). | ADR-0008. | CLI `sablectl task takeover`. Routes `POST /api/v1/tasks/{id}/takeover`, `.../stdin` and `.../detach`. Audit actions `task.takeover` and `task.detach`. Web UI: the "Takeover" button on the task page. |
| Sanctum | A `sablecourtd` mode, the default. | ADR-0009 ("Sanctum and Citadel modes"). | `sablecourtd --mode=sanctum`. The `--mode=sanctum` argument in the launchd job and the systemd unit that `sablectl service install` writes. The error "council opening requires Sanctum". The help text of the `--mode` flag, "Operating mode: sanctum (local macOS/Ubuntu) or citadel (Kubernetes)". |
| Citadel | The Kubernetes mode of `sablecourtd`. | ADR-0009. | `sablecourtd --mode=citadel`. The log line "citadel mode active: coding agents and council seats disabled, fail-closed operator auth enforced". The help text of the `--mode` flag. |
| Court Memory | The key-value store the court keeps across tasks and Conclaves. See [context and learning](how-it-works/context-and-learning.md). | ADR-0010 ("Scoped learnings on Court Memory"). | Web UI: the page `/memory` with the title and heading "Court Memory", the nav item "Memory", the "Store Memory Entry" form, and the command palette entry "Court Memory (FTS5)". CLI `sablectl memory` with `list`, `set` and `delete`. Routes under `/api/v1/memory`. MCP gate tools `memory.get`, `memory.put`, `memory.search` and `memory.delete`. Store table `court_memory`. |
| Houses | The web UI's name for the declared workloads. | UI label only. | Web UI home page only: the card "Declared Houses" and the panel "Houses (Workloads)". |
| Tribunal | The web UI's name for the audit view and the approval panel. See [approvals](how-it-works/approvals.md). | UI label only. | Web UI: the heading and title "Audit Tribunal" on the `/audit` page, the command palette entry "Audit Tribunal & SHA-256 Chain", the count card "Tribunal Approvals" and the panel "Tribunal · Approvals" on the home page. |
| Cockpit | The web UI's home page. | UI label only. | Web UI: the nav item "Cockpit" and the command palette entry "Cockpit Dashboard". The home page layout also uses the CSS class `sc-cockpit`. |
| Studio | The web UI page for defining an agent, a workflow or a Conclave. | Web UI route and label only. | Web UI: the nav item "Studio", the page `/studio` (and `/ui/studio/*` form routes) with the heading "Agent & workflow studio", and the command palette entry "Workload & Conclave Studio". |
| Convene | The verb for opening a Conclave or a Council. | ADR-0013 ("convenes `claude`, `codex`, and `agy`"). | CLI: the short text of `sablectl conclave` ("Convene and direct multi-agent Conclaves (Vassals, Wards, Decrees, Relics)"), `sablectl conclave open` ("Convene a new Conclave from a manifest template") and `sablectl council` ("Convene a bounded multi-agent council, stream round answers, and print the digest"), and the output "convened conclave" and "convened council". The descriptions of the `sablecourt_open_conclave` and `sablecourt_council_convene` tools. Web UI: the heading "Convene a Conclave", the button "Convene Conclave" and the notice "Conclave convened". |

The names in manifests, routes and commands are the same words as the table: `Conclave`, `vassals`, `wards`, `decree`, `relic`, `scry`, `takeover`. The web UI names (Houses, Tribunal, Cockpit, Studio) appear only in the web UI, and the plain word for a workload elsewhere is "workload".

## Why the names fit

This is our reasoning for why each name suits its component: what the word means in a court or in ordinary English, and which property of the component it points at. It is design rationale, not the history of how the names were chosen. Each entry ends with a rating: good, partial or weak.

| Term | What the word means | The property it points at | Fit |
| --- | --- | --- | --- |
| Sablecourt | A court is a body that hears a matter, applies rules and records what it decided. "Sable" is the heraldic word for black. | The product applies rules to requests and keeps a record. | Good for "court". The "sable" reading is ours. |
| The Court | The body that decides under rules. | `sablecourtd` is the part that holds the rules. | Good |
| Conclave | A private meeting held behind closed doors. | A bounded room of members with limits of its own. | Good |
| Council | A body that deliberates and advises, and does not rule. | Three harnesses answer one question and advise. | Good |
| Vassal | Someone who serves a higher authority and answers to it. | A member bound to a workload that answers to the room. | Good. The feudal sense of owing service fits less well than the sense of answering to the room. |
| Ward | To keep ward is to guard, and a ward is also a person under a guardian. | A limit that guards the room. | Good. Both senses point at a guard that stops the room and does not judge it. |
| Decree | An authoritative order from the person in charge. | The operator's message to the room. | Good |
| Relic | An object that is kept and passed on. | A named file handed between members. | Partial. Relics are old objects, and these files are made during the run. The "kept and passed on" part fits. |
| Scry | To see something far away by looking into a glass, without touching it. | A view of a running task that does not touch it. | Good |
| Takeover | Taking control of something that someone else is running. | The operator takes the input of a running task. | Good |
| Sanctum | A private, protected place. | The mode for one person's machine. | Good |
| Citadel | The fortified stronghold of a city. | The mode for a shared Kubernetes place. The pair reads as a personal room against a fortified shared place. | Good |
| Court Memory | A court keeps records of what it has been told. | A store of entries the court keeps. The audit chain is closer to a court record than this store is. | Partial |
| Houses | A household or a noble family under a lord. | The home page shows each declared workload as a card. "Workload" is the working name. | Weak |
| Tribunal | A court of justice, where a decision is given. | The approvals panel is a decision point. The audit view is closer to a record. | Partial |
| Cockpit | The place where a pilot sits among the instruments. | The home page shows several counts and lists on one screen. It is not a court word. | Weak as a court term. It describes the page. |
| Studio | A room where something is made. | The page where manifests are made. It is not a court word. | Weak as a court term. It describes the page. |
| Convene | To call a group together for a purpose. | The act of opening a room and calling its first members. | Good |

## Manifest kinds and options

A manifest is a YAML document with an `apiVersion`, a `kind` and a `metadata.name`. The loader accepts six kinds: Workload, Policy, Budget, Workflow, Schedule and Conclave. This section says what each name means. Types, defaults, validation and limits are in the [manifest reference](reference/manifests.md). How a Workload runs is in [workloads and runtimes](how-it-works/workloads-and-runtimes.md), and a Conclave in [Conclaves](how-it-works/conclaves.md).

### Common to every kind

| Field | Why it is named so |
| --- | --- |
| `apiVersion` | The version of the manifest language, written in the Kubernetes style. `v1alpha1` says it is early. |
| `kind` | Which of the six declarations this document is. |
| `metadata` | What the object is, as opposed to `spec`, which says what you want. |
| `metadata.name` | The name other manifests use to refer to this one. |
| `metadata.zone` | The isolation zone (`runtime` or `sandbox`), required on a Workload and optional on a Workflow; a `sandbox` workload cannot hold mutating action or delegation grants. |
| `spec` | What you want, as opposed to `metadata`. |

### Workload

A workload is something the court may launch. The word is plain on purpose: it covers a coding agent, a long-lived agent and a program with no model.

| Field | Why it is named so |
| --- | --- |
| `spec.class` | What kind of workload it is. |
| `spec.runtime` | How the court launches the workload. |
| `spec.runtime.type` | The launch method. |
| `spec.runtime.image` | The container image. |
| `spec.runtime.readOnlyRootfs` | Whether the container's root filesystem is read-only. |
| `spec.coding` | The block that describes a coding agent. |
| `spec.coding.harness` | The coding CLI that is launched (`claude`, `codex` or `agy`). See [harness containers](how-it-works/harness-containers.md). |
| `spec.coding.repoPath` | The repository the agent works in. |
| `spec.coding.sandbox` | The host sandbox for the run. |
| `spec.agentCard` | A short description of the workload for other agents, in the style of an A2A Agent Card (ADR-0006). |
| `spec.roleCard` | The card that says what role the workload plays. |
| `spec.grants` | What the workload may ask for. A grant is permission given. |
| `spec.grants.modelGate` | The grant for the Model Gate, the door for inference. |
| `spec.grants.mcpTools` | The tools it may call through the MCP Gate. |
| `spec.grants.mcpServers` | Remote MCP servers the gate may proxy for it (parked by ADR-0011). |
| `spec.grants.actions` | The typed actions it may ask for. |
| `spec.grants.delegateTo` | The workloads it may hand a task to. |

### Policy

A policy is a set of rules about what a workload may do. See [approvals](how-it-works/approvals.md).

| Field | Why it is named so |
| --- | --- |
| `spec.defaultEffect` | The outcome when no rule matches. |
| `spec.rules` | The list of rules. |
| `spec.rules[].workload` | The workload the rule is about. |
| `spec.rules[].capability` | What is being asked for. |
| `spec.rules[].effect` | The outcome of the rule. |

### Budget

A budget is a spending limit. It is named as in accounting, not as a court term.

| Field | Why it is named so |
| --- | --- |
| `spec.perTask` | Limits for a single task. |
| `spec.perTask.maxInputTokens`, `maxOutputTokens` | The most tokens in and out. |
| `spec.perTask.maxCostUSD` | The most cost, in US dollars. |
| `spec.daily` | Limits for a day. |
| `spec.daily.maxCostUSD` | The most cost, in US dollars. |

### Workflow

A workflow is a fixed order of steps, each run by a workload.

| Field | Why it is named so |
| --- | --- |
| `spec.description` | What the workflow is for. |
| `spec.steps` | The ordered steps. |
| `spec.steps[].name` | The name of the step. |
| `spec.steps[].workload` | Which workload runs it. |
| `spec.steps[].role` | The part the step plays. |
| `spec.steps[].tier` | The class of model to use. A tier is a rung, from cheap to careful. |
| `spec.steps[].maxRetries` | How many times a failed step may be tried again. |
| `spec.steps[].provider`, `model` | An explicit model route. |
| `spec.steps[].prompt` | The text sent to the workload. |

### Schedule

A schedule starts work on a clock.

| Field | Why it is named so |
| --- | --- |
| `spec.cron` | The time pattern, in cron syntax. |
| `spec.targetKind` | The kind of thing the schedule starts. |
| `spec.targetName` | The name of that thing. |
| `spec.input` | The input passed to the workload task or workflow it starts. |
| `spec.enabled` | Whether the schedule is switched on. |

### Conclave

A **Conclave** is the room. It is the one kind that uses court words for its fields. See [Conclaves](how-it-works/conclaves.md) and [Council](how-it-works/council.md).

| Field | Why it is named so |
| --- | --- |
| `spec.description` | What the room is for. |
| `spec.budgetRef` | A reference by name to a `Budget` for the room. ADR-0008 counts it among the Wards. |
| `spec.wards` | The limits on the room. |
| `spec.wards.maxRuns` | The most runs in one run window. An operator Decree that resumes a paused room resets the window baseline to the current run count, allowing up to `maxRuns` more runs, so the room total can exceed `maxRuns`. |
| `spec.wards.maxMessages` | The most Vassal messages since the last operator Decree. |
| `spec.wards.maxPingPong` | The most back-and-forth turns, named after the table-tennis game. |
| `spec.vassals` | The members. |
| `spec.vassals[].role` | The name a member answers to in the room, such as `reviewer`. |
| `spec.vassals[].workload` | The workload that holds the role. |
| `spec.vassals[].activation` | When the member is launched. |
| `spec.vassals[].canMessage` | The roles this member may speak to. |

## Plain words

If you know another tool, these are the nearest court terms. They are close, not equal.

| In other tools | In Sablecourt | Note |
| --- | --- | --- |
| Agent | Workload, or Vassal when it holds a role in a Conclave | A coding agent is a `Workload` of class `coding-agent`, run by a harness. |
| Session, run | Task | Plain word, not a court term. See [tasks](how-it-works/tasks.md). |
| Room, chat, team | Conclave | A Council is a Conclave with a fixed panel. |
| Participant, seat | Vassal | The role and the workload together. |
| Artifact, attachment | Relic, inside a Conclave | A task also has artifacts. They are not Relics. |
| Approval, confirmation | Approval | Plain word. See [approvals](how-it-works/approvals.md). |
| Memory, notes | Court Memory | A key-value store. |
| Guardrail, circuit breaker | Ward | A limit on a Conclave. |
| Human in the loop, intervention | Decree | A message to a Conclave. |
| Live logs, tail | Scry | A read-only view. |
| Attach, take the controls | Takeover | Exclusive input (parked by ADR-0011). |
| Local mode, cluster mode | Sanctum, Citadel | The two primary values of `--mode` (Citadel is parked by ADR-0011). |

For the concepts behind these names, see [concepts](concepts.md). Commands are in the [CLI reference](reference/cli.md).

## Not built yet

These terms appear in ADRs but have no identifier or UI label in `cmd/` or `internal/` on `main`:

- Three Doors: [ADR-0004](adr/0004-in-house-capability-gates-three-doors.md) names the three capability gates, and some docs (such as [concepts](concepts.md) and the [glossary](glossary.md)) mention the phrase. The gates themselves are built (Approval Queue is partly built), but a search of `cmd/` and `internal/` found no identifier or label with this name; the code uses Model Gate, Data/Action Gate (called MCP Gate in the docs) and Approval Queue.
- Barrier: [ADR-0013](adr/0013-bounded-council-rounds.md) describes the rule that round two starts only when all recorded answers are in. That rule is implemented in the council round scheduler (`internal/conclave/rounds.go`), though non-test code has no identifier named "barrier".
- Learning acceptance (planned, [ADR-0012](adr/0012-learning-evidence-and-expiry.md), #43 to #47): the `learnings` SQLite table and store methods exist, and context packets with a local destination read accepted records from it, while the operator review interface is not built yet and has no court name.
