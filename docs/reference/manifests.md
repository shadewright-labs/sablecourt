# Manifests reference

A manifest is a YAML document that declares one `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule` or `Conclave`; the daemon and `sablectl validate` read them with the same loader.

Status: built.

The field tables are checked by `TestManifestReferenceMatchesTypesAndLoader` in `internal/config`, which `go test ./...` runs. Field and Type must match the JSON fields of the manifest types in `internal/domain/types.go`. Required is measured with the loader: the test deletes the field from example documents that load and loads them again. `yes` means the loader refuses every document without the field, `no` means it accepts every one; for a field inside a list entry, such as `spec.rules[].workload`, this holds for documents that have the entry, since an optional list may be left out, and `conditional` means it depends on other fields, named in the Meaning column. The Meaning column is written by hand, one sentence per field.

## Loading rules

Source: `internal/config/loader.go`, `internal/domain/types.go`.

| Rule | Value |
| --- | --- |
| `apiVersion` | Must be `sablecourt.io/v1alpha1`. Any other value is an error. |
| `kind` | One of `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule`, `Conclave`. |
| Input | A single file, or a directory searched recursively for `.yaml` and `.yml` files (any letter case) in sorted path order. A directory with no such file is an error. |
| Documents | Several documents per file, split on lines that contain only `---`. Blank documents are skipped. |
| Unknown fields | Rejected. Each document is decoded strictly. |
| Names | `metadata.name` is required and must be unique within its kind. A duplicate is an error. |
| Manifest digest | SHA-256 over every file's base name and bytes, in load order. The daemon stores it with each workload record. |
| Defaults | The loader applies one default: a Conclave vassal without `activation` becomes `on-mention`. Every other omitted field keeps its zero value. |

Check a path without a daemon with `sablectl validate <path>`. Reload a running daemon with `sablectl apply` (`POST /api/v1/reload`). The reload fails as a whole if any document is invalid, and the previous bundle stays in force.

## Common fields

Source: `internal/domain/types.go` (`TypeMeta`, `ObjectMeta`).

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `apiVersion` | string | yes | Must be `sablecourt.io/v1alpha1`. |
| `kind` | string | yes | One of `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule`, `Conclave`. |
| `metadata.name` | string | yes | Unique within its kind; on a Workload, `claude`, `codex` and `agy` are reserved for the built-in council panel. |
| `metadata.zone` | string | conditional | `runtime` or `sandbox`: required on a Workload, optional on a Workflow, and ignored on the other kinds. |

## Workload

