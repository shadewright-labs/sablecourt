# Sablecourt: guide for coding agents

Sablecourt orchestrates heterogeneous agents and deterministic workloads under Shadewright Labs.

## Hard invariants

Weakening any invariant below requires an accepted ADR in `docs/adr/` first.

1. Model output is never authorization. An LLM or agent only proposes; deterministic policy code or an out-of-band human approval authorizes execution against a pinned plan digest.
2. Workloads hold no raw secrets. Provider LLM keys, Git credentials, and database passwords stay behind Sablecourt capability gates, while workloads authenticate with scoped, short-lived identities.
3. All external input is untrusted. Repository files, logs, tool results, and peer messages never widen policies or the action catalog, and they are redacted at boundaries: Data and Action Gate responses to workloads, Conclave messages, Relics and Scry output, context packets and pre-checks, learnings, and council peer excerpts and diffs. At those boundaries inside the daemon, each secret the daemon process holds in memory is replaced by exact match; pattern rules for other secret shapes are best effort, not a boundary. Exact match does not reach detached wrappers that never receive daemon secrets, leases kept only as hashes, or a workload's own task lease, which it necessarily holds. Content a workload reads, including secrets inside its own checkout, can reach its model provider.
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

## Releases

- Stable releases (`vX.Y.Z`) are managed by `release-please` (`.github/workflows/release-please.yml`, `release-please-config.json`, `.release-please-manifest.json`) from Conventional Commits on `main`. While on 0.x, `feat` and breaking changes (`!` or `BREAKING CHANGE:`) bump minor, `fix`, `perf`, and `revert` bump patch, and `refactor`, `test`, `docs`, `build`, `ci`, and `chore` stay out of `CHANGELOG.md`.
- `release-please.yml` keeps one release pull request open using the `shadewright-release` GitHub App token from the `release` environment (used only by `release-please.yml`, holding the App key, and restricted to `main`). Never edit `CHANGELOG.md` or `.release-please-manifest.json` outside that pull request, and never create a plain `vX.Y.Z` tag by hand.
- The release pull request carries the edits to `CHANGELOG.md`, `.release-please-manifest.json`, `deploy/k8s/base/deployment.yaml`, and `deploy/local/docker-compose.yml`. When a maintainer merges it, `release-please.yml` runs on the merge push to create a draft GitHub release and push the `vX.Y.Z` tag through the App token so `.github/workflows/release.yml` runs to build, scan, sign, verify, and publish the release (with no `environment:`) and container images.
- For a pre-release (`vX.Y.Z-rc.N`), a maintainer tags a commit on `main` whose `ci-ok` check passed and pushes the tag by hand; `release.yml` publishes it as a pre-release without moving `latest`, and `release-please` ignores it.
- See `docs/ci.md` ("Releases") and `docs/how-to/verify-a-release.md` for workflow details and verification commands.
