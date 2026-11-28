# Council

Status: partly built. Missing: knowledge of subscription windows (#36). The end-to-end smoke test (#80) is still open. See "Not built yet".

A council puts one question to `claude`, `codex` and `agy` at once, lets each read the others' answers, and ends with a digest the daemon writes from the recorded answers. It is a [Conclave](conclaves.md) with a fixed three-seat panel, a scheduler that works in rounds, and ceilings on rounds, starts and time. Each turn is one launch of one harness, in its own [harness container](harness-containers.md), built by the same function as a standalone coding task's container. Without a container runtime the daemon has no coding runtime and refuses to open a council, unless host mode is forced with `--unsafe-host-harnesses`, which puts the turns outside invariants 2 and 3. The scheduler is code, not a model: it counts, reserves and records. The harnesses answer, and a writer seat, when the API names one, can also change its own worktree.

The picture shows the shape. Each turn's packet carries the question and the Decrees addressed to that seat's role or to `all`. The scheduler moves to the next round only after all three answers of the round before it are recorded, and a later round's packet carries the earlier answers the seat may see. At close the daemon writes a digest from the recorded turns. The ceilings are in "Ceilings and how a council ends".

[![What a council looks like: the question and the Decrees addressed to a seat go into each turn of three seats, claude, codex and agy; a barrier holds round 2 until all three round 1 answers are recorded; round 2 packets carry the round 1 answers each seat may see; the daemon writes a digest at close; by default the ceilings are two rounds, six starts and 15 minutes.](../diagrams/council-structure.svg)](../diagrams/council-structure.svg "Open full size")

## What `sablectl council` does

`sablectl council "question"` (or a prompt for the question) opens a council, prints each answer as it appears, then prints the digest. With no flags it uses the current directory, the live working tree, two rounds, all three harnesses in parallel and the default ceilings. The question may be at most 30 KiB, and the CLI redacts it before sending it. The CLI sends `POST /api/v1/councils` with a random `Idempotency-Key`. The same key with the same request returns the same council; the same key with a different request is refused with a conflict.

The daemon refuses to open a council in Citadel mode. It also refuses when no coding runtime can start a turn: with no container runtime and host mode off (or forced on with no working Seatbelt), it refuses with a message that names the missing runtime, before the checkout and harness checks. It refuses when the repository fails the checks under "Checkouts". With Docker configured (harness containers, the supported path), it then runs a preflight for `claude`, `codex` and `agy`, in that order, and the first failure refuses the request: the local image, the labelled login volume, for `claude` and `agy` the tool volume and the installed tool (pinned version and SHA-256), and a credential file in the login volume, which a probe container with no network tests by path without opening it. [Council harness images and logins](../council-harnesses.md) lists the refusals and the command each names. In this mode the daemon does not look up the harness executables on its `PATH` and does not run the Seatbelt check; the checkout check still runs `git` from the daemon's `PATH`. In forced host mode (`--unsafe-host-harnesses`, outside invariants 2 and 3), it refuses with "required council executable unavailable: <role>" when `claude`, `codex` or `agy` does not resolve to an absolute path on the daemon's `PATH`, and, after the checkout check, with "council checkout confinement is unavailable" when the Seatbelt check fails. Last, it refuses when a panel workload is missing, outside the sandbox zone, not a coding workload with the sandbox of the active mode (`container` with Docker, `seatbelt` in forced host mode), or holds a Model Gate, MCP tool, MCP server, action or delegation grant. When the daemon registers the built-in panel (not in Citadel mode, and with a coding adapter), a manifest workload named `claude`, `codex` or `agy` is rejected.

[![The council preflight with Docker: for claude, codex and agy in that order, the image, the login volume, and the tool (claude and agy) and login file are checked; the first failed check refuses the request, naming the build, login or install command unless it was a timeout or a backend fault; when all pass the daemon runs a dispatch check and creates the council.](../diagrams/council-harness-preflight.svg)](../diagrams/council-harness-preflight.svg "Open full size")

## Rounds and the barrier

In round one each seat answers independently. Round two starts only when all three seats have a recorded answer for round one: a missing or failed turn holds the barrier, and a retry spends from the remaining starts. With `--sequential` one seat runs at a time, in the order given by `--with` (claude, codex, agy by default), and each waits for the answer of the seat before it.

[![One council turn across the CLI, daemon, harness container, proxy and provider.](../diagrams/council-rounds.svg)](../diagrams/council-rounds.svg "Open full size")

The diagram shows one seat of one round on the Docker path. The three seats of a round run side by side. The lines:

1. `sablectl` sends the question with the operator credential. The daemon stores the council: question, panel, checkout, ceilings and deadline.
2. The council broker reserves a turn for `(council, round, role, attempt)` and creates its task in the same transaction.
3. The reconciler claims the task and mints a task lease that expires no later than the council deadline. The store keeps only the lease's SHA-256.
4. The coding runtime adapter starts a per-launch proxy and gives it the lease digest, not the lease.
5. The adapter has the Docker engine start the harness container and writes the lease and the prompt to the container's standard input over the engine's attach stream. Inside the container a startup script writes the lease to a mode-0600 file in the memory-backed scratch directory.
6. A gate call goes from the harness to the proxy as plain HTTP inside the per-launch internal network, with the lease in a bearer header. The proxy checks the route list and compares the lease with its digest, then forwards the call to a per-launch gate in the daemon. That gate checks the route list and the digest again, and the daemon's gate routes look the lease up in the store.
7. Provider traffic leaves through the proxy as a `CONNECT` tunnel to one of the harness's own provider hosts on port 443. The proxy refuses any other host or port.
8. When the harness exits, the Docker adapter has its standard output from the attach stream and its exit code from the engine. The broker records an answer only when the exit code is 0 and the output holds a non-empty final message.
9. When all three answers of a round are in, the broker plans the next round. When the council closes, the store writes the digest.

What is recorded: task state changes go to `task_events`. The audit chain gets `task.submit` for each turn, `conclave.ward` when a Ward pauses the room, `conclave.run_window_reset` when a Decree resumes it, and `conclave.close` when the operator closes or stops it (`sablectl conclave close` or Ctrl-C in the CLI). A close by the scheduler itself (`nothing_to_add`, `round_cap`, `start_cap` or the deadline) writes no audit row. Tool calls through the MCP Gate append their own audit rows. Answers, votes, reservations and the digest live in the council tables, not in the chain.

The boundaries around that container, its proxy and the daemon are drawn on the [harness containers](harness-containers.md) page and repeated here.

[![Trust boundaries of a harness container, its proxy and the daemon.](../diagrams/harness-container.svg)](../diagrams/harness-container.svg "Open full size")

## What a turn sees

A turn gets a context packet: the question, the Decrees addressed to its role or to `all`, and redacted copies of the earlier answers it may see, its own included. In parallel mode that is every recorded answer of earlier rounds. In sequential mode it also includes the answers of the seats before it in its own round. Peer text gets at most 16 KiB. When it does not fit, older rounds shrink to the first line of each answer (`[older detail omitted]`), then drop (`[older rounds omitted]`), and last the newest round is cut so each answer keeps at most the same number of characters (`[peer text omitted]`). The packet text is at most 32 KiB and its JSON encoding at most 63 KiB; the opening question alone must encode to at most 59 KiB. Mentions never start council turns, and a turn sees an answer only after it is recorded. While the council is active, the harness can read the same view through the gate. Writes from a seat (messages and Relics) are refused, because a council has no manifest to resolve the seat's role.

## Ceilings and how a council ends

| Ceiling | Default | Limit | Set by |
| --- | --- | --- | --- |
| Rounds | 2 | at least 1 | `--rounds`, API `rounds` |
| Starts, retries included | 6 | 1 to 6 | API `starts` |
| Wall time from opening | 15 minutes | at least 1 second | API `durationSeconds` |

A zero or negative value is rejected, not replaced by the default. A Decree changes none of them. The scheduler closes a council by itself by one of three rules. After a round completes it closes with `nothing_to_add` when all three seats voted to stop, and with `round_cap` when that round is the round ceiling. A round that cannot finish because every start is spent and no turn is running closes with `start_cap`.

[![How a council ends: nothing to add, round cap or start cap, otherwise the next round.](../diagrams/council-finish.svg)](../diagrams/council-finish.svg "Open full size")

A seat votes to stop only when its entire trimmed final message is `NOTHING TO ADD`. An empty final message records no answer, and the seat is retried while starts remain. The deadline and Ctrl-C in the CLI end the council as `cancelled`; `sablectl conclave close` ends it as `completed`. All three cancel the turns still active and write the digest. A Ward pauses the council in `input-required`, and the council waits for a Decree or its deadline. The CLI stops at the pause and exits with an error. That error names the Ward reason when at least one seat has answered; with no answers it reports only that no seat answered.

[![States of one council turn: reserved, running, recorded or failed, with retry back to reserved.](../diagrams/council-turn-states.svg)](../diagrams/council-turn-states.svg "Open full size")

A failed, cancelled or answerless turn is retried as the next attempt while starts remain. After a daemon restart, recovery revokes the lease of each turn that was running and removes its container, proxy and network, because the per-launch gate ended with the old daemon. If the adapter had already written the turn's result to its state file, the task then completes or fails from that result. Otherwise it fails as lost on restart, and the council retries or closes. `sablectl task retry` refuses council tasks.

## Checkouts

`--live` (the default) mounts a read-only view of the working tree, uncommitted changes included. `--ref <ref>` pins a branch, tag, `HEAD` or full commit ID to one commit and mounts a read-only checkout of it. The two flags cannot be combined. The API also takes `writer`, one of the three roles, never with `live`; without `ref` it pins `HEAD`. That role gets a writable worktree on a branch `sablecourt/council-<key>`, the others a read-only review checkout, and the writer's redacted diff is stored as a Relic that later packets include. `sablectl council --writer <seat>` sends that field.

The daemon refuses a repository that is not owned by the daemon's user, that has no `.git` directory of its own or is not the primary worktree, or that overlaps the daemon state directory or a credential directory under the home directory (such as `.ssh`, `.config`, `.claude`, `.codex` or `Library/Keychains`). It refuses a ref name that is both a branch and a tag on different commits. A second writer role for the same council is refused when its turn launches.

## Digest

At close the daemon writes the Relic `digest`, authored by `system`: the stop reason, then each round and seat in panel order with the recorded answer or `Missing answer`, and the name and SHA-256 of any evidence Relic of that attempt. Each seat gets an equal share of the digest's size limit, and a longer answer is cut at `[omitted]`. The digest does not summarise or merge answers. The CLI prints it.

## Not built yet

- The preflight makes no provider call and does not open the credential file, so it cannot tell an expired or revoked login from a valid one. Such a login passes it when its file is a regular, non-empty file at an expected path and the other checks pass.
- Knowing each subscription's usage windows before a round starts (#36).
- Model Gate accounting does not cover subscription inference ([ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)). The council ceilings bound the number of turns and the council's wall time. Each turn also has its own wall time, at most the time left before the deadline, and the proxy allows a harness at most 16 tunnels at once, each at most 15 minutes. None of these counts tokens or cost.

## In depth

- [ADR-0013](../adr/0013-bounded-council-rounds.md) and [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md).
- `cmd/sablectl/council.go`, `cmd/sablecourtd/councils.go`, `internal/conclave/rounds.go`, `internal/conclave/council_driver.go`, `internal/conclave/council_results.go`, `internal/contextpack/council.go`, `internal/store/council.go`, `internal/controlplane/recovery.go`.
- Next: [harness containers](harness-containers.md), [run a council](../how-to/run-a-council.md).
