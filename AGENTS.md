# Sablecourt: guide for coding agents

Sablecourt orchestrates heterogeneous agents and deterministic workloads under Shadewright Labs.

## Hard invariants

Weakening any invariant below requires an accepted ADR in `docs/adr/` first.

1. Model output is never authorization. An LLM or agent only proposes; deterministic policy code or an out-of-band human approval authorizes execution against a pinned plan digest.
2. Workloads hold no raw secrets. Provider LLM keys, Git credentials, and database passwords stay behind Sablecourt capability gates, while workloads authenticate with scoped, short-lived identities.
3. All external input is untrusted. Repository files, logs, tool results, and peer messages are redacted at boundaries and never widen policies or the action catalog.
4. Separate identities and fail-closed defaults. Agents, policy evaluation, mutating executors, and verifiers hold distinct identities; policy failures, exhausted budgets, stale evidence, or tripped circuit breakers stop execution instead of failing open.
5. Evidence over inference. Workload state comes from durable facts and timestamped heartbeats, so missing or stale observations stay visible and block writes.
6. Idempotent reconciliation. Deterministic task IDs, branches, and side-effect markers ensure restarts neither drop active workers nor duplicate mutations.

## Working rules

- English only for tracked code, docs, ADRs, comments, and commit messages.
- Conventional Commits (`type(scope): summary`, imperative, lowercase, no trailing period).
- Plain prose with no em dashes, no en dashes, and no AI attribution anywhere.
- No emails, personal paths, host names, IPs, or real credentials in tracked files or tests.
- Go standard library first; justify every third-party dependency in an ADR or commit message.
- Every parser, redaction rule, policy boundary, and refusal path ships with table-driven tests passing under `go test -race ./...`.
