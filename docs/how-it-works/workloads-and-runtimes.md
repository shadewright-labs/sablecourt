# Workloads and runtimes

A workload is something Sablecourt may launch. Its class decides which runtime adapter starts it and how that run is isolated. This page covers the three classes, the two zones, what each adapter does, and what a deterministic controller cannot do.

Status: partly built. The Kubernetes adapter is built and parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md); suspend and resume are incomplete (tracking issue #14, see [tasks](tasks.md#known-gaps)).

[![Inside the host, the daemon holds the MCP Gate, Model Gate and runtime adapters; a coding run sits in a macOS-only host sandbox, a long-lived agent and a controller run in containers on an internal network, the controller is refused at the Model Gate, and a Kubernetes adapter is parked.](../diagrams/runtime-isolation.svg)](../diagrams/runtime-isolation.svg "Open full size")

## Classes and zones

A `Workload` manifest names a class and a zone. The adapter is chosen by the class, not by `runtime.type`.

| Class | Adapter | Holds |
| --- | --- | --- |
| `coding-agent` | Coding adapter: a harness CLI (`claude`, `codex` or `agy`) in a Git worktree on the host | A `coding` block with harness and `repoPath`. |
| `long-lived-agent` | Docker adapter | A container image. |
| `deterministic-controller` | Controller adapter wrapping the Docker adapter | A container image and no Model Gate grant. |

The zone is `runtime` or `sandbox`. The loader refuses a sandbox workload that lists `actions` or `delegateTo` grants, and the policy engine denies `action:` and `delegate:` capabilities for it at run time, so a sandbox workload cannot use gate-mediated actions or delegate work. The host sandbox still allows outbound network traffic, so a sandbox coding workload on a host run can send a direct request that changes an external service. Moving a workload to the runtime zone is a reviewed change to its manifest. `sablectl promote <workload>` and the `sandbox.propose_promotion` action only create a branch named `sablecourt/promote-<workload>` (or check that the workload is in the sandbox zone); neither edits a manifest. The names `claude`, `codex` and `agy` are reserved for the council panel.

## Coding adapter: host sandbox

The adapter creates a Git worktree and a branch `sablecourt/<task id>` from the current commit of the configured repository, writes the session marker, and starts the harness with a stripped environment: `PATH`, `LANG`, `HOME` and `TMPDIR` (both set to a private scratch directory), `SABLECOURT_MANAGED=1`, `SABLECOURT_TASK_ID`, `SABLECOURT_WORKLOAD`, and, when set, `SABLECOURT_DATA_GATE_URL` and `SABLECOURT_TASK_TOKEN`. The process runs in its own process group with a wall-time limit of 15 minutes by default.

- The sandbox is macOS Seatbelt through `/usr/bin/sandbox-exec`. The profile denies everything by default and allows reading system runtime files, the harness binary, the worktree and the scratch directory, and writing the scratch directory and, unless the run is read-only, the worktree. It allows outbound network traffic. Writes to Git storage are limited to the task branch, its index and new loose objects.
- At startup the daemon probes Seatbelt by checking that a private file cannot be read or written under a probe profile. If the probe fails, or the host is not macOS, the host coding adapter is not registered. No Bubblewrap or Linux sandbox exists, so a host coding task cannot run on Linux; `coding.sandbox` accepts `seatbelt`, and any other value fails at launch because confinement cannot be enforced.
- When the harness exits, the adapter commits any remaining edits to the task branch, records `git-status`, `git-diff` (32 KiB), `session-log` (16 KiB) and `final-message` artifacts, and removes the worktree. The branch stays; the daemon prunes finished branches only when `--coding-branch-retention` is set (default 0, no pruning).
- A detached wrapper process runs the harness, writes redacted `stdout` and `stderr` logs, and publishes the exit record when the harness finishes. A non-zero exit code from the harness wins. If the harness exits `0` and closing either output log or writing the finalized `stdout` or `stderr` file fails, the wrapper records exit code `-2` (`internal/runtime.ExitCodeLogClose`, a code a harness cannot return: normal exits are `0` to `255` and signalled, timed-out, stopped, or output-capture failures use `-1`), marks `log_close` on the exit record, omits `final-message`, and records a redacted error naming the failed log (`stdout`, `stderr`, or both). The reconciler marks that task `failed` with the reason and skips auto-remediation so committed side effects are not rerun. If the harness already exited non-zero and a log close or post-close write also fails, the harness exit code stays, `log_close` is set, and the error is prepended to the diagnostic message so it stays within the 1024-byte `reason` limit. If rewriting `stderr` fails, the wrapper carries a bounded redacted summary inside the `exit` record (capped at 512 bytes total) and still tries to publish `exit`. If publishing `exit` itself also fails, the wrapper exits non-zero with the joined close and write errors and the reconciler later fails the task as `process_not_alive` because no exit record exists for the dead process.

The session marker tells [Nightloom](../glossary.md#session-marker) that Sablecourt manages the session. It is the environment variable `SABLECOURT_MANAGED=1`, set by the coding and Docker adapters, and a file `.sablecourt-session.json` (mode 0400) in the worktree with `managed_by`, `task_id`, `workload`, `zone`, `started_at` and, for a Conclave member, `conclave_id` and `role`. The file is removed before the final commit. Council turns use checkout modes and run in containers; see [council](council.md) and [harness containers](harness-containers.md).

## Docker adapter and controllers

The adapter needs a local image that the engine can resolve to a `sha256:` ID; it never pulls. The container joins the internal bridge network `sablecourt-internal`, which Docker creates without a route to the outside, drops all capabilities, sets `no-new-privileges`, and uses a read-only root filesystem only when the manifest says `readOnlyRootfs: true`. No CPU, memory or process limits are set for these containers. The environment has the session variable, task and workload names, `SABLECOURT_INPUT_JSON`, the data gate URL and the task token, and the Model Gate URL when the workload holds a grant. The gate address handed to a container defaults to `host.docker.internal:<port>`, and the daemon routes requests to that name as gate requests only for `/gate/` paths.

The controller adapter refuses to start a workload that is not a `deterministic-controller`, or that holds a Model Gate grant, or whose launch carries a Model Gate URL or token. The Model Gate refuses the same workloads again at request time, so a controller cannot obtain model access by either route. It talks to the daemon only through its lease and the grants in its manifest.

## Not built yet

- Host fallback for harnesses without a container runtime: separate harness state directories and a refusal of host-mode `codex` are described in [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md) and are not in the code. Today the host harness gets a scratch `HOME`.
- Checkpointed suspend and resume for long-lived agents ([ADR-0002](../adr/0002-heterogeneous-mvp-workload-adapters.md)).
- Linux host sandboxing ([ADR-0003](../adr/0003-hybrid-workload-isolation.md) names Bubblewrap; none exists).

## In depth

- [Tasks](tasks.md), [model gate](model-gate.md), [tools and actions](tools-and-actions.md)
- [ADR-0002](../adr/0002-heterogeneous-mvp-workload-adapters.md), [ADR-0003](../adr/0003-hybrid-workload-isolation.md), [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)
- Code: `internal/runtime/runtime.go`, `internal/runtime/coding/`, `internal/runtime/docker/docker.go`, `internal/runtime/controller/controller.go`, `internal/config/loader.go`