Source: `internal/domain/types.go` (`WorkloadSpec`), `internal/config/loader.go` (`validateWorkload`). A Workload declares one agent, coding harness or deterministic controller and what it may reach. The [zone](../glossary.md) is `runtime` (trusted to hold mutating grants) or `sandbox` (read-only reach).

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `spec.class` | string | yes | `coding-agent`, `long-lived-agent` or `deterministic-controller`; it picks the adapter that runs the workload. |
| `spec.runtime.type` | string | yes | `docker` or `worktree-cli`. |
| `spec.runtime.image` | string | conditional | Container image, required when `runtime.type` is `docker`. |
| `spec.runtime.readOnlyRootfs` | bool | no | Run the container with a read-only root filesystem. |
| `spec.coding.harness` | string | conditional | `claude`, `codex` or `agy`, required for a `coding-agent`; the other classes must not set `spec.coding` at all. |
| `spec.coding.repoPath` | string | conditional | Repository the coding harness works in, required for a `coding-agent` and not checked for existence at load. |
| `spec.coding.sandbox` | string | no | Not checked at load. Ignored when the task runs in a harness container, which is the default; `sablectl init` and the web UI write `container`. Only in forced host mode (`--unsafe-host-harnesses`, outside invariants 2 and 3) does the adapter read it, and then it accepts only `seatbelt`, on macOS. |
| `spec.agentCard.description` | string | no | Description stored with the workload record and shown on the agent card. |
| `spec.agentCard.skills` | list of strings | no | Tags shown on the agent card. |
| `spec.roleCard` | string | no | Role text put into the seat's prompt by the context pack and the council. |
| `spec.grants.modelGate.allowed` | bool | no | Lets the workload call the Model Gate; a `deterministic-controller` must leave it `false`. |
| `spec.grants.modelGate.providers` | list of strings | no | Provider routes the workload may call; an empty list allows all of them. |
| `spec.grants.modelGate.budgetRef` | string | conditional | Name of a declared Budget, required when `allowed` is `true`. |
| `spec.grants.mcpTools` | list of strings | no | MCP tool names granted through the MCP gate; names are not checked at load, except on the workload of a `verifier` step (see [Workflow](#workflow)). |
| `spec.grants.mcpServers` | list of objects | no | Upstream MCP servers whose tools the gate proxies (parked, see below). |
| `spec.grants.actions` | list of strings | no | Typed actions granted; a `sandbox` workload must leave it empty. |
| `spec.grants.repos` | list of strings | no | Granted repositories for GitOps actions; an entry must not be empty, have outer whitespace or contain spaces, tabs or newlines. `git.push` accepts `owner/repo`, a URL or an absolute path. `git.open_pr`, `git.pr_checks` and `git.merge_pr` compare entries, when the list is not empty, as normalized `owner/repo` (outer slashes and a `.git` suffix removed). `git.create_branch` ignores the field. In `git.push`, a plain `owner/repo` entry matches only a GitHub push URL. A `git.push` destination must match an entry unless it is `origin` at the repository pinned at start, and it must also equal the `destination` field of the approved value exactly. Both must be an accepted form: `https://host[:port]/path` with no user information, `ssh://[git@]host[:port]/path`, the scp-like `git@host:path` or an absolute local path; `::` helper syntax, `file://` and other schemes, opaque URLs and any user other than plain `git` are refused (see [tools and actions](../how-it-works/tools-and-actions.md)). |
| `spec.grants.delegateTo` | list of strings | no | Other declared Workloads this one may delegate to; not itself, and empty for a `sandbox` workload. |

### Upstream MCP server (parked)

Source: `internal/domain/types.go` (`UpstreamMCPServer`), `internal/gate/mcp/upstream.go`. Each entry exposes the tools in `allowedTools` as `<name>.<tool>`. Parked: [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md) parks upstream MCP work. The loader checks only `name` (see below) and does not validate the other fields.

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `spec.grants.mcpServers[].name` | string | no | Prefix of the proxied tool names; an entry without a name is skipped. The loader refuses a name whose part before the first dot is a built-in tool namespace (`memory`, `conclave`, `sablecourt`, `git`, `sandbox`), compared trimmed and in lower case. |
| `spec.grants.mcpServers[].url` | string | no | Streamable HTTP endpoint of the server; an entry without a URL is skipped. A call needs an `http` or `https` URL with a host. With `http`, the `secretName` credential crosses the network unencrypted. |
| `spec.grants.mcpServers[].secretName` | string | no | Secret store entry sent as the credential on every call, whatever the URL scheme; with none, calls carry no credential. |
| `spec.grants.mcpServers[].authHeader` | string | no | Header that carries the secret, `Authorization` when empty. |
| `spec.grants.mcpServers[].authPrefix` | string | no | Text put before the secret, `Bearer ` when empty and the header is `Authorization`. |
| `spec.grants.mcpServers[].allowedTools` | list of strings | no | The only tools of this server that are registered. |

### MCP tool and action names

Source: `internal/gate/mcp/actions.go`, `internal/gate/mcp/memory.go`, `internal/conclave/broker.go`.

| Grant list | Names the daemon registers |
| --- | --- |
| `mcpTools` | `sablecourt.list_workloads`, `git.pr_checks`, `memory.put`, `memory.delete`, `memory.get`, `memory.search`, `conclave.post`, `conclave.read`, `conclave.relic_put`, `conclave.relic_get`, and `<server>.<tool>` for each upstream tool. |
| `actions` | `git.create_branch`, `git.push`, `git.open_pr`, `git.pr_checks`, `git.merge_pr`, `sandbox.propose_promotion`. |

## Policy

Source: `internal/domain/types.go` (`PolicySpec`), `internal/config/loader.go`, `internal/policy/policy.go`. A Policy holds deterministic rules that allow, require approval for, or deny a capability.

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `spec.defaultEffect` | string | yes | Must be `deny`, the only accepted value. |
| `spec.rules` | list of objects | no | Rules evaluated after the grant checks; with none, every capability ends in the default deny. |
| `spec.rules[].workload` | string | yes | `*`, an exact workload name, or a prefix ending in `*`. |
| `spec.rules[].capability` | string | yes | Capability pattern with the same matching rules as `workload`. |
| `spec.rules[].effect` | string | yes | `allow`, `approve` or `deny`. |

Capabilities are strings of three shapes: `mcp:<tool>`, `action:<name>` and `delegate:<workload>`. The Model Gate does not use the policy engine; it uses the workload's grant and Budget.

How the engine decides, in order: an empty workload or capability is denied; capabilities that start with `policy:`, `secret:` or `rbac:` are always denied; a `sandbox` workload is denied any `action:` or `delegate:` capability; a capability not listed in the workload's grants is denied; an `action:` capability is denied while its circuit breaker is open (three consecutive failures open it for two minutes). Then the rules run: any matching `deny` wins, otherwise any matching `approve` wins over `allow`, and no match is `default_deny`.

## Budget

Source: `internal/domain/types.go` (`BudgetSpec`), `internal/config/loader.go`, `internal/store/budget.go`. A Budget caps what a workload may spend through the Model Gate. No limit may be negative, `0` means no limit on that field, and at least one of the four must be positive.

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `spec.perTask.maxInputTokens` | integer | conditional | Input tokens one task may use. Must be positive when the other three are 0 or unset. |
| `spec.perTask.maxOutputTokens` | integer | conditional | Output tokens one task may use. Must be positive when the other three are 0 or unset. |
| `spec.perTask.maxCostUSD` | number | conditional | Cost one task may reach, compared in micro-USD. Must be positive when the other three are 0 or unset. |
| `spec.daily.maxCostUSD` | number | conditional | Cost per workload over the UTC calendar day, live reservations included. Must be positive when the other three are 0 or unset. |

## Workflow

Source: `internal/domain/types.go` (`WorkflowSpec`, `WorkflowStep`), `internal/config/loader.go`. A Workflow is an ordered list of steps, each run by a declared Workload.

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `spec.description` | string | no | Free text. |
| `spec.steps` | list of objects | yes | The steps, at least one. |
| `spec.steps[].name` | string | yes | Step name. |
| `spec.steps[].workload` | string | yes | Name of the declared Workload that runs the step. |
| `spec.steps[].role` | string | no | The reconciler acts on `verifier` and `author`; any other value has no effect. The loader refuses a `verifier` step whose workload holds any grant other than the read-only `mcpTools` `memory.get`, `memory.search`, `conclave.read`, `conclave.relic_get` and `sablecourt.list_workloads` (no upstream server can register these names, because the loader refuses an `mcpServers` name in a built-in namespace), or whose workload a non-verifier step before it also uses, and a verifier on a model workload when no earlier step runs a model. |
| `spec.steps[].tier` | string | no | `fast`, `coding` or `critic`; the tier's model route must suit the workload. |
| `spec.steps[].maxRetries` | integer | no | How many times a failed verifier step may send the author step back with a failure handoff. |
| `spec.steps[].provider` | string | no | Provider for the step's task, overriding the tier's route; a `deterministic-controller` step must leave it empty. |
| `spec.steps[].model` | string | no | Model for the step's task, overriding the tier's route; a `deterministic-controller` step must leave it empty. |
| `spec.steps[].prompt` | string | no | The step's task prompt. |

Source for the built-in tier routes: `internal/contextpack/tiers.go`. With no override saved, `fast` is `cli:agy` with `gemini-3.8-flash-high`, `coding` is `cli:claude` with `claude-sonnet-5-5`, and `critic` is `cli:codex` with `gpt-6.2-astra`, a model family other than the `coding` route's (see [configuration](configuration.md#model-tiers-and-families)). `GET` and `POST /api/v1/providers/tiers` read and change them.

## Schedule

Source: `internal/domain/types.go` (`ScheduleSpec`), `internal/config/loader.go`, `internal/controlplane/scheduler.go`. A Schedule starts a task or a Workflow on a cron timetable.

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `spec.cron` | string | yes | Standard five-field cron or a descriptor such as `@hourly`, evaluated in UTC. |
| `spec.targetKind` | string | yes | `workload` or `workflow`. |
| `spec.targetName` | string | yes | Name of a declared Workload or Workflow of that kind. |
| `spec.input` | string | no | Task input passed to the target. |
| `spec.enabled` | bool | no | Only an enabled Schedule fires on its timetable; a manual run works either way. |

A timetable run of a workload target uses the idempotency key `sched:<name>:<UTC minute>`, so the same minute never starts a second task. A timetable run of a workflow target is deduplicated by the same minute from a record the daemon keeps in memory for 24 hours. A manual run uses a unique key each time, and a manual workflow run starts a new workflow with the input as given.

## Conclave

Source: `internal/domain/types.go` (`ConclaveSpec`, `ConclaveWards`, `ConclaveVassal`), `internal/config/loader.go`, `internal/store/conclave.go`. A Conclave template describes a room where several workloads (vassals) exchange messages under limits called Wards. [ADR-0008](../adr/0008-conclaves-wards-and-live-attach.md) defines the model.

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `spec.description` | string | no | Free text. |
| `spec.budgetRef` | string | no | Name of a declared Budget for the room. |
| `spec.wards.maxRuns` | integer | no | How many times the room may start a task, from `0` to `6`; `0` is read as `6`. |
| `spec.wards.maxMessages` | integer | no | Message cap, not negative; `0` is read as twice the number of vassals. |
| `spec.wards.maxPingPong` | integer | no | Cap on back-and-forth exchanges, not negative; `0` is read as twice the number of vassals. |
| `spec.vassals` | list of objects | yes | The room's members, at least one. |
| `spec.vassals[].role` | string | yes | Unique role in the room, with no surrounding whitespace, no leading `@`, and not `operator`, `all` or `system`. |
| `spec.vassals[].workload` | string | yes | Name of the declared Workload that fills the seat. |
| `spec.vassals[].activation` | string | no | `always-on` or `on-mention`; the loader fills in `on-mention` when it is missing. |
| `spec.vassals[].canMessage` | list of strings | no | Each entry is `operator`, `all` or the role of a vassal in the room, the sender's own role included; a `sandbox` vassal cannot reach a `runtime` vassal, directly or through `all`. |

Councils opened with `sablectl council` or `POST /api/v1/councils` do not use a template. Their settings are request fields, listed under `POST /api/v1/councils` in the [HTTP API reference](http-api.md#councils-and-conclaves).

## In depth

- [Concepts](../concepts.md) and the [glossary](../glossary.md) for workload, zone, policy, budget and Conclave.
- [Workloads and runtimes](../how-it-works/workloads-and-runtimes.md), [model gate](../how-it-works/model-gate.md), [tools and actions](../how-it-works/tools-and-actions.md) and [conclaves](../how-it-works/conclaves.md) for what each field does at run time.
- [ADR-0002](../adr/0002-heterogeneous-mvp-workload-adapters.md), [ADR-0003](../adr/0003-hybrid-workload-isolation.md) and [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md) for the decisions behind classes, zones and grants.
