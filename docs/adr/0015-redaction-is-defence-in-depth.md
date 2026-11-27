# ADR-0015: Redaction is defence in depth, not a boundary

- Date: 2026-11-27
- Status: Accepted (2026-11-27)

## Context

Invariant 3 in `AGENTS.md` says that repository files, logs, tool results and peer messages "are redacted at boundaries". It does not say what that redaction guarantees. Read as a promise that no secret gets through, it makes the pattern redactor a security boundary. The code and three review passes say it is not one.

The redactor is a list of regular expressions for known secret shapes: private key blocks, Sablecourt task and operator tokens, a few provider key prefixes, bearer headers and `key = value` pairs whose key looks sensitive ([internal/redact/redact.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/redact/redact.go), lines 18 to 69). A secret in any other shape passes through. Three review passes on [issue 126](https://github.com/shadewright-labs/sablecourt/issues/126) each found new bypasses of one kind. The approach was brought back for a decision instead of a fourth round.

Redaction also does not run on the paths where repository content leaves the machine:

- The model gate reads the request body and forwards the same bytes upstream ([internal/gate/model/gate.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/gate/model/gate.go), lines 225 and 286; the fallback path at line 425 does the same). The package does not import the redactor.
- The council proxy handles provider traffic as an HTTPS `CONNECT` tunnel. It checks the host against the harness's provider list, then copies bytes in both directions ([internal/gate/councilproxy/proxy.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/gate/councilproxy/proxy.go), lines 368 to 374 and 409 to 433). It cannot see the content inside the TLS session.

So any file a harness reads from its checkout can reach the provider, whatever the redactor would have done to it.

Redaction does run where Sablecourt itself writes text: coding output ([internal/runtime/coding/output.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/runtime/coding/output.go), line 106), council artifacts and diffs ([internal/runtime/coding/council.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/runtime/coding/council.go), lines 1511 and 1611), Conclave posts ([internal/conclave/broker.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/conclave/broker.go), line 328) and context packets ([internal/contextpack/assembler.go](https://github.com/shadewright-labs/sablecourt/blob/main/internal/contextpack/assembler.go), line 166). On those paths it lowers the chance that a known secret shape lands in the store, the UI or a peer's prompt.

The daemon process holds some secrets in memory and knows their exact values: the operator token, the task leases it issued since it started, and any provider or Git credentials it has loaded behind the gates. [Issue 180](https://github.com/shadewright-labs/sablecourt/issues/180) adds exact-match redaction of those values at the redacting boundaries inside the daemon. That match does not depend on the shape of the secret, so it can be tested as a guarantee. It is not on `main` yet.

Some text is outside what exact match can cover. Detached session wrappers write coding output without ever receiving daemon secrets, so they have nothing to match against. Leases issued before a daemon restart survive only as hashes, so the daemon no longer knows their values. A workload necessarily holds its own task lease, so no redaction keeps that lease from that workload.

Host mode is covered by the 2026-11-27 amendment to [ADR-0011](0011-sanctum-scope-trust-and-cli-harnesses.md#amendment-2026-11-27-container-only-harnesses-for-010). In short, coding tasks and council seats run in harness containers for 0.1.0, and host mode, if kept, is off by default and outside invariants 2 and 3.

## Decision

The pattern redactor is a best-effort filter. It is defence in depth and Sablecourt does not describe it as a boundary.

The boundaries that must redact stay as the accepted ADRs and the design document name them: Data and Action Gate responses before they reach a workload, Conclave messages, Relics and Scry output before persistence or broadcast, context packets and repository pre-checks, learning statements and their export, and council peer excerpts and writer diffs. This ADR changes what redaction at those boundaries guarantees. It does not remove any of them.

What invariant 3 guarantees after this ADR:

- At each redacting boundary inside the daemon, every secret the daemon process holds in memory is replaced by exact match. This is the target of issue 180 and does not hold on `main` yet.
- Workloads cannot reach daemon-held secrets. This holds in container mode only, with two exceptions. The subscription login in each harness's own volume remains the one exception to invariant 2, as ADR-0011 records. The workload also holds its own task lease, which is the scoped, short-lived identity invariant 2 allows.
- Untrusted input never widens policies or the action catalog. This part of invariant 3 is unchanged.

What it does not guarantee:

- Exact match does not cover text written by detached wrappers, leases persisted only as hashes, or secrets the daemon has never loaded.
- Pattern rules catch no fixed set of secrets. Docs describe them only through examples generated from test rows in `internal/redact`, never by a list of what they "catch".
- Repository content a harness reads reaches the provider. The model gate forwards request bodies as sent, and the council proxy tunnels TLS it cannot read.
- Secrets inside the checkout itself, such as a `.env` file or a Git config with an embedded token, are outside invariant 2 in every mode. The harness can read its checkout, so it can read them. The docs say so.
- In host mode, when forced on, redaction is the only control and guarantees nothing.

| Guarantee | Container mode | Forced host mode |
| --- | --- | --- |
| Exact-match redaction of secrets the daemon holds in memory, at redacting boundaries inside the daemon | Target of issue 180, not on `main` yet | Same target on the same boundaries, but nothing stops the harness from reading the secret elsewhere |
| Workload cannot reach daemon-held secrets (invariant 2) | Holds for council seats, except the harness's own subscription login and the workload's own task lease | Does not hold |
| Harness cannot read secrets inside its own checkout | Does not hold | Does not hold |
| Pattern redaction of other secret shapes | Best effort | Best effort, and the only control |
| Repository content stays on the machine | Does not hold | Does not hold |
| Untrusted input never widens policy or the action catalog | Holds | Holds |

On `main`, only council seats run in harness containers with checks on the checkout they mount. Standalone coding tasks still run on the host, so issue 181 (the decision in [issue 132](https://github.com/shadewright-labs/sablecourt/issues/132)) must move them onto the council container path, with the same checkout checks, before the container-mode column covers them.

Per-format parsers (for `.env`, Git config, JSON and similar files) are deferred. They are revisited only if a path needs a guarantee that exact match cannot give.

### Wording of invariant 3

Item 3 under "Hard invariants" in `AGENTS.md` reads:

> 3. All external input is untrusted. Repository files, logs, tool results, and peer messages never widen policies or the action catalog, and they are redacted at boundaries: Data and Action Gate responses to workloads, Conclave messages, Relics and Scry output, context packets and pre-checks, learnings, and council peer excerpts and diffs. At those boundaries inside the daemon, each secret the daemon process holds in memory is replaced by exact match; pattern rules for other secret shapes are best effort, not a boundary. Exact match does not reach detached wrappers that never receive daemon secrets, leases kept only as hashes, or a workload's own task lease, which it necessarily holds. Content a workload reads, including secrets inside its own checkout, can reach its model provider.

## Relation to earlier ADRs

The accepted text of earlier ADRs and of `docs/design.md` stays as written. Where it disagrees with this ADR, this ADR wins.

- [ADR-0003](0003-hybrid-workload-isolation.md), lines 15 and 26 (coding CLI agents under Seatbelt or Bubblewrap on the host): superseded for 0.1.0 by the 2026-11-27 amendment to ADR-0011, which runs coding tasks and council seats in harness containers only.
- [ADR-0004](0004-in-house-capability-gates-three-doors.md), line 15 (the Data and Action Gate redacts responses at the gate boundary): kept as a required boundary. Refined: the guarantee there is exact match of secrets the daemon holds; pattern rules are best effort. Line 27 (linear-time redaction in the model gate proxy): superseded. The model gate forwards request bodies without redaction, and this ADR records that as the design.
- [ADR-0008](0008-conclaves-wards-and-live-attach.md), line 21 (Conclave messages, Relics and Scry output pass through `internal/redact` before persistence or broadcast): kept as a required boundary, refined as above.
- [ADR-0010](0010-agentic-context-learnings-and-verification-pipeline.md), line 38 (pre-checks redacted through `internal/redact`, "preserving Sablecourt's secret-isolation invariant"): the redaction is kept and refined as above. Superseded in one point: redaction does not preserve invariant 2 by itself. Container isolation carries invariant 2.
- [ADR-0012](0012-learning-evidence-and-expiry.md), lines 12, 18 and 20 (redacted learning statements, advisory section and export): kept as required boundaries, refined as above.
- [ADR-0013](0013-bounded-council-rounds.md), lines 16 and 18 (redacted peer excerpts and the writer's redacted diff): kept as required boundaries, refined as above.
- `docs/design.md`, lines 162, 167 and 238 (the gate as a "redaction boundary" and `internal/redact` as the "boundary redactor"): the boundary stays a place where redaction is required. Refined: the pattern rules are not a security boundary.

## Alternatives considered

Treat pattern rules as a boundary and keep fixing bypasses. Three review passes each found new cases of one kind, so the rule set does not converge. Each fix also widens the rules toward false positives in ordinary text. A boundary that a fourth pass is expected to break is not one, and calling it one would mislead users about what reaches the provider.

Write per-format parsers now. Parsers for `.env`, Git config and JSON would find secrets by position instead of shape. They would still miss formats nobody wrote a parser for, each parser is new attack surface under the parser test rule, and none of them helps on the model gate or the proxy tunnel, where content is not redacted at all. Exact match covers the secrets the daemon is responsible for at a fraction of the cost.

Allow-list what leaves. Sablecourt could send only content it has approved to the provider. The harness CLIs read files and build their own requests, and the council proxy sees only a TLS tunnel, so Sablecourt cannot inspect or filter those requests without terminating TLS inside the harness's provider session. That would mean holding or forging provider trust inside each container, which ADR-0011 rules out, and it would break the CLIs on every protocol change.

## Consequences

Users must not keep live secrets in a checkout they hand to a harness. The harness docs state this limit next to the setup steps.

Docs that describe redaction show examples generated from the test rows of `internal/redact` and say that the rules are best effort. Pages that list "what redaction catches" as a promise are rewritten.

Issue 180 becomes the work that makes invariant 3 testable: a table test per redacting boundary inside the daemon that feeds each secret the daemon holds in memory in and asserts it does not come out.

Container mode carries invariant 2 for 0.1.0. Any later path that runs a harness outside a container needs its own amendment that states which guarantees hold there.

Bypasses of the pattern rules are still fixed when cheap, as defence in depth, but a bypass is no longer a security defect by itself. A leak of a secret the daemon holds in memory, at a redacting boundary inside the daemon, is one.

## Amendment 2026-11-27: what exact match covers in 0.1.0

- Status of this amendment: Proposed

Three review passes on [issue 180](https://github.com/shadewright-labs/sablecourt/issues/180) each found a held value that got past the matcher in a form nobody had listed: a backslash before a value that starts with `n`, a duplicate JSON name, a number written with an exponent, a fragment split across two fields. Each fix added a decoded view, and each new view left the next form open. The user decided on 2026-11-27 to stop adding views and to accept exact match with stated limits. It stays a second layer: isolation keeps provider keys in the gate, and a workload holds only its task lease.

This amendment narrows the first guarantee in the decision above ("every secret the daemon process holds in memory is replaced by exact match"). The accepted text stays as written; where it disagrees with this amendment, this amendment wins once it is accepted.

### What is matched

A registered value is matched in its literal bytes and in a fixed list of spellings: quoted (Go `strconv.Quote`), JSON-escaped (with and without HTML escaping, and with `\/`), and, for binary values, standard and URL base64 with and without padding and lower and upper case hex. The matcher reads the raw bytes, and also the bytes with CR, LF and their escaped forms skipped, so a value wrapped across lines still matches. Nothing else is decoded.

At a boundary that carries a declared JSON record, the record is decoded once and every string and object name is matched, duplicates included. A record that shows a held value is refused as a whole: it is replaced by the marker as a JSON string, and none of its payload is kept. The typed boundaries in 0.1.0 are the launch input (`SABLECOURT_INPUT_JSON`) and stored task messages. Every other boundary replaces matches in place by literal match.

The replacement never discloses a value. The marker is `[REDACTED:known_secret]` unless a registered value occurs inside it; then the replacement is empty and a refused record becomes `null`. Replaced text is scanned again, so a marker that forms a held value with its neighbours is replaced too. If that has not settled after eight rounds, the text becomes the marker alone. A stream keeps its raw input and emits only what the batch matcher returns for a prefix that ends right after a byte no registered spelling holds (other than CR, LF, a backslash, `r` or `n`); no match, line-break escape or backslash pair crosses such a byte, so the stream emits what a batch over the whole input returns. The cost: streamed output arrives in larger pieces, and a stream with no such byte in 64 KiB of pending input, or whose prefix does not settle, emits the marker and stops. A pattern rule that forms a held value after the exact pass turns the whole text into the marker.

### New read boundaries

Values registered after a record was stored are caught on read:

- Stored task input and task messages, read by A2A `tasks/get`, the task API (`GET /api/v1/tasks`, `GET /api/v1/tasks/{id}`), the dashboard and the workload's own `/gate/task/messages`. All of them call one function and apply the record rule.
- Model Gate responses on the primary and the fallback path: response headers are matched (a header whose name shows a held value is dropped) and the body passes through the stream matcher. A response the matcher cannot read, because it still carries a `Content-Encoding`, is refused with 502.
- Scry replay: the ring is checked again against the registry as it is when a viewer subscribes. A viewer whose channel was full gets a gap marker before its next chunk, so the browser never joins two chunks the matcher did not see side by side.

### Minimum length

A credential shorter than 8 bytes (after trimming whitespace) cannot be matched without hitting ordinary text, so it is refused where it is loaded. The error names the setting and never the value. At start the daemon stops: provider keys, webhook secrets and every other secret in the store, MCP server secrets named by a manifest, `SABLECOURT_NOTIFY_WEBHOOK_URL` and a PostgreSQL DSN password. At run time a gate, handler or provider that loads such a value refuses the request or the write instead of using it; an unauthenticated caller is told only that the credential was refused. Kubernetes credentials read from a kubeconfig follow the same rule.

### Documented limits

These are not covered in 0.1.0 and are tracked in [issue 189](https://github.com/shadewright-labs/sablecourt/issues/189):

- Spellings outside the list: percent encoding, mixed-case hex, base64 nested inside another encoding, Unicode normalisation, number renderings of a numeric value, and JSON nested inside a JSON string beyond the one decode.
- Fragments of a value split across fields, records or messages. A reader who joins them can rebuild the value.
- Paths with no exact matcher: audit fields, error and log text, the Agent Card, council reads, Takeover input to a workload's stdin, and database and Kubernetes credentials that the drivers resolve themselves (pgx query settings, environment and passfiles; a refreshed `BearerTokenFile` or a `KeyFile`).

The property tested on the branch: at each named boundary, no registered value passes in its literal bytes or a listed spelling. Tests that pin a limit carry it in their name, so changing one is a visible decision.
