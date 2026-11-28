# Command steps

Status: built.

This page is an overview: the diagram's arrows leave out the reconciler and the store, which sit between each step, and the sections below give the exact conditions.

A command step is a workflow step that runs one command from the allowed set of its workload's manifest, such as `make test`, in a harness container on a read-only checkout of the commit of the author step. The step has no prompt and no model, and its container never receives the task lease. The command is never started as a host process.

[![This is an overview: the arrows leave out the reconciler and the store, which sit between each step. A command step is refused or runs in the harness container. A passed result moves the run on; a refusal or a result that did not pass makes the control plane try to halt the run.](../diagrams/command-step.svg)](../diagrams/command-step.svg "Open full size")

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

The manifest loader refuses a command step that is the first step, that follows a `verifier` step, whose workload is not a `coding-agent` or does not list the command, or that has a nonempty `role`, `tier`, `provider`, `model` or `prompt` or a nonzero `maxRetries` (the loader checks values, so an explicit empty string or `maxRetries: 0` passes). A command step may follow another command step. The argv of a command has 1 to 64 entries, each at most 4 KiB of UTF-8 without a NUL byte, and its first entry is not empty or an option. `timeoutSeconds` is at most one hour and defaults to 10 minutes. `maxOutputBytes` is at most 1 MiB and defaults to 64 KiB. `failureReason` is at most 512 bytes of valid UTF-8 with no control characters or DEL ([limits](../reference/limits.md#command-steps)).

## Launch

The argv always comes from the manifest. A task's input names the command but never supplies argv.

A task whose input has a field named `command`, in any letter case, but that is not a declared command step of its workload with the same command name, is refused as `command_not_allowed`. A declared command step also needs a step registration written by the control plane, with the author's commit, and a workload that is a `coding-agent`; without them it does not launch. The control plane puts the author's commit in the registration when the author step completes, if it can extract exactly one. A command step also needs the run's first author base ([the author base](#the-author-base)), or it is refused as `author_base_unavailable`.

| Refusal | Meaning |
| --- | --- |
| `command_not_allowed` | The command is not a declared step with its workload and allowed set, or its argv is not valid. |
| `author_base_unavailable` | The run has no recorded first author base, or the base is not a commit ID of the same length as the pinned commit, or it is not an ancestor of the pinned commit. |
| `no_container_runtime` | The adapter has no container runner. There is no host fallback. |
| `container_spec_refused` | The container launch routine returned an error, which can come after Docker has started the container. There is no host fallback. |

A refused task fails with the reason `command_refused`, and its event holds the refusal reason and a fixed detail text.

## What the command container gets

The container is built by the same function as a coding task's container ([harness containers](harness-containers.md)) and has the same guards: user `10001:10001`, a read-only root file system, all capabilities dropped and `no-new-privileges`. These differ:

- No login volume is mounted, and no prompt or model is passed.
- `HOME` is `/scratch`, a tmpfs.
- The checkout is mounted read-only, and a launch with any writable checkout mount is refused.
- No context packet is built, and the reconciler gives the coding adapter the task input `{}`; the command itself receives no task input.
- Standard input is closed without the task lease written to it.

## The author base

The coding adapter reports the commit a checkout started from as a `git-base` artifact when it collects artifacts, and the control plane keeps the first author base in the step registrations of the run when the run's history provides it. A command step is refused unless it is recorded. Before the command runs, the adapter checks that the base is an ancestor of the pinned commit and writes it as the ref `refs/sablecourt/author-base` in the container's Git metadata. The adapter runs `git merge-base --is-ancestor` on the base and pinned commit IDs before it writes the ref; the example command below separately checks the base read from that ref against `HEAD`.

The `coding` workflow of [`examples/coding-court/coding-court.yaml`](https://github.com/shadewright-labs/sablecourt/blob/main/examples/coding-court/coding-court.yaml) runs a command step named `invariant-paths` after the author. It turns each listed path into the pathspec `:(top,literal)<path>`, so Git matches it literally and never as a glob, and it diffs the author base against `HEAD` over those paths. It exits 1 and lists the paths when the diff is not empty, and exits 2 on an invalid list entry, a missing base or a Git error. With an empty path list it exits 0 and prints that no path was checked, without reading the base or running Git.

## The result

The result is the task artifact `command-result` (`application/json`) with the fields `command`, `outcome`, `exit_code`, `output` and, when true, `output_truncated` (omitted when false). `outcome` is one of `passed`, `failed`, `timeout` and `output_over_cap`.

- The output is standard output, then standard error, each trimmed, with a blank stream left out, then redacted and capped to `maxOutputBytes` with `redact.Tail`.
- `passed` means exit status 0 and no cut. A nonzero status is `failed` unless the output was cut or the step timed out, which take precedence. Output that was cut, or a stream over the runner's limit, is `output_over_cap` and fails the step even when the command exited 0.
- A result recorded without the runner's output (a time limit, a stop, or a daemon restart while the command ran) has outcome `failed` or `timeout` and no output. After a restart, a container found exited with status 0 and no recorded result is recorded with exit status 1.
- The step writes no session log.

## When a command step halts the run

A refused command step, and a command step whose result did not pass, makes the control plane try to halt the run. It halts only if the task is a registered command step of that run, the loaded manifest still declares a command step at the task's workflow index for the task's workload (the command name need not match), and the run is not already halted. The halt writes an event on the run's first task, an audit row with the action `workflow.command`, and the run's registration marked `halted` in one store transaction. A halt attempt writes nothing when the manifest or registration does not match or the run is already halted, and a repeated halt of the same step records no second event.

| Event | Fields |
| --- | --- |
| `workflow_halted_command_failed` | `command_task_id`, `step`, `command`, `outcome` (defaults to `failed`); `exit_code` only when the task has a stored result; `failure_reason` only when the manifest sets one |
| `workflow_halted_command_refused` | `command_task_id`, `step`, `command`, `refusal`; `failure_reason` only when the manifest sets one |

Only a `passed` result with exit status 0 lets the run go on. The next task after a command step is a child of the command task; the author's commit supplies the evidence.
