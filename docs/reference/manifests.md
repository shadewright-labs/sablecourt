# Manifests reference

A manifest is a YAML document that declares one `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule` or `Conclave`; the daemon and `sablectl validate` read them with the same loader.

Status: built.

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

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `apiVersion` | string | yes | See above. |
| `kind` | string | yes | See above. |
| `metadata.name` | string | yes | Blank or whitespace-only is an error. On a Workload, `claude`, `codex` and `agy` are reserved for the built-in council panel and are refused. |
| `metadata.zone` | `runtime` or `sandbox` | Workload: yes. Workflow: no. | The shared metadata type accepts it on every kind. Only Workload and Workflow check it. |

## Workload

Source: `internal/domain/types.go` (`WorkloadSpec`), `internal/config/loader.go` (`validateWorkload`). A Workload declares one agent, coding harness or deterministic controller and what it may reach. The [zone](../glossary.md) is `runtime` (trusted to hold mutating grants) or `sandbox` (read-only reach).

| Field | Type | Default | Validation |
| --- | --- | --- | --- |
| `spec.class` | `coding-agent`, `long-lived-agent` or `deterministic-controller` | none | Required. Any other value is `invalid class`. Read at run time to pick the adapter: the coding adapter for `coding-agent`, the container adapter for the others. |
| `spec.runtime.type` | `docker` or `worktree-cli` | none | Required. Any other value is an error. The loader checks it; the adapter is chosen by `spec.class`, so no run-time code reads a value the loader accepts. |
| `spec.runtime.image` | string | empty | Required when `runtime.type` is `docker`. Read by the Docker and Kubernetes adapters, which refuse an empty image. |
| `spec.runtime.readOnlyRootfs` | bool | `false` | None. Read by the Docker and Kubernetes adapters. |
| `spec.coding.harness` | `claude`, `codex` or `agy` | none | Required for `coding-agent`. The other classes must not set `spec.coding`. Read by the coding adapter and the model tier check. |
| `spec.coding.repoPath` | string | none | Required for `coding-agent`. Not checked for existence at load. Read by the context pack and by the Workflow checkout checks. |
| `spec.coding.sandbox` | string | none | Not checked at load. Read by the coding adapter at launch: `seatbelt` is accepted on macOS when the Seatbelt executable exists, and every other value, `none` included, is refused. |
| `spec.agentCard.description` | string | empty | None. Stored with the workload record and advertised on the agent card (not for `sandbox` workloads). |
| `spec.agentCard.skills` | list of strings | empty | None. Advertised as tags on the agent card. |
| `spec.roleCard` | string | empty | None. Role text that the context pack and the council put into a seat's prompt. |
| `spec.grants.modelGate.allowed` | bool | `false` | Must be `false` for `deterministic-controller`. Read by the Model Gate, which refuses a workload without it (403), and by the reconciler when it decides whether to pass `SABLECOURT_MODEL_GATE_URL`. |
| `spec.grants.modelGate.providers` | list of strings | empty | An empty list allows every provider route (`openai`, `anthropic`, `gemini`, `local`). The names are not checked at load. Read by the Model Gate: a call to a provider outside a non-empty list is refused, and the list also decides whether a fallback between `openai` and `local` is allowed. |
| `spec.grants.modelGate.budgetRef` | string | empty | Required when `allowed` is `true`, and must name a declared Budget. Read by the Model Gate on every call. |
| `spec.grants.mcpTools` | list of strings | empty | Names are not checked at load. Read by the MCP gate, which skips a name it has not registered when it builds the tool list (see [MCP tool names](#mcp-tool-and-action-names)), and by the policy engine as the grant behind `mcp:<tool>`. |
| `spec.grants.mcpServers` | list of objects | empty | Parked. Read by the daemon when it registers upstream tools. See [Upstream MCP server](#upstream-mcp-server-parked). |
| `spec.grants.actions` | list of strings | empty | Names are not checked at load. A `sandbox` workload must leave it empty. Read by the policy engine as the grant behind `action:<name>`. |
| `spec.grants.delegateTo` | list of strings | empty | Each name must be another declared Workload, not the workload itself. A `sandbox` workload must leave it empty. Read by the policy engine as the grant behind `delegate:<workload>`, and by the council panel check, which refuses a panel workload that has any delegation, action or tool grant. The only function that evaluates a `delegate:` capability, `ProposeChildTask`, has no caller in the daemon, so nothing delegates a task today. |

### Upstream MCP server (parked)

Source: `internal/domain/types.go` (`UpstreamMCPServer`), `internal/gate/mcp/upstream.go`. Each entry in `spec.grants.mcpServers` exposes the tools in `allowedTools` as `<name>.<tool>`. The status is parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md). The loader does not validate these fields.

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `name` | string | none | Entries with an empty `name` or `url` are skipped. |
| `url` | string | none | Must be an `http` or `https` URL with a host when a tool is called. |
| `secretName` | string | empty | Name in the secret store. With none, the call carries no credential. |
| `authHeader` | string | `Authorization` | Header that carries the secret. |
| `authPrefix` | string | `Bearer ` when the header is `Authorization`, otherwise empty | Prepended to the secret. |
| `allowedTools` | list of strings | empty | Only these tools are registered. |

### MCP tool and action names

Source: `internal/gate/mcp/actions.go`, `internal/gate/mcp/memory.go`, `internal/conclave/broker.go`.

| Grant list | Names the daemon registers |
| --- | --- |
| `mcpTools` | `sablecourt.list_workloads`, `memory.put`, `memory.delete`, `memory.get`, `memory.search`, `conclave.post`, `conclave.read`, `conclave.relic_put`, `conclave.relic_get`, and `<server>.<tool>` for each upstream tool. |
| `actions` | `git.create_branch`, `git.open_pr`, `sandbox.propose_promotion`. |

## Policy

Source: `internal/domain/types.go` (`PolicySpec`), `internal/config/loader.go`, `internal/policy/policy.go`. A Policy holds deterministic rules that allow, require approval for, or deny a capability.

| Field | Type | Default | Validation |
| --- | --- | --- | --- |
| `spec.defaultEffect` | `deny` | none | Required, and the only accepted value. An omitted or different value fails with `defaultEffect must be "deny"`. |
| `spec.rules[].workload` | string | none | Required. `*`, an exact name, or a prefix ending in `*`. |
| `spec.rules[].capability` | string | none | Required. Same pattern rules. |
| `spec.rules[].effect` | `allow`, `approve` or `deny` | none | Required. Any other value is `invalid effect`. |

Capabilities are strings of three shapes: `mcp:<tool>`, `action:<name>` and `delegate:<workload>`. The Model Gate does not use the policy engine; it uses the workload's grant and Budget.

How the engine decides, in order: an empty workload or capability is denied; capabilities that start with `policy:`, `secret:` or `rbac:` are always denied; a `sandbox` workload is denied any `action:` or `delegate:` capability; a capability not listed in the workload's grants is denied; an `action:` capability is denied while its circuit breaker is open (three consecutive failures open it for two minutes). Then the rules run: any matching `deny` wins, otherwise any matching `approve` wins over `allow`, and no match is `default_deny`.

## Budget

Source: `internal/domain/types.go` (`BudgetSpec`), `internal/config/loader.go`, `internal/store/budget.go`. A Budget caps what a workload may spend through the Model Gate.

| Field | Type | Default | Validation |
| --- | --- | --- | --- |
| `spec.perTask.maxInputTokens` | integer | `0` | Must not be negative. `0` means no limit on this field. |
| `spec.perTask.maxOutputTokens` | integer | `0` | Same. |
| `spec.perTask.maxCostUSD` | number | `0` | Same. Compared in micro-USD. |
| `spec.daily.maxCostUSD` | number | `0` | Same. Counted per workload, over the UTC calendar day, including live reservations. |

At least one of the four limits must be positive. A Budget with all four at `0` is an error.

## Workflow

Source: `internal/domain/types.go` (`WorkflowSpec`, `WorkflowStep`), `internal/config/loader.go`. A Workflow is an ordered list of steps, each run by a declared Workload.

| Field | Type | Default | Validation |
| --- | --- | --- | --- |
| `metadata.zone` | `runtime` or `sandbox` | empty | If set, must be one of the two. |
| `spec.description` | string | empty | None. |
| `spec.steps` | list | none | At least one step. |
| `spec.steps[].name` | string | none | Required. |
| `spec.steps[].workload` | string | none | Required, and must name a declared Workload. |
| `spec.steps[].name` | string | none | Required (`step <n>: name is required`). |
| `spec.steps[].role` | string | empty | Not checked at load. The reconciler reads `verifier` (it builds a verifier prompt and checks out the author's commit) and `author` (the step a failed verifier sends back); any other value is passed through and has no effect. |
| `spec.steps[].tier` | `fast`, `coding` or `critic` | empty | If set, the tier must resolve to a route, and the route must suit the workload. The loader checks the built-in routes; the daemon checks again against the saved routes when it starts the workflow. Otherwise `unknown model tier`, `invalid tier provider or model` (a route id of 1 to 64 characters for the provider and 1 to 128 for the model, from letters, digits and `-._:/`, not starting with `-`), `deterministic workload cannot use a model tier`, or `tier model is incompatible with the workload harness` (for a `coding-agent`, the provider must be `cli:<harness>` and the model must fit the harness: `claude-*`, `opus`, `sonnet` or `haiku` for `claude`; `gpt-*` or `codex-*` for `codex`; `gemini-*` or `claude-*` for `agy`). |
| `spec.steps[].maxRetries` | integer | `0` | Not checked at load. Read by the reconciler. A positive value lets a failed verifier step send the author step again with a failure handoff, up to that many times. |
| `spec.steps[].provider` | string | empty | A `deterministic-controller` step must leave it empty. Read by the reconciler as the task's provider. With a `tier`, a step that sets either `provider` or `model` uses its own values instead of the tier's route, and they must pass the same check. |
| `spec.steps[].model` | string | empty | Same. |
| `spec.steps[].prompt` | string | empty | None. Read by the reconciler as the step's task prompt. |

Source for the built-in tier routes: `internal/contextpack/tiers.go`. With no override saved, `fast` is `cli:agy` with `gemini-3.8-flash-high`, `coding` is `cli:claude` with `claude-sonnet-5-5`, and `critic` is `cli:claude` with `claude-opus-5-5`. `GET` and `POST /api/v1/providers/tiers` read and change them.

## Schedule

Source: `internal/domain/types.go` (`ScheduleSpec`), `internal/config/loader.go`, `internal/controlplane/scheduler.go`. A Schedule starts a task or a Workflow on a cron timetable.

| Field | Type | Default | Validation |
| --- | --- | --- | --- |
| `spec.cron` | string | none | Required. Standard five-field cron or a descriptor such as `@hourly`, evaluated in UTC. A parse failure is `invalid cron expression`. |
| `spec.targetKind` | `workload` or `workflow` | none | Required. Any other value is an error. |
| `spec.targetName` | string | none | Required, and must name a declared Workload or Workflow of that kind. |
| `spec.input` | string | `{}` for a workload target | None. Passed as the task input. |
| `spec.enabled` | bool | `false` | A Schedule that is not enabled never fires on its timetable. `POST /api/v1/schedules/{name}/run` still runs it. |

A timetable run of a workload target uses the idempotency key `sched:<name>:<UTC minute>`, so the same minute never starts a second task. A timetable run of a workflow target is deduplicated by the same minute from a record the daemon keeps in memory for 24 hours. A manual run uses a unique key each time, and a manual workflow run starts a new workflow with the input as given.

## Conclave

Source: `internal/domain/types.go` (`ConclaveSpec`, `ConclaveWards`, `ConclaveVassal`), `internal/config/loader.go`, `internal/store/conclave.go`. A Conclave template describes a room where several workloads (vassals) exchange messages under limits called Wards. [ADR-0008](../adr/0008-conclaves-wards-and-live-attach.md) defines the model.

| Field | Type | Default | Validation |
| --- | --- | --- | --- |
| `spec.description` | string | empty | None. |
| `spec.budgetRef` | string | empty | If set, must name a declared Budget. |
| `spec.wards.maxRuns` | integer | `0`, read as `6` | Must be from `0` to `6`. Caps how many times the room starts a task. |
| `spec.wards.maxMessages` | integer | `0`, read as twice the number of vassals | Must not be negative. |
| `spec.wards.maxPingPong` | integer | `0`, read as twice the number of vassals | Must not be negative. |
| `spec.vassals` | list | none | At least one. |
| `spec.vassals[].role` | string | none | Required, unique in the room. No surrounding whitespace, no leading `@`, and not `operator`, `all` or `system`. |
| `spec.vassals[].workload` | string | none | Required, and must name a declared Workload. |
| `spec.vassals[].activation` | `always-on` or `on-mention` | `on-mention` | Any other value is an error. |
| `spec.vassals[].canMessage` | list of strings | empty | Each entry is `operator`, `all` or another vassal's role. A `sandbox` vassal cannot name a `runtime` vassal, and cannot use `all` while the room has any `runtime` vassal. |

Councils opened with `sablectl council` or `POST /api/v1/councils` do not use a template. Their settings are request fields, listed under `POST /api/v1/councils` in the [HTTP API reference](http-api.md#councils-and-conclaves).

## Validation errors

Source: `internal/config/loader.go`. The texts below are the fixed part of each message; the loader adds the file, document number or object name.

| Message | Cause |
| --- | --- |
| `unsupported apiVersion` | `apiVersion` is not `sablecourt.io/v1alpha1`. |
| `unsupported kind` | `kind` is not one of the six. |
| `duplicate <Kind> "<name>"` | Two documents of one kind share a name. |
| `<Kind> metadata.name is required` | Blank name. |
| `workload "<name>": name is reserved for the built-in council panel` | A Workload is named `claude`, `codex` or `agy`. |
| `no YAML manifest files found in "<dir>"` | A directory with no `.yaml` or `.yml` file. |
| `workload "<name>": metadata.zone must be "runtime" or "sandbox"` | Missing or unknown zone. |
| `workload "<name>": invalid class` | `spec.class` is not one of the three. |
| `workload "<name>": coding config is required for class "coding-agent"` | No `spec.coding`. |
| `workload "<name>": coding config is only valid for class "coding-agent"` | `spec.coding` on another class. |
| `workload "<name>": unsupported coding harness` | Harness is not `claude`, `codex` or `agy`. |
| `workload "<name>": coding.repoPath is required` | Empty `repoPath`. |
| `workload "<name>": deterministic-controller must not hold a Model Gate grant` | `modelGate.allowed` on a controller. |
| `workload "<name>": runtime.type must be docker or worktree-cli` | Missing or unknown runtime type. |
| `workload "<name>": runtime.image is required for docker runtime` | Docker runtime with no image. |
| `workload "<name>": sandbox zone workloads cannot hold mutating actions or delegation grants` | `actions` or `delegateTo` on a sandbox workload. |
| `workload "<name>": grants.modelGate.budgetRef is required when modelGate.allowed is true` | Allowed with no `budgetRef`. |
| `workload "<name>": referenced budget "<ref>" not found` | `budgetRef` names no Budget. |
| `workload "<name>": cannot delegate to itself` | `delegateTo` lists the workload. |
| `workload "<name>": delegateTo target "<target>" not found` | `delegateTo` names no Workload. |
| `budget "<name>": limits must be non-negative` | A negative limit. |
| `budget "<name>": at least one limit must be positive` | All limits are `0`. |
| `policy "<name>": defaultEffect must be "deny"` | `defaultEffect` is not `deny`. |
| `policy "<name>" rule <n>: workload and capability are required` | A rule field is blank. |
| `policy "<name>" rule <n>: invalid effect` | Effect is not `allow`, `approve` or `deny`. |
| `workflow "<name>": invalid zone` | `metadata.zone` is set to something other than `runtime` or `sandbox`. |
| `workflow "<name>": at least one step is required` | No steps. |
| `workflow "<name>" step <n>: name is required` | A step has a blank name. |
| `workflow "<name>" step "<step>": workload "<ref>" not found` | Step names no Workload. |
| `workflow "<name>" step "<step>": deterministic-controller cannot specify provider or model` | Provider or model on a controller step. |
| `workflow "<name>" step "<step>": unknown model tier` | `tier` is not `fast`, `coding` or `critic`. |
| `workflow "<name>" step "<step>": invalid tier provider or model` | The route id is empty, too long, starts with `-` or has other characters. |
| `workflow "<name>" step "<step>": deterministic workload cannot use a model tier` | `tier` on a `deterministic-controller` step. |
| `workflow "<name>" step "<step>": tier model is incompatible with the workload harness` | The route does not fit the `coding-agent` harness. |
| `schedule "<name>": invalid cron expression` | The cron text does not parse. |
| `schedule "<name>": targetKind must be workload or workflow` | Missing or unknown target kind. |
| `schedule "<name>": target workload "<ref>" not found` | Target names no Workload (and likewise for a workflow). |
| `conclave "<name>": at least one vassal is required` | No vassals. |
| `conclave "<name>": referenced budget "<ref>" not found` | `budgetRef` names no Budget. |
| `conclave "<name>": maxRuns must be between 0 and 6` | `maxRuns` out of range. |
| `conclave "<name>": ward limits must be non-negative` | A negative `maxMessages` or `maxPingPong`. |
| `conclave "<name>" vassal <n>: invalid role` | Role is blank, padded, starts with `@`, or is a reserved word. |
| `conclave "<name>": duplicate vassal role` | Two vassals share a role. |
| `conclave "<name>" vassal "<role>": workload "<ref>" not found` | Vassal names no Workload. |
| `conclave "<name>" vassal "<role>": invalid activation` | Activation is not `always-on` or `on-mention`. |
| `conclave "<name>" vassal "<role>": canMessage target "<target>" not found` | `canMessage` names no vassal. |
| `conclave "<name>" vassal "<role>": sandbox workload cannot message runtime vassal` | A sandbox vassal reaches a runtime vassal, directly or through `all`. |

## In depth

- [Concepts](../concepts.md) and the [glossary](../glossary.md) for workload, zone, policy, budget and Conclave.
- [Workloads and runtimes](../how-it-works/workloads-and-runtimes.md), [model gate](../how-it-works/model-gate.md), [tools and actions](../how-it-works/tools-and-actions.md) and [conclaves](../how-it-works/conclaves.md) for what each field does at run time.
- [ADR-0002](../adr/0002-heterogeneous-mvp-workload-adapters.md), [ADR-0003](../adr/0003-hybrid-workload-isolation.md) and [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md) for the decisions behind classes, zones and grants.
