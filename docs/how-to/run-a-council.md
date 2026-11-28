# Run a council

Status: partly built. A round is not refused up front when an image or login is missing (#79), and the end-to-end smoke test (#80) is still open.

Ask `claude`, `codex` and `agy` one question about a repository and read the answers and the digest.

## Prerequisites

- The daemon is running in Sanctum mode and `sablectl` can reach it ([install and start](install-and-start.md)).
- `claude`, `codex` and `agy` are on the `PATH` of `sablectl` (it checks before sending), and of the daemon only in the unsupported host mode.
- Docker Engine 28.0 or newer, the three harness images built and each harness logged in ([log in to harnesses](log-in-harnesses.md)). The `claude` and `agy` images carry no vendor tool: `sablectl harness login` installs it when the login volume does not exist yet or the tool is missing, of another version or does not match its digest ([harness images and tool install](harness-images.md)).
- Only without Docker, and only with `--unsafe-host-harnesses` (off by default; without it the council is refused), on a macOS host where Seatbelt works: the daemon then probes it when a council opens and refuses the council on any other platform. With Docker configured it skips that probe. This is the unsupported host mode; the supported 0.1.0 path is a Docker engine and harness containers ([ADR-0011 amendment of 2026-11-27](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md#amendment-2026-11-27-container-only-harnesses-for-010)).
- A Git checkout owned by the daemon's user, with its own `.git` directory (not a linked worktree), outside the daemon state directory and outside credential directories such as `~/.ssh` and `~/.config`.

## Steps

1. Ask the question from the repository, using its current working tree (read-only, uncommitted changes included):

   ```sh
   sablectl council "Is the retry logic in the sync loop safe to change?"
   ```

2. Or pin a branch, tag or commit instead. `--ref` and `--live` cannot be combined:

   ```sh
   sablectl council --ref main "Review the layering of the store package"
   ```

3. Adjust the council. `--repo <dir>` picks another repository, `--rounds <n>` sets the round ceiling (default 2, at least 1), `--sequential` runs the seats one after another in each round, and `--with claude,codex,agy` sets the order (all three are always required). Starts and wall time have no flag; the API takes `starts` (1 to 6) and `durationSeconds`.
4. Read the output. The CLI prints `--- Round N ---`, then each answer as `@role (review, #id)`. A line starting with `!` is a `system` message such as a Ward. Add the global `--json` flag for one JSON document instead. While the rounds run, a terminal also shows a short animation on standard error with the round and how many seats answered; `--no-art` or `SABLECOURT_NO_ART=1` turns it off (see [wait animation](../reference/cli.md#wait-animation)).
5. Stop early with Ctrl-C. The CLI cancels the council and its turns, and the daemon still writes the digest.

## Check it worked

The output ends with `=== Digest ===`. For a closed council that is the stop reason (`nothing_to_add`, `round_cap`, `start_cap`, `deadline`, `owner_stop` after Ctrl-C, or `owner_close`), then each round and seat. A council paused by a Ward has no digest yet, and the CLI prints `no digest recorded`. `Missing answer` means that seat has no recorded answer for that round. The command exits with an error when no seat answered, when the council was cancelled, or when a Ward paused it. A paused council stays open: resume it with `sablectl conclave decree <id> "<message>"`, or close it with `sablectl conclave close <id>`. Inspect any council later with `sablectl conclave inspect <id>`.

The writer mode (one seat gets a writable worktree) is available with `--writer <seat>` (one of `claude`, `codex` or `agy`, not with `--live`), which the CLI sends as the API field `writer`.

## In depth

[Council](../how-it-works/council.md), [harness containers](../how-it-works/harness-containers.md), [watch and steer](watch-and-steer.md).
