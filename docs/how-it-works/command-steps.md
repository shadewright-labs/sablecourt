# Command steps

Status: built.

A command step is a workflow step that runs one command the operator allows, such as `make test` or a linter, against the commit of the step before it. It records the exit code and the output as evidence. No model takes part: the step has no prompt, no model and no task lease inside its container. The command runs in a harness container built by the same function as a coding task's container ([harness containers](harness-containers.md)), never as a host process.

[![A command step runs only when it is in the allowed set, the run has an author base and a container can be built, then advances or halts the run.](../diagrams/command-step.svg)](../diagrams/command-step.svg "Open full size")

After the author step commits its change, the control plane checks that the step's command is in the allowed set of the workload's manifest and that the run has an author base that is an ancestor of the author's commit ([the author base](#the-author-base)), then that a harness container can be built. If either check fails, the step is refused before any container starts, never on the host, and the run is halted. Otherwise the command runs in the harness container on a read-only checkout of the author's commit. When it exits, its result is recorded, capped and redacted. A passed result moves the run to the next step, which follows the author's task; a failed result halts the run, with the reason recorded on the run.

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

The verifier runs on its own workload: the loader refuses a verifier whose workload an earlier author or command step also uses. The manifest loader refuses a command step that is the first step, that follows a `verifier` step, whose workload is not a `coding-agent` or does not list the command, or that sets `role`, `tier`, `provider`, `model`, `prompt` or `maxRetries`. It refuses a command whose name, argv, time limit, output cap or failure reason is outside the limits on the [limits page](../reference/limits.md#command-steps).

## What runs, and what is refused

| Case | What happens | Recorded as |
| --- | --- | --- |
| The command is in the allowed set and a container runtime is configured | The command runs in a harness container: the checkout mounted read-only, read-only root, user `10001:10001`, all capabilities dropped, a network whose only peer is the proxy, which allows no provider. Its output is capped and redacted before it is stored. | The step's `command-result` artifact. A failed command also halts the run. |
| The container cannot be built or started (for example an argv the container spec refuses, a missing image, or a proxy that fails to start) | Refused. There is no host fallback. | Step event `command_refused`, refusal `container_spec_refused`; run halted. |
| No container runtime is configured | Refused before any worktree or container is made, even when host mode is forced on. | Step event `command_refused`, refusal `no_container_runtime`; run halted. |
| The run has no recorded author base, or the base is not a commit in the repository, or it is not the pinned commit or one of its ancestors | Refused before any container starts. | Step event `command_refused`, refusal `author_base_unavailable`; run halted. |
| A host process | Never: no code path runs a command step outside a container. | |
| The command is not in the allowed set, or a task's input names a command | Refused before any container starts. The argv comes only from the loaded manifest, found through the step registration the control plane wrote when it created the step; argv in a task's input is never read. A task that names a command without that registration (for example one submitted directly) is refused. | Step event `command_refused`, refusal `command_not_allowed`; a task submitted directly whose input names a declared command step gets `author_checkout_unavailable` instead. The run is halted when the task is a declared workflow command step. |

The adapter repeats the container and checkout checks, so a launch that reached it without a container runner, a pinned commit or a read-only checkout is refused there too.

## What the command container can read, write and reach

It uses the same container spec function, and so the same guards, as a coding task: the user, the read-only root, the dropped capabilities, `no-new-privileges`, the CPU, memory and process limits, the mount check and the environment allowlist. These differ:

- The argv is the manifest's, started under the same `timeout` entrypoint; there is no shell unless the argv names one.
- No context packet is built for it, so none of the host pre-checks that feed a verifier's prompt run, and it gets no Model Gate coordinates.
- The container spec function refuses a command launch whose checkout is not the read-only one pinned to a commit.
- No login volume and no tool volume are mounted. `HOME` is `/scratch`, the `noexec` tmpfs.
- Standard input carries nothing and is closed: the command never receives the task lease, which the daemon still mints so the per-launch proxy can start.
- The proxy runs with the `command` profile, which allows no provider host, so every tunnel is refused. The gate route is there, but without the lease every gate call is refused.
- The checkout is a new worktree pinned to the author's commit and mounted read-only, with its own Git metadata directory, also read-only. The daemon deletes both when the step ends.
- That Git metadata also holds the ref `refs/sablecourt/author-base`, naming the run's first author base (see [the author base](#the-author-base)).
- The time limit (`timeoutSeconds`) starts when the daemon sets up the proxy and network, not when the command starts.

## The result

Each stream and the joined output are bounded by one function, `redact.Tail`, in the same order. It redacts the whole text first, so a secret that spans the cut is replaced whole. A private key whose `BEGIN` line has no `END` line after it matches no redaction rule, so `redact.Tail` replaces everything from that `BEGIN` line to the end with `[REDACTED:private_key]` before any cut. Output after an unfinished private key is dropped from the tail, diagnostics included. It then cuts the redacted text to the limit, drops the first line of what is left (a cut can fall inside a secret, whose rest no rule would match) and everything up to a private key's `END` line when the cut fell inside the key, and redacts the cut text again until it stops changing; if that grows it past the limit, it cuts again. Any cut, whether the text was too long or redaction made it longer, counts as an overflow, so a step never passes with `output_truncated` false after a cut. The overflow flag comes only from a recorded cut: output that only prints the text `[REDACTED:output limit]` is not an overflow.

The runner keeps at most 1 MiB of each stream. Past that it keeps the stream's last 1 MiB and the 64 KiB before it: the 64 KiB is redacted together with the tail, so a key name or header just before the cut is seen with its value, but none of it is shown. While the runner drops older bytes it records whether they end inside an unfinished private key; if they do, the kept bytes up to the next `END` line are hidden as key body, or all of them when no `END` line follows. Key markers are found by pattern, which is best effort: a `BEGIN` and an `END` line that share their dashes on one line, or a `BEGIN` line so long that the runner drops its start before the line ends, are not tracked across a cut (issue 244). The command's standard output and standard error are each bounded this way by the runner, joined (output first), made valid UTF-8 (each run of invalid bytes becomes one U+FFFD, so encoding the result as JSON cannot grow it), then bounded again to `maxOutputBytes`, so storing the output never changes it or grows it past the cap. The step's result is the artifact `command-result` (`application/json`); the control plane redacts it field by field, so the stored JSON always decodes:

| Field | Meaning |
| --- | --- |
| `command` | The command's name. |
| `outcome` | `passed`, `failed` (nonzero exit, or output lost), `timeout` or `output_over_cap`. |
| `exit_code` | The command's exit status, or `-1` when it never exited (time limit, stop). |
| `output` | The redacted output, at most `maxOutputBytes`. |
| `output_truncated` | Set when the output passed the cap or a stream passed the runner's 1 MiB limit; only the last `maxOutputBytes` bytes of the kept output are stored. |

Only `passed` completes the step. Any other outcome fails it, including `output_over_cap` with exit status 0. The step keeps no session log or final message, so no copy of the output exceeds the cap. Like every coding task it also stores `git-status` (of the read-only checkout), `git-commit` (the pinned commit it checked) and `git-base` (for a command step, also the pinned commit).

When a command step fails on any path (the command's outcome, a refusal, a launch that could not be set up, observation failures, or a daemon restart) or is refused, the control plane adds an event to the run's first task and marks the run halted, so no later step starts. A step that failed without a stored result has outcome `failed` and no `exit_code` in the event:

| Event on the run | Fields |
| --- | --- |
| `workflow_halted_command_failed` | `command_task_id`, `step`, `command`, `outcome`, `exit_code`, `failure_reason` |
| `workflow_halted_command_refused` | `command_task_id`, `step`, `command`, `refusal`, `failure_reason` |

`failure_reason` is the command's `failureReason` from the manifest, so a workflow can say what to do instead, for example which other workflow to start.

## The author base

Each coding task reports the commit its checkout started from as the artifact `git-base`, next to `git-commit`. The value comes from the daemon's own record of the worktree, not from reading the repository later. When an author step completes, the control plane copies that author's `git-base` into the registration of the next step, and every later step registration of the run carries it on. A step that already has a base never takes a new one, and a verifier or command step never sets one. A retry of an author step, by hand or after a verifier's `CHANGES_REQUESTED`, keeps the base of the run's first author, even though the retry starts from the failed attempt's commit. When the failed attempt was the first author, the retry takes that attempt's `git-base`. The registrations are stored with their tasks, so the base survives a daemon restart.

Before a command step starts, the control plane refuses it with `author_base_unavailable` when its registration has no base, or one that is not a full commit ID of the same length as the pinned commit. The adapter then checks that the base names a commit in the configured repository and that it is the pinned commit or an ancestor of it (`git merge-base --is-ancestor`), and refuses the step with the same reason otherwise. Every command step needs a base, not only a guard, so a run whose registrations were written before this check existed halts at its first command step. Because the base is an ancestor of the pinned commit, its objects are already in the container's Git metadata: the ref only names a commit the container could already read.

## The invariant path guard

The example coding court ([`examples/coding-court/coding-court.yaml`](https://github.com/shadewright-labs/sablecourt/blob/main/examples/coding-court/coding-court.yaml)) declares a command `invariant-paths` on the `developer-go` workload and runs it after the author in the `coding` workflow. Its argv is `sh -c <script> guard <path>...`: the script and the path list are both operator manifest, never read from the repository (invariant 3). The script:

1. With an empty path list, prints that no path was checked and passes.
2. Fails (exit 2) on a list entry that is empty, absolute, or contains `.` or `..` segments or a double slash.
3. Fails (exit 2) when `refs/sablecourt/author-base` does not name a commit, or that commit is not an ancestor of `HEAD`.
4. Runs `git diff --name-status -z -M <base> HEAD` and fails (exit 2) if Git exits nonzero.
5. Compares every path in that output, both sides of a rename, with each list entry. An entry matches the path itself and everything below it as a directory: `internal/redact/` and `internal/redact` both match `internal/redact/rules.go`, and neither matches `internal/redactor/x.go`. The status letters of the output are compared too; a status that equals a list entry fails the step, never passes it.
6. On a match, prints the matching paths and exits 1. Otherwise prints that no changed path is on the list and passes.

The diff spans the run's first author base to the pinned commit, so the agent's own commits, the two snapshot commits the adapter makes for staged and unstaged edits, and a failed attempt before a retry are all inside it. A failed guard halts the run like any failed command step, before the reviewer starts, and the halt event's `failure_reason` is the manifest's text, which names the `coding-invariant` workflow. The example list (`internal/redact/`, `internal/policy/`, `internal/approval/`, `internal/gate/`, `internal/audit/`) is a placeholder to replace with the repository's own invariant paths. The script is held to the argv limits on the [limits page](../reference/limits.md#command-steps).

## Steps after a command step

A step after a command step follows the author's task, not the command's: a verifier gets the author's artifacts in its prompt, the author's commit as its checkout, and the author's retry budget, and a `CHANGES_REQUESTED` verdict sends the author step back. A command step runs again after each author retry.

## Stated limits

- `/scratch` is mounted `noexec` and the checkout is read-only, so a command cannot run a program it builds. `go test`, for example, builds its test binary into a temporary directory and runs it there, and fails in a command step.
- The checkout's Git metadata holds only the objects reachable from the author's commit and `HEAD`, with no other branch, plus the `refs/sablecourt/author-base` ref. A command compares the commit with its ancestors only.
- The guard compares paths, not content: a change made and then undone between the base and the pinned commit leaves no path in the diff and passes. Paths are split on line breaks after the NUL-separated diff is read, so a file name with a line break can only add a match, never hide one.
- A proxy image built before command steps existed does not know the `command` profile, and the proxy name does not change with its code. Command steps are then refused with `container_spec_refused` until `sablectl harness build` builds the proxy again.
- If the daemon restarts while a command runs, its output is lost: the step fails with no `command-result` artifact, and the run halts with outcome `failed`.
- Standard output and standard error are kept apart and joined after the run, so their order relative to each other is lost. A stream longer than 1 MiB fails the step with `output_over_cap`, and only its last whole lines are kept. The cap then keeps the end of the joined output, so when standard error is long the stored output comes from it alone. A stream with no line break in its last 1 MiB after the cut is replaced by `[REDACTED:output limit]`, and output over `maxOutputBytes` with no line break left after the cut is stored empty.
- A secret whose key or header lies more than 64 KiB before the runner's cut, with only whitespace or a long quoted value in between, is not seen whole: its value can stay in the kept tail. Pattern rules are best effort (invariant 3).
