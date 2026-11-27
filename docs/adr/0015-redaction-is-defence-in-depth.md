# ADR-0015: Redaction is defence in depth, not a boundary

- Date: 2026-11-27
- Status: Proposed

## Context

Invariant 3 in `AGENTS.md` says that repository files, logs, tool results and peer messages "are redacted at boundaries". Read literally, that makes the pattern redactor a security boundary. The code and three review passes say it is not one.

The redactor is a list of regular expressions for known secret shapes: private key blocks, Sablecourt task and operator tokens, a few provider key prefixes, bearer headers and `key = value` pairs whose key looks sensitive ([internal/redact/redact.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/redact/redact.go), lines 17 to 69). A secret in any other shape passes through. Three review passes on [issue 126](https://github.com/shadewright-labs/sablecourt/issues/126) each found new bypasses of one kind. The approach was brought back for a decision instead of a fourth round.

Redaction also does not run on the paths where repository content leaves the machine:

- The model gate reads the request body and forwards the same bytes upstream ([internal/gate/model/gate.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/gate/model/gate.go), lines 225 and 286; the fallback path at line 425 does the same). The package does not import the redactor.
- The council proxy handles provider traffic as an HTTPS `CONNECT` tunnel. It checks the host against the harness's provider list, then copies bytes in both directions ([internal/gate/councilproxy/proxy.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/gate/councilproxy/proxy.go), lines 302 to 307 and 409 to 433). It cannot see the content inside the TLS session.

So any file a harness reads from its checkout can reach the provider, whatever the redactor would have done to it.

Redaction does run where Sablecourt itself writes text: coding output ([internal/runtime/coding/output.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/runtime/coding/output.go), line 106), council artifacts and diffs ([internal/runtime/coding/council.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/runtime/coding/council.go), lines 1511 and 1610), Conclave posts ([internal/conclave/broker.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/conclave/broker.go), line 328) and context packets ([internal/contextpack/assembler.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/contextpack/assembler.go), line 166). On those paths it lowers the chance that a known secret shape lands in the store, the UI or a peer's prompt.

The daemon holds secrets whose exact values it knows: the operator token, task leases and any provider or Git credentials behind the gates. [Issue 180](https://github.com/shadewright-labs/sablecourt/issues/180) adds exact-match redaction of those values on every redacting path. That match does not depend on the shape of the secret, so it can be tested as a guarantee. It is not on `main` yet.

Host mode is covered by the 2026-11-27 amendment to [ADR-0011](0011-sanctum-scope-trust-and-cli-harnesses.md#amendment-2026-11-27-container-only-harnesses-for-010). In short, coding tasks and council seats run in harness containers for 0.1.0, and host mode, if kept, is off by default and outside invariants 2 and 3.

## Decision

The pattern redactor is a best-effort filter. It is defence in depth and Sablecourt does not describe it as a boundary.

What invariant 3 guarantees after this ADR:

- Exact-match redaction of every daemon-held secret on every redacting path, once issue 180 lands.
- Workloads cannot reach daemon-held secrets. This holds in container mode only. The subscription login in each harness's own volume remains the one exception to invariant 2, as ADR-0011 records.
- Untrusted input never widens policies or the action catalog. This part of invariant 3 is unchanged.

What it does not guarantee:

- Pattern rules catch no fixed set of secrets. Docs describe them only through examples generated from test rows in `internal/redact`, never by a list of what they "catch".
- Repository content a harness reads reaches the provider. The model gate forwards request bodies as sent, and the council proxy tunnels TLS it cannot read.
- Secrets inside the checkout itself, such as a `.env` file or a Git config with an embedded token, are outside invariant 2 in every mode. The harness can read its checkout, so it can read them. The docs say so.
- In host mode, when forced on, redaction is the only control and guarantees nothing.

| Guarantee | Container mode | Forced host mode |
| --- | --- | --- |
| Exact-match redaction of daemon-held secrets on redacting paths (issue 180) | Holds | Holds on the same paths, but nothing stops the harness from reading the secret elsewhere |
| Workload cannot reach daemon-held secrets (invariant 2) | Holds, except the harness's own subscription login | Does not hold |
| Harness cannot read secrets inside its own checkout | Does not hold | Does not hold |
| Pattern redaction of other secret shapes | Best effort | Best effort, and the only control |
| Repository content stays on the machine | Does not hold | Does not hold |
| Untrusted input never widens policy or the action catalog | Holds | Holds |

Per-format parsers (for `.env`, Git config, JSON and similar files) are deferred. They are revisited only if a path needs a guarantee that exact match cannot give.

### Proposed wording of invariant 3

Once this ADR is accepted, replace item 3 under "Hard invariants" in `AGENTS.md` with:

> 3. All external input is untrusted. Repository files, logs, tool results, and peer messages never widen policies or the action catalog. Every path that redacts replaces each secret the daemon holds by exact match; pattern rules for other secret shapes are best effort, not a boundary. Content a workload reads, including secrets inside its own checkout, can reach its model provider.

## Alternatives considered

Treat pattern rules as a boundary and keep fixing bypasses. Three review passes each found new cases of one kind, so the rule set does not converge. Each fix also widens the rules toward false positives in ordinary text. A boundary that a fourth pass is expected to break is not one, and calling it one would mislead users about what reaches the provider.

Write per-format parsers now. Parsers for `.env`, Git config and JSON would find secrets by position instead of shape. They would still miss formats nobody wrote a parser for, each parser is new attack surface under the parser test rule, and none of them helps on the model gate or the proxy tunnel, where content is not redacted at all. Exact match covers the secrets the daemon is responsible for at a fraction of the cost.

Allow-list what leaves. Sablecourt could send only content it has approved to the provider. The harness CLIs read files and build their own requests, and the council proxy sees only a TLS tunnel, so Sablecourt cannot inspect or filter those requests without terminating TLS inside the harness's provider session. That would mean holding or forging provider trust inside each container, which ADR-0011 rules out, and it would break the CLIs on every protocol change.

## Consequences

Users must not keep live secrets in a checkout they hand to a harness. The harness docs state this limit next to the setup steps.

Docs that describe redaction show examples generated from the test rows of `internal/redact` and say that the rules are best effort. Pages that list "what redaction catches" as a promise are rewritten.

Issue 180 becomes the work that makes invariant 3 testable: a table test per redacting path that feeds each daemon-held secret in and asserts it does not come out.

Container mode carries invariant 2 for 0.1.0. Any later path that runs a harness outside a container needs its own amendment that states which guarantees hold there.

Bypasses of the pattern rules are still fixed when cheap, as defence in depth, but a bypass is no longer a security defect by itself. A leak of a daemon-held secret on a redacting path is one.
