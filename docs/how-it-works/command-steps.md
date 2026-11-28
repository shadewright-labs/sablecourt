# Command steps

Status: built.

A command step is a workflow step that runs one command from the allowed set of its workload's manifest, such as `make test` or a linter, in a harness container on a read-only checkout of the commit the author step produced. When the container's exit is observed and recorded, it stores the exit status and the capped, redacted output as a task artifact. The step has no prompt and no model, and its container never receives the task lease. The container is built by the same function as a coding task's container ([harness containers](harness-containers.md)); a command is never started as a host process.

[![A command step is refused when its launch checks fail or its container cannot be created or started, and otherwise runs and either advances or tries to halt the run.](../diagrams/command-step.svg)](../diagrams/command-step.svg "Open full size")

When an author step completes and the next declared step is a command, the control plane submits the command step and puts the author's commit in its registration if it can extract exactly one. A launch without that commit fails with `author_checkout_unavailable`. At launch the control plane looks up the command in the loaded manifest through the step registration it wrote, then the adapter builds the container. A failed launch check refuses the step before the container is built. When a command step is refused or fails, the control plane tries to halt the run. It halts only if the task is a registered command step of that run, the loaded manifest still declares that command step for the task's workload, and the run is not already halted. A refused task that is not a registered step (for example a task submitted directly whose input names a command) fails without halting any run. A command that runs and passes moves the run to the next step, which follows the author's task. A command that does not pass triggers the halt attempt too, and when it succeeds the event carries the command's `failureReason` if the manifest sets one.

## Declare a command

The allowed set is part of a `coding-agent` Workload, under `spec.coding.commands`. A Workflow step names one entry with `command` ([manifest reference](../reference/manifests.md)):

```yaml
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: builder
  zone: runtime
spec:
  class: coding-agent
  runtime:
    type: worktree-cli
  coding:
    harness: claude
    repoPath: /srv/repo
    commands:
      - name: check
        argv: ["make", "check"]
        timeoutSeconds: 600
        maxOutputBytes: 65536
        failureReason: "make check failed: fix the change before review"
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: reviewer
  zone: runtime
spec:
  class: coding-agent
  runtime:
    type: worktree-cli
  coding:
    harness: codex
    repoPath: /srv/repo
---
apiVersion: sablecourt.io/v1alpha1
kind: Workflow
metadata:
  name: coding
spec:
  steps:
    - name: author
      workload: builder
      role: author
      prompt: Implement the change.
    - name: check
      workload: builder
      command: check
    - name: review
      workload: reviewer
      role: verifier
      prompt: Review the change.
```

