# Workloads and runtimes

A workload is something Sablecourt may launch. Its class decides which runtime adapter starts it and how that run is isolated. This page covers the three classes, the two zones, what each adapter does, and what a deterministic controller cannot do.

Status: partly built. The Kubernetes adapter is built and parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md); suspend and resume are incomplete (tracking issue #14, see [tasks](tasks.md#known-gaps)).

[![Inside the host, the daemon holds the MCP Gate, Model Gate and runtime adapters; a coding run sits in a harness container behind a per-launch proxy, host mode is off unless forced with an unsafe flag, a long-lived agent and a controller run in containers on an internal network, the controller is refused at the Model Gate, and a Kubernetes adapter is parked.](../diagrams/runtime-isolation.svg)](../diagrams/runtime-isolation.svg "Open full size")

## Classes and zones

A `Workload` manifest names a class and a zone. The adapter is chosen by the class, not by `runtime.type`.

| Class | Adapter | Holds |
| --- | --- | --- |
| `coding-agent` | Coding adapter: a harness CLI (`claude`, `codex` or `agy`) in a harness container, against a Git worktree of the task | A `coding` block with harness and `repoPath`. |
| `long-lived-agent` | Docker adapter | A container image. |
| `deterministic-controller` | Controller adapter wrapping the Docker adapter | A container image and no Model Gate grant. |

The zone is `runtime` or `sandbox`. The loader refuses a sandbox workload that lists `actions` or `delegateTo` grants, and the policy engine denies `action:` and `delegate:` capabilities for it at run time, so a sandbox workload cannot use gate-mediated actions or delegate work. A harness container reaches only its provider's hosts through the proxy, so a sandbox coding workload cannot call an arbitrary external service; in forced host mode the Seatbelt profile allows outbound network traffic and that limit is gone. Moving a workload to the runtime zone is a reviewed change to its manifest. `sablectl promote <workload>` and the `sandbox.propose_promotion` action only create a branch named `sablecourt/promote-<workload>` (or check that the workload is in the sandbox zone); neither edits a manifest. The names `claude`, `codex` and `agy` are reserved for the council panel.

## Coding adapter: harness containers

Standalone coding tasks and council seats run in harness containers, as the 2026-11-27 amendment of [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md#amendment-2026-11-27-container-only-harnesses-for-010) decides. For a standalone task the adapter creates a Git worktree and a branch `sablecourt/<task id>` from the current commit of the configured repository, writes the session marker, and hands the worktree to the same launch path a council seat uses: a per-launch proxy and network, then one container built by the one container spec function in `internal/runtime/docker/council_config.go`. The worktree is mounted read-write at `/workspace` (checkout kind `task`); a workflow verifier pinned to a commit gets it read-only (kind `review`). Git in the container uses the launch's own metadata directory at `/workspace-git`, not the shared repository; commits made there are not kept as commits, but the files they changed are saved to the task branch when the task ends (see [harness containers](harness-containers.md)). On native Linux the worktree belongs to the daemon's user, and the container runs as uid 10001, so a read-write bind does not make it writable: a task can start and then fail to edit files. Docker Desktop on macOS maps ownership and is not affected ([issue 197](https://github.com/shadewright-labs/sablecourt/issues/197)). The mounts, environment, network, user, read-only root, dropped capabilities, login volume, tool volume and limits are those of a council seat and are listed on [harness containers](harness-containers.md). The wall-time limit is 15 minutes by default.

The reconciler asks for a container for every coding task when the coding adapter has a container runner, which the daemon wires when a Docker engine is configured. Without one, coding tasks and councils are refused; there is no silent fallback to the host.

### Host mode (unsafe, off by default)

The host launcher stays in the code only behind the daemon flag `--unsafe-host-harnesses`. A run in host mode is outside invariants 2 and 3: Sablecourt makes no claim that the harness cannot reach secrets the daemon holds, and redaction is the only control on what it writes ([ADR-0015](../adr/0015-redaction-is-defence-in-depth.md)). The flag takes effect only when no container runtime is configured; with Docker present, the adapter refuses a host launch even with the flag set. The daemon logs a warning at startup and at each host launch, and `sablectl doctor` shows the mode (`GET /api/v1/runtime/coding`).

In host mode the harness starts with a stripped environment (`PATH`, `LANG`, `HOME` and `TMPDIR` set to a private scratch directory, `SABLECOURT_MANAGED=1`, `SABLECOURT_TASK_ID`, `SABLECOURT_WORKLOAD`, and, when set, `SABLECOURT_DATA_GATE_URL` and `SABLECOURT_TASK_TOKEN`) in its own process group, under macOS Seatbelt through `/usr/bin/sandbox-exec`. The profile denies by default, allows reading system runtime files, the harness binary, the worktree and the scratch directory, allows writing the scratch directory and, unless the run is read-only, the worktree, and allows outbound network traffic. The daemon registers the host launcher only when the flag is set and its Seatbelt probe passes; `coding.sandbox` must then be `seatbelt`. No Linux host sandbox exists.

When the harness exits, in either mode:

- The adapter commits any remaining edits to the task branch, records `git-status`, `git-diff` (32 KiB), `session-log` (16 KiB) and `final-message` artifacts, and removes the worktree. The branch stays; the daemon prunes finished branches only when `--coding-branch-retention` is set (default 0, no pruning).
- In host mode, a detached wrapper process runs the harness, writes redacted `stdout` and `stderr` logs, and publishes the exit record when the harness finishes. A non-zero exit code from the harness wins. If the harness exits `0` and closing either output log or writing the finalized `stdout` or `stderr` file fails, the wrapper records exit code `-2` (`internal/runtime.ExitCodeLogClose`, a code a harness cannot return: normal exits are `0` to `255` and signalled, timed-out, stopped, or output-capture failures use `-1`), sets `LogClose` on the exit record (the reconciler records it as `log_close` on the task event), omits `final-message`, and records a redacted error naming the failed log (`stdout`, `stderr`, or both). The reconciler marks that task `failed` with the reason and skips auto-remediation so committed side effects are not rerun. If the harness already exited non-zero and a log close or post-close write also fails, the harness exit code stays, `log_close` is set, and the error is prepended to the diagnostic message so it stays within the 1024-byte `reason` limit. If rewriting `stderr` fails, the wrapper carries a bounded redacted summary inside the `exit` record (capped at 512 bytes total) and still tries to publish `exit`. If publishing `exit` itself also fails, the wrapper exits non-zero with the joined close and write errors and the reconciler later fails the task as `process_not_alive` because no exit record exists for the dead process.

The session marker tells [Nightloom](../glossary.md#session-marker) that Sablecourt manages the session. It is the environment variable `SABLECOURT_MANAGED=1`, set by the coding and Docker adapters, and a file `.sablecourt-session.json` (mode 0400) in the worktree with `managed_by`, `task_id`, `workload`, `zone`, `started_at` and, for a Conclave member, `conclave_id` and `role`. The file is removed before the final commit. Council turns use checkout modes; see [council](council.md) and [harness containers](harness-containers.md).

## Docker adapter and controllers

The adapter needs a local image that the engine can resolve to a `sha256:` ID; it never pulls. The container joins the internal bridge network `sablecourt-internal`, which Docker creates without a route to the outside, drops all capabilities, sets `no-new-privileges`, and uses a read-only root filesystem only when the manifest says `readOnlyRootfs: true`. No CPU, memory or process limits are set for these containers. The environment has the session variable, task and workload names, `SABLECOURT_INPUT_JSON`, the data gate URL and the task token, and the Model Gate URL when the workload holds a grant. The gate address handed to a container defaults to `host.docker.internal:<port>`, and the daemon routes requests to that name as gate requests only for `/gate/` paths.

The controller adapter refuses to start a workload that is not a `deterministic-controller`, or that holds a Model Gate grant, or whose launch carries a Model Gate URL or token. The Model Gate refuses the same workloads again at request time, so a controller cannot obtain model access by either route. It talks to the daemon only through its lease and the grants in its manifest.

## Not built yet

- A machine without a container runtime cannot run coding tasks or councils in 0.1.0, except in forced host mode. In host mode a harness gets a scratch `HOME` and finds no login (issue 63). The separate harness state directories and the host-mode `codex` check of the 2026-11-25 amendment are dropped, not planned.
- Checkpointed suspend and resume for long-lived agents ([ADR-0002](../adr/0002-heterogeneous-mvp-workload-adapters.md)).
- Linux host sandboxing. [ADR-0003](../adr/0003-hybrid-workload-isolation.md) names Bubblewrap and none exists; the ADR-0011 amendment of 2026-11-27 supersedes that line, so none is planned.

## In depth

- [Tasks](tasks.md), [model gate](model-gate.md), [tools and actions](tools-and-actions.md)
- [ADR-0002](../adr/0002-heterogeneous-mvp-workload-adapters.md), [ADR-0003](../adr/0003-hybrid-workload-isolation.md), [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)
- Code: `internal/runtime/runtime.go`, `internal/runtime/coding/`, `internal/runtime/docker/council_config.go`, `internal/runtime/docker/docker.go`, `internal/runtime/controller/controller.go`, `internal/config/loader.go`
