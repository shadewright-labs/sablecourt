# Tools and actions

The MCP Gate (also called the Data and Action Gate) serves the tools a workload is granted and runs a small closed set of typed actions, after the policy engine has said yes. It is the only way a workload reads Court Memory or changes anything outside its own checkout.

Status: built. The upstream MCP bridge is built and parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md). Several faults are known and tracked in issue #9 (see [Known gaps](#known-gaps)).

## Two routes

| Route | What it serves | Credential |
| --- | --- | --- |
| `/gate/mcp` | MCP over Streamable HTTP, stateless. `tools/list` returns only the tools named in the workload's `grants.mcpTools` that the gate has registered. | Task lease as a bearer token |
| `/gate/actions/<name>` | `POST` of one typed action with `{"payload": ..., "approvalId": ...}`. The action must be in the catalog; an unknown name is 404 and audited. | Task lease |

The lease must be active, and its workload must be declared in the loaded manifests. The action body is limited to 2 MiB, unknown fields are refused. The action path is `/gate/actions/<name>`.

Built-in tools: `sablecourt.list_workloads` and four memory tools, `memory.put`, `memory.get`, `memory.search` and `memory.delete`. `memory.put` redacts the value it stores (not the key), namespaces `learning:*` and `system:*` are closed to the tools, and `search` is a substring match (SQL `LIKE`) on keys and values. Built-in actions: `git.create_branch` (local branch, name must start with `sablecourt/`), `git.open_pr` (opens a GitHub pull request with the gate's token; head must start with `sablecourt/`) and `sandbox.propose_promotion` (checks that the workload is in the sandbox zone and returns the next step; it changes nothing).

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

`internal/redact` is a best-effort filter, not a guarantee. It provides defence in depth on the text paths it covers: MCP tool and action results, session output, council prompt, messages and artifacts, notifications and the Shadewright export.

Request bodies sent to a model through the Model Gate are forwarded as sent and are not redacted (`internal/gate/model/gate.go`), and the council proxy is an HTTPS `CONNECT` tunnel that cannot see content. Repository content a harness reads therefore reaches the model provider. An exact-match guarantee for daemon-held secrets is tracked in [issue 180](https://github.com/shadewright-labs/sablecourt/issues/180).

The table below is generated from test cases in `internal/redact/examples_doc_test.go`, with secret values shortened for display:

<!-- redact-examples:start -->

| Case | Input | Output |
| --- | --- | --- |
| PEM private key block | `-----BEGIN RSA ...-----\n...\n-----END ...` | `[REDACTED:private_key]` |
| Prefixed token | `key sk-ant-...` | `key [REDACTED:anthropic_key]` |
| URL userinfo password | `https://alice:...@example.com/p` | `https://[REDACTED:url_credential]@example.com/p` |
| Authorization header | `Authorization: Bearer ...` | `Authorization: Bearer [REDACTED:bearer_token]` |
| Cookie header | `Cookie: sid=...` | `Cookie: [REDACTED:cookie]` |
| JWT | `jwt eyJ... .eyJ... .sig...` | `jwt [REDACTED:jwt]` |
| Key=value secret | `db_password=...` | `db_password=[REDACTED:secret]` |
| Non-secret kept | `region: europe-west4` | `region: europe-west4` |
| Status prose kept | `Authorization: skipping verification` | `Authorization: skipping verification` |
| Known gap: key synonym leaks | `SECRET_KEY_BASE=<value>` | `SECRET_KEY_BASE=<value>` |
| Value in parentheses | `password=(...)` | `password=[REDACTED:secret]` |
| Known gap: logfmt auth header leaks | `authorization=Bearer <value>` | `authorization=Bearer <value>` |
| Known gap: scheme-less Authorization leaks | `Authorization: <value>` | `Authorization: <value>` |
| Known gap: short unquoted value leaks | `password=abc12` | `password=abc12` |
| Known gap: unquoted passphrase keeps later words | `passphrase: correct horse battery staple` | `passphrase: [REDACTED:secret] horse battery staple` |
| Known gap: CRLF-split GitHub token leaks tail | `ghp_...\r\n<tail>` | `[REDACTED:github_token]\r\n<tail>` |
| Known gap: env var reference over-redacted | `password: ${DB_PASSWORD}` | `password: [REDACTED:secret]` |
| Known gap: -tokens suffix over-redacted | `max-tokens=123456` | `max-tokens=[REDACTED:secret]` |

<!-- redact-examples:end -->

### Known redaction gaps

These gaps are known and accepted today. The list is generated from the rows that pin them in `internal/redact` (`TestKnownGaps` and `TestCorpusKnownGaps`), and `TestKnownGapsDoc` fails when the list and the pinned rows differ.

<!-- redact-gaps:start -->

1. Key synonyms are not redacted: `SECRET_KEY_BASE=...`, `ENCRYPTION_KEY=...` and `master_key: ...` keep their value.
2. A no-break space (U+00A0) between key and value is not a separator, so `password:` followed by a no-break space and a value keeps the value.
3. An unquoted value stops at `&`, which reads as the start of the next query parameter: `password=&value` keeps the value, and so does `password=abc&value`, because `abc` is shorter than six characters.
4. Double-escaped JSON, a JSON document embedded in a JSON string so that its quotes are escaped twice, keeps its secret fields.
5. A `Cookie` header folded onto a continuation line (`Cookie:` then a line break) keeps its value.
6. Logfmt pairs with `=` instead of `:` are not header matches: `authorization=Bearer ...` and `cookie="sid=..."` keep their value.
7. An `Authorization` header with no scheme word (`Authorization: value`) keeps its value.
8. `curl --proxy-user user:value` and the cookie flags `-b` and `--cookie` keep their value.
9. An unquoted value is redacted only up to its first whitespace, and only when it is six or more characters, so later words of a passphrase and short values stay.
10. When a secret value contains a token that is redacted first, the text after the marker stays: `password=<value>-<aws key>xyz` ends in `[REDACTED:aws_access_key]xyz`.
11. A GitHub token split by a line break can leak: with 20 characters after the prefix before a CRLF, the tail after the break stays; with only the prefix before the break, the characters after it stay.
12. A quoted value that is cut short or spans lines can leak: an unterminated `password="abc12` keeps `abc12`, and a multiline double-quoted value stops at a continuation line that holds `:` or `=` (`password="abc`, a new line, `def:ghi"` keeps `ghi`).
13. A line break between key and separator (`password`, a new line, `= value`) or after the separator keeps the value when the key does not start a YAML line: `password=`, `x secret :` or `{"password":`, then the value on the next line. A YAML value on the line after a `password:` line is redacted.
14. After a `:` separator, a lowercase word of up to 15 letters that ends like a status word (`-ed`, `-ing`, `-tion`, `-sion`, `-ment`, `-ness`, `-able`, `-ible`) is read as log prose and kept: `password: dragonslaying` keeps its value.
15. An unquoted value shaped like a function call is kept: in `f(password=Xy9(value)` the value reads as a call to `Xy9`.
16. An unquoted value shaped like a file path (starting with `/`, `./`, `../` or `~/`, with short path segments) is kept: `password=/x9/value` keeps it.
17. A provider token split by a space leaks the part after the space: only the characters before the space match the token shape.
18. A URL password that holds both `@` and `/` leaks its tail: in `postgres://app:p@ss/word@example.com` the text from the first `@` on reads as a host and a path.
19. An unlabelled opaque hex string (`deploy <40 hex characters>`) is kept, because it cannot be told apart from a Git SHA, a UUID or image data.
20. In a `{` or `[` collection that spans lines, a line indented no deeper than the line that opened it ends the collection unless it starts with `}`, `]` or `,`, so an item on such a line keeps its value. This keeps an unclosed collection from hiding the rest of the output.
21. Redaction is not always idempotent when invalid UTF-8 or NUL bytes sit next to quotes: redacting the output again can redact more of it (`seCret:'[REDACTED:secret]'`, an invalid byte and `'` lose the text after the marker). No secret text comes back.
22. A key name that ends in `-tokens` is treated like `tokens`, so `max-tokens=123456` loses its value.
23. Variable references and secret names are over-redacted: `password: ${DB_PASSWORD}`, `token=$API_TOKEN`, `Authorization: Bearer $API_TOKEN` and `existingSecret: postgres-secret` lose their text.
24. Go short assignments from an expression are over-redacted in diffs and source: `password := cfg.Password` loses `cfg.Password`.

<!-- redact-gaps:end -->

## Audit rows

The gate appends a row, category `data_action_gate`, for each denial, error and allowed call, for an unknown action and for an approval mismatch; the memory tools add rows in category `memory`, including refusals of reserved namespaces. Creating an approval writes a row in category `approval`. A tool call that returns `approval_required` writes nothing in the gate itself.

## Known gaps

What the code does today, where it falls short of the design.

- Tool calls whose effect is `approve` cannot complete: each call creates a new pending approval and there is no way to present an approved one. Only actions can be approved and re-run.
- An approved action can be repeated for the life of the lease: the approval is not marked used and its expiry is not checked at use. Making it single-use is decided in [ADR-0014](../adr/0014-identity-and-authentication.md), Proposed.
- The circuit breaker never opens, because the gate and the policy engine hold separate breaker objects.
- `git.open_pr` does not validate `owner`, `repo` or `base`. There is no action that commits or pushes, so the branch, commit and pull request path of [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md) stops at the branch and the pull request call.
- The sandbox-zone rule covers `action:` and `delegate:` only; a mutating MCP tool such as `memory.put` is open to a sandbox workload when policy allows it.
- Line-at-a-time output callers such as `internal/runtime/coding/output.go` call stateless `redact.String` rather than `LineRedactor`, so secrets split across lines in session output can pass through.

## Not built yet

- Parked by ADR-0011: upstream MCP servers (`grants.mcpServers`) are bridged with an injected secret and a 4 MiB response cap, but are not part of the current scope.

## In depth

- [Approvals](approvals.md), [model gate](model-gate.md), concepts (page planned)
- [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md), [ADR-0005](../adr/0005-dual-path-human-in-the-loop.md)
- Code: `internal/gate/mcp/gate.go`, `internal/gate/mcp/actions.go`, `internal/gate/mcp/memory.go`, `internal/policy/policy.go`, `internal/redact/redact.go`