The manifest loader refuses a command step that is the first step, that follows a `verifier` step, whose workload is not a `coding-agent` or does not list the command, or that sets `role`, `tier`, `provider`, `model`, `prompt` or `maxRetries`. A command step may follow another command step. The loader refuses a verifier whose workload an earlier author or command step also uses, so the verifier in the example runs on its own workload. It refuses a command whose name, argv, time limit, output cap or failure reason is outside the limits on the [limits page](../reference/limits.md#command-steps).

## Which launch is a command step

A launch runs a command only when the control plane resolves all of the following. Workflow name and step index in the task input are not enough:

1. The loaded manifest has a step at that workflow and index that names a command and uses the task's workload.
2. The `command` in the task input equals that step's command.
3. A step registration written by the control plane exists for the task, with the same workflow and index and with an author commit.
4. The workload is a `coding-agent` with a `coding` block.
5. The command is in the workload's allowed set and its argv is valid.
6. The run's first author base is recorded (see [the author base](#the-author-base)).

A task that is not a step of that kind but whose input has a field named `command`, in any letter case, is refused as `command_not_allowed`. The argv always comes from the manifest, so argv in a task's input is never read. A task whose input names neither is not a command step and launches as an ordinary task.

For a task that names a declared command step, the launch first needs the author's checkout. When the step has no registration, the registration has no author commit, or the workload is not a `coding-agent`, the task fails with `author_checkout_unavailable` instead. That task can halt a run only if it has a registration, under the conditions in [when a command step halts the run](#when-a-command-step-halts-the-run).

## What is refused

| Reason | When | Where it is decided |
| --- | --- | --- |
| `command_not_allowed` | One of checks 1 to 5 above fails. The adapter also refuses a launch with no pinned commit, a checkout that is not read-only, another checkout mode, a council deadline, an invalid argv, or a time limit or output cap outside the limits. | The control plane, then the adapter |
| `author_base_unavailable` | Check 6 fails, the base is not a commit ID of the same length as the pinned commit, or it does not resolve to itself in the repository or is not an ancestor of the pinned commit. | The control plane, then the adapter |
| `no_container_runtime` | The adapter has no container runner, or the workload is not a `coding-agent`. The control plane refuses before it issues a lease or makes a worktree, and the adapter repeats the check before its host-mode check, so forcing host mode does not change it. | The control plane, then the adapter |
| `container_spec_refused` | The adapter's step that provisions the proxy and creates and starts the harness container returns an error, for example an argv the container spec refuses, a proxy image that cannot be resolved, a proxy that fails to start, or Docker failing to create or start the container. Failures before that step, such as preparing the Git metadata, have other reasons. An error after Docker has started the command (closing standard input, or the start confirmation) is reported the same way, so this reason does not prove the command never ran. There is no host fallback. | The adapter |

A refused task fails with the reason `command_refused`, and the event of that transition holds the refusal reason and a fixed detail text.

## What the command container can read, write and reach

The container comes from the same function, and so has the same guards, as a coding task's container: user `10001:10001`, a read-only root file system, all capabilities dropped, `no-new-privileges`, CPU, memory and process limits, and an allowlisted environment. These differ:

- The argv is the manifest's, started under a `timeout` entrypoint, with no shell unless the argv names one.
- No login volume and no tool volume are mounted. The environment sets `HOME` to `/scratch`, a tmpfs mounted `noexec`.
- A command launch needs a read-only checkout pinned to a commit and is refused if any mount of its checkout is writable. A command cannot execute a file it writes to `/scratch`, and it cannot write to the checkout.
- No context packet is built for it, and it gets no Model Gate coordinates.
- Standard input is closed without the task lease being written to it, so the command does not receive the lease.
- The proxy runs with the `command` profile, which has no provider host.
- The checkout is a new worktree pinned to the author's commit, with its own Git metadata directory mounted beside it and also read-only. The daemon removes both when the step is finished.
- The time limit (`timeoutSeconds`) is counted from a deadline fixed before the daemon sets up the proxy and network, not from when the command starts.

## The author base

The run's first author step records the commit its checkout started from as a `git-base` artifact. The control plane keeps it in each step registration as the run's author base. It is recorded once, and no later author step replaces it:

- A new run starts with the state `pending`.
- While the state is `pending`, a first author attempt that names a valid `git-base` records it. An attempt that did not complete and left neither a `git-base` nor a `git-commit` leaves the state `pending` in the retry's registration. A retry of a coding author is checked out at the author's commit, so a retry of an attempt that left no commit fails with `author_checkout_unavailable` instead of starting afresh.
- Any other outcome of the first author attempt (a commit without a base, an invalid or repeated base, a completed attempt with no artifacts, an attempt pinned to an earlier commit) sets the state `unknown`, and so does a registration written before the base was recorded.
- A retried or remediated author step carries the run's base and state on.
- A verifier or command step whose task input parses does not change a `pending` state.

A command step is refused with `author_base_unavailable` unless the state is `recorded`. When it runs, the adapter checks that the base resolves to a commit and is an ancestor of the pinned commit, and then names it as the ref `refs/sablecourt/author-base` in the container's Git metadata. The ancestry check applies to that ref only. The metadata is a new directory with the objects reachable from the pinned commit and the blobs staged in the checkout's index; the daemon does not tell the command anything else about the author's earlier commits.

### The guard in the coding example

The `coding` workflow of [`examples/coding-court/coding-court.yaml`](https://github.com/shadewright-labs/sablecourt/blob/main/examples/coding-court/coding-court.yaml) runs a command step named `invariant-paths` after the author. Its argv is a `sh -c` script followed by `guard` and a path list, and it has a time limit of 60 seconds. The script:

- exits 0 with a message when the path list is empty, so an empty list checks nothing;
- exits 2 on a list entry that is empty, absolute, has a `.` or `..` path component, or contains `//`;
- turns each entry into the pathspec `:(top,literal)<entry>`, with one trailing `/` removed, so Git matches the entry as a literal path from the top of the repository and never as a glob;
- reads `refs/sablecourt/author-base` with replace objects off and exits 2 when it is missing or is not an ancestor of `HEAD`;
- runs `git diff --name-status --no-renames --ignore-submodules=none --no-ext-diff --no-textconv --no-relative --exit-code` with replace objects off, from the base to `HEAD` over those pathspecs, so renames, external diff and text conversion drivers and relative paths are off and submodule changes are not ignored;
- exits 1 and lists the paths when the diff is not empty, and exits 2 on any other Git status.

A nonzero exit fails the step, which triggers the halt attempt described below; the event carries the command's `failureReason`.

## The result

The result is the task artifact `command-result` (`application/json`). It is stored after the control plane redacts every string field and every key of the JSON, so the stored text still decodes:

| Field | Meaning |
| --- | --- |
| `command` | The command's name. |
| `outcome` | `passed`, `failed`, `timeout` or `output_over_cap`. |
| `exit_code` | The exit status recorded for the container. It is `-1` when the result was recorded without an observed exit (for example the time limit or a stop). When the daemon restarted after a container exited with 0 and before its output was recorded, it is `1`. |
| `output` | The output, capped to `maxOutputBytes` and redacted before the control plane redacts the stored JSON again. |
| `output_truncated` | Set when the output passed the cap or a stream passed the runner's 1 MiB limit. |

The outcome is `passed` for exit status 0 with no cut, `failed` for a nonzero status or for a result recorded without output, `timeout` when the daemon's deadline passed first, and `output_over_cap` when output was cut. An `output_over_cap` result for a command that exited 0 keeps `exit_code` 0 in the artifact, and the step still fails. Only `passed` with `exit_code` 0 moves the run to the next step. The step keeps no session log or final message, so the capped output is the only copy. Its other artifacts come from the same code as a coding task's: `git-status` of the checkout, then `git-commit` and `git-base` when the control plane collects the artifacts of an observed exit. A task that fails without that observation (a refusal, a launch that could not be set up, a stale observation) has no `command-result` artifact and none of these.

### How the output is capped

The runner keeps each stream apart, up to 1 MiB of it. Past that limit it keeps the end of the stream: the last 1 MiB as the tail, and the bytes held before it as context only (at least 64 KiB once older bytes have been dropped, and more until its buffer is next compacted). The runner replaces the exact task lease and prompt in the tail and its context, and applies the `redact.String` pattern rules to them together, so a secret that starts in the context and ends in the tail can be replaced whole. None of the context is returned. The pattern rules are best effort, so this does not guarantee that every such secret is removed. While the runner drops older bytes, it records whether they end inside a private key that has no `END` line yet. If so, it hides the kept bytes up to the next `END` line, or all of them when none follows.

Then both streams go through `redact.Tail`. The control plane joins them, standard output first, after trimming each one and leaving out a stream that is only whitespace, so such a stream displaces nothing. The joined text is made valid UTF-8, with each run of invalid bytes becoming one U+FFFD, and `redact.Tail` bounds it again to `maxOutputBytes`. `redact.Tail`:

1. redacts the whole text, and replaces everything from a private key `BEGIN` line that has no `END` line after it to the end with `[REDACTED:private_key]`, so a diagnostic after an unfinished key is lost;
2. cuts the redacted text to its last `limit` bytes;
3. after a cut, drops the first line of what is left, then everything through the last private key `END` line that comes before the first private key `BEGIN` line in the rest, whether or not the cut fell inside a key;
4. redacts the cut text again until it stops changing, and cuts again if redaction made it longer than the limit.

It stops after 8 passes and returns empty text when the passes do not settle or no whole line is left. Any cut counts as an overflow, so the step cannot pass with `output_truncated` false after a cut. The overflow flag comes only from a recorded cut: output that prints the text `[REDACTED:output limit]` is not an overflow.

## When a command step halts the run

When a command step fails on one of these paths (the command's outcome, a refusal, a launch that could not be set up, an observation failure, or a restart), the control plane tries to halt the run. The halt happens only if the task is a registered command step of that run, the loaded manifest still declares that command step for the workload, and the run is not already halted. When terminal reconciliation cannot revoke the task's leases or clean up its runtime, the halt waits for a later pass. The halt writes three things in one store transaction: an event on the run's first task, an audit row with the action `workflow.command`, and the run's registration marked `halted`. If the transaction fails, none of them is stored, and a later pass retries the whole halt. A repeated halt of the same step writes no second event. A halted run starts no later step.

| Event on the run | Fields |
| --- | --- |
| `workflow_halted_command_failed` | `command_task_id`, `step`, `command`, `outcome`; `exit_code` only when the task has a stored result; `failure_reason` only when the manifest sets one |
| `workflow_halted_command_refused` | `command_task_id`, `step`, `command`, `refusal`; `failure_reason` only when the manifest sets one |

A step that failed without a stored result has outcome `failed` and no `exit_code` in the event. `failure_reason` is the command's `failureReason` from the manifest, so a workflow can say what to do instead, for example which other workflow to start.

## Steps after a command step

A step after a command step follows the author's task, not the command's. A verifier's prompt is built from the author's task, and its checkout is pinned to the author's commit. It gets the author's retry budget unless the verifier step sets its own `maxRetries`. A `CHANGES_REQUESTED` verdict submits the author step again only when retries are left under that budget and the author step index is valid. A command step runs again after an author attempt completes, whether the attempt was a retry after a failure (which needs the commit the failed attempt left) or a fix round after a verdict. An author attempt that fails without a retry left starts no later step, and so no command step.

## Stated limits

- `/scratch` is mounted `noexec` and the checkout is read-only, so a command cannot run a program it writes there or into the checkout.
- If the daemon restarts while a command runs and the container, still present, has exited with 0 before the output is recorded, the step fails with outcome `failed`, exit status 1 and no output. A nonzero exit status of the container is kept, also with no output.
- Standard output and standard error are kept apart and joined after the run, so their order relative to each other is lost. A stream longer than 1 MiB fails the step with `output_over_cap`, and only its last whole lines are kept. The cap on the joined output keeps its end, so a long standard error that is not blank can displace standard output. A stream with no line break in its last 1 MiB after the cut is replaced by `[REDACTED:output limit]`, and output over `maxOutputBytes` with no line break left after the cut is stored empty.
- A secret that starts before the context the runner keeps (at least 64 KiB before the last 1 MiB, once older bytes were dropped) is not covered by it. Pattern rules are best effort (invariant 3 in `AGENTS.md`).
