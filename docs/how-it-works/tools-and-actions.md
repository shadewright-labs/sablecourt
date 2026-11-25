# Tools and actions

The MCP Gate (also called the Data and Action Gate) serves the tools a workload is granted and runs a small closed set of typed actions, after the policy engine has said yes. It is the only way a workload reads Court Memory or changes anything outside its own checkout.

Status: built. The upstream MCP bridge is built and parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md). Several faults are known and tracked in issue #9 (see [Known gaps](#known-gaps)).

## Two routes

| Route | What it serves | Credential |
| --- | --- | --- |
| `/gate/mcp` | MCP over Streamable HTTP, stateless. `tools/list` returns only the tools named in the workload's `grants.mcpTools` that the gate has registered. | Task lease as a bearer token |
| `/gate/actions/<name>` | `POST` of one typed action with `{"payload": ..., "approvalId": ...}`. The action must be in the catalog; an unknown name is 404 and audited. | Task lease |

The lease must be active, and its workload must be declared in the loaded manifests. The action body is limited to 2 MiB, unknown fields are refused. The action path is `/gate/actions/<name>`.

Built-in tools: `sablecourt.list_workloads` and four memory tools, `memory.put`, `memory.get`, `memory.search` and `memory.delete`. Memory writes are redacted, namespaces `learning:*` and `system:*` are closed to the tools, and `search` is a substring match (SQL `LIKE`) on keys and values. Built-in actions: `git.create_branch` (local branch, name must start with `sablecourt/`), `git.open_pr` (opens a GitHub pull request with the gate's token; head must start with `sablecourt/`) and `sandbox.propose_promotion` (checks that the workload is in the sandbox zone and returns the next step; it changes nothing).

## How a call is decided

The capability string is `mcp:<tool>`, `action:<name>` or `delegate:<workload>`. The engine (`internal/policy`) applies these steps in order and the first match wins:

[![One tool or action call: fixed checks, then policy rules lead to the handler, an approval in the queue, or a denial.](../diagrams/policy-decision.svg)](../diagrams/policy-decision.svg "Open full size")

A rule names a workload and a capability, each exact, `*`, or a prefix ending in `*`. All matching rules are read: one `deny` wins, then `approve`, then `allow`; no match means `default_deny`. A `Policy` whose `defaultEffect` is not `deny` is rejected at load. A missing engine, an empty name or an unknown effect also deny. The reason string (`explicit_policy_deny`, `capability_not_granted_on_workload`, `sandbox_zone_forbids_mutation`, `circuit_breaker_open`, `default_deny` and others) goes into the audit row and the error the workload sees.

- allow: the handler runs. The result is redacted before it is returned, and the audit row records the number of redaction hits. Arguments are not redacted on the way in.
- approve: for an action without `approvalId`, an approval is created with a 15 minute lifetime, the task moves to `input-required`, and the gate answers 202 with the approval ID and payload digest. The workload repeats the call with the approved ID; the gate runs it only if the approval is `approved` and matches the same task, the same capability and the SHA-256 of the same payload (see [approvals](approvals.md)).
- deny: 403 for an action, an error result for a tool, plus an audit row.

## Circuit breaker

Each action that fails records a failure for its capability; three in a row open the breaker for 2 minutes, and a success resets it. An open breaker makes the engine deny `action:` calls with `circuit_breaker_open`. In the current wiring the gate records failures on a breaker object that the policy engine does not check, so the breaker never opens (see [Known gaps](#known-gaps)).

## Redaction

`internal/redact` replaces private keys, Sablecourt tokens, Anthropic, OpenAI, GitHub, Google and AWS keys, bearer headers and secret-looking `key: value` pairs, in linear time. The gate applies it to every tool and action result and to error messages, Court Memory writes redact their value, and the coding adapter redacts task input. It does not run on tool arguments or on Model Gate bodies.

## Audit rows

The gate appends a row, category `data_action_gate`, for each denial, error and allowed call, for an unknown action and for an approval mismatch; the memory tools add rows in category `memory`, including refusals of reserved namespaces. Creating an approval writes a row in category `approval`. A tool call that returns `approval_required` writes nothing in the gate itself.

## Known gaps

What the code does today, where it falls short of the design.

- Tool calls whose effect is `approve` cannot complete: each call creates a new pending approval and there is no way to present an approved one. Only actions can be approved and re-run.
- An approved action can be repeated for the life of the lease: the approval is not marked used and its expiry is not checked at use. Making it single-use is decided in [ADR-0014](../adr/0014-identity-and-authentication.md), Proposed.
- The circuit breaker never opens, because the gate and the policy engine hold separate breaker objects.
- `git.open_pr` does not validate `owner`, `repo` or `base`. There is no action that commits or pushes, so the branch, commit and pull request path of [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md) stops at the branch and the pull request call.
- The sandbox-zone rule covers `action:` and `delegate:` only; a mutating MCP tool such as `memory.put` is open to a sandbox workload when policy allows it.

## Not built yet

- Parked by ADR-0011: upstream MCP servers (`grants.mcpServers`) are bridged with an injected secret and a 4 MiB response cap, but are not part of the current scope.

## In depth

- [Approvals](approvals.md), [model gate](model-gate.md), [concepts](../concepts.md)
- [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md), [ADR-0005](../adr/0005-dual-path-human-in-the-loop.md)
- Code: `internal/gate/mcp/gate.go`, `internal/gate/mcp/actions.go`, `internal/gate/mcp/memory.go`, `internal/policy/policy.go`, `internal/redact/redact.go`
