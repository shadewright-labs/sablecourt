# Tools and actions

The MCP Gate (also called the Data and Action Gate) serves the tools a workload is granted and runs a small closed set of typed actions, after the policy engine has said yes. It is the only way a workload reads Court Memory or changes anything outside its own checkout.

Status: built. The upstream MCP bridge is built and parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md). Several faults are known and tracked in issue #9 (see [Known gaps](#known-gaps)).

## Two routes

| Route | What it serves | Credential |
| --- | --- | --- |
| `/gate/mcp` | MCP over Streamable HTTP, stateless. `tools/list` returns only the tools named in the workload's `grants.mcpTools` that the gate has registered. | Task lease as a bearer token |
| `/gate/actions/<name>` | `POST` of one typed action with `{"payload": ..., "approvalId": ...}`. The action must be in the catalog; an unknown name is 404 and audited. | Task lease |

The lease must be active, and its workload must be declared in the loaded manifests. The action body is limited to 2 MiB, unknown fields are refused. The action path is `/gate/actions/<name>`.

Built-in tools: `sablecourt.list_workloads` and four memory tools, `memory.put`, `memory.get`, `memory.search` and `memory.delete`. Memory writes are redacted, namespaces `learning:*` and `system:*` are closed to the tools, and `search` is a substring match (SQL `LIKE`) on keys and values. Built-in actions: `git.create_branch` (local branch, name must start with `sablecourt/`), `git.open_pr` (opens a GitHub pull request with the gate's token; head must start with `sablecourt/`, and owner and repository must be the GitHub repository pinned at start, see below) and `sandbox.propose_promotion` (checks that the workload is in the sandbox zone and returns the next step; it changes nothing).

Each action payload is decoded once, strictly, into the action's typed input: a duplicate key, a key that differs from a field only in case, an unknown field, a `null` value, text that is not UTF-8, a `\u` escape of an unpaired surrogate (which the decoder would turn into U+FFFD) or any data after the object is refused with 400 before an approval exists. The approval digest, the dialog and the executor all use that one decoded value. Branch and base names are checked as Git ref names and used as given, nothing trimmed, and `git.create_branch` needs an explicit `base`. The gate accepts a narrower set than Git: at most 200 bytes, each `/`-separated part starts with an ASCII letter or digit and holds only letters, digits, `.`, `_` and `-`, no part ends in `.` or `.lock`, and no `..` or `HEAD`. A base such as `release/_candidate`, which Git accepts, is refused with 400. A body over 2 MiB is refused with 413.

## Which repository `git.open_pr` may target

When the daemon starts it reads the `origin` remote of its repo root once and pins that GitHub `owner/repo`. `git.open_pr` refuses any other target with 403 when the payload is checked, before an approval is requested or spent, refuses every call when the origin read at call time differs from the pin, and refuses every call when nothing could be pinned (no origin, or an origin that is not on github.com; GitHub's SSH over port 443, user `git` at host `ssh.github.com` port 443, is accepted). Each action's audit row names the pin as `allowed_repo`. A reload (`POST /api/v1/reload`, a manifest saved in the web UI, or a manifests git sync) never changes the pin: it reads the origin again only to compare it. A changed or unreadable origin logs a warning on each reload and writes one `denied` audit row with reason `repo_origin_changed` or `repo_origin_unreadable` each time the observed origin changes. A row that cannot be written stays owed, and later reloads write it in order, even after the origin has changed again. To point `git.open_pr` at another repository, change the origin and restart the daemon.

## How a call is decided

The capability string is `mcp:<tool>`, `action:<name>` or `delegate:<workload>`. The engine (`internal/policy`) applies these steps in order and the first match wins:

[![One tool or action call: fixed checks, then policy rules lead to the handler, an approval in the queue, or a denial.](../diagrams/policy-decision.svg)](../diagrams/policy-decision.svg "Open full size")

A rule names a workload and a capability, each exact, `*`, or a prefix ending in `*`. All matching rules are read: one `deny` wins, then `approve`, then `allow`; no match means `default_deny`. A `Policy` whose `defaultEffect` is not `deny` is rejected at load. A missing engine, an empty name or an unknown effect also deny. The reason string (`explicit_policy_deny`, `capability_not_granted_on_workload`, `sandbox_zone_forbids_mutation`, `circuit_breaker_open`, `default_deny` and others) goes into the audit row and the error the workload sees.

- allow: the handler runs. The result is redacted before it is returned, and the audit row records the number of redaction hits. Arguments are not redacted on the way in.
- approve: for an action without `approvalId`, an approval is created with a 15 minute lifetime, the task moves to `input-required`, and the gate answers 202 with the approval ID and payload digest. The workload repeats the call with the approved ID; the gate runs it only if the approval is `approved` and matches the same task, the same capability and the SHA-256 of the same payload (see [approvals](approvals.md)).
- deny: 403 for an action, an error result for a tool, plus an audit row.

## Circuit breaker

Each action that fails records a failure for its capability, including a refused target and, on an allowed request, a refused payload; three in a row open the breaker for 2 minutes, and a success resets it. An open breaker makes the engine deny `action:` calls with `circuit_breaker_open`. In the current wiring the gate records failures on a breaker object that the policy engine does not check, so the breaker never opens (see [Known gaps](#known-gaps)).

## Redaction

`internal/redact` replaces private keys, Sablecourt tokens, Anthropic, OpenAI, GitHub, Google and AWS keys, bearer headers and secret-looking `key: value` pairs, in linear time. The gate applies it to every tool and action result and to error messages, Court Memory writes redact their value, and the coding adapter redacts task input. It does not run on tool arguments or on Model Gate bodies.

## Audit rows

The gate appends a row, category `data_action_gate`, for each denial, error and allowed call, for an unknown action and for an approval mismatch; the memory tools add rows in category `memory`, including refusals of reserved namespaces. Creating an approval writes a row in category `approval`. A tool call that returns `approval_required` writes nothing in the gate itself.

## Known gaps

What the code does today, where it falls short of the design.

- Tool calls whose effect is `approve` cannot complete: each call creates a new pending approval and there is no way to present an approved one. Only actions can be approved and re-run.
- The circuit breaker never opens, because the gate and the policy engine hold separate breaker objects.
- There is no action that commits or pushes, so the branch, commit and pull request path of [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md) stops at the branch and the pull request call.
- The sandbox-zone rule covers `action:` and `delegate:` only; a mutating MCP tool such as `memory.put` is open to a sandbox workload when policy allows it.

## Not built yet

- Parked by ADR-0011: upstream MCP servers (`grants.mcpServers`) are bridged with an injected secret and a 4 MiB response cap, but are not part of the current scope.

## In depth

- [Approvals](approvals.md), [model gate](model-gate.md), concepts (page planned)
- [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md), [ADR-0005](../adr/0005-dual-path-human-in-the-loop.md)
- Code: `internal/gate/mcp/gate.go`, `internal/gate/mcp/actions.go`, `internal/gate/mcp/memory.go`, `internal/policy/policy.go`, `internal/redact/redact.go`
