# Council harness images and logins

In Sanctum container mode, each council harness turn (`claude`, `codex`, `agy`) runs in its own container, as ADR-0011 describes. Before a council is created, `sablecourtd` checks each role's image, login volume, tool (for `claude` and `agy`) and credential path, and stops at the first failure. Container mode applies when a Docker runtime is configured; without one, the host path with its own checks is used.

## Setup, once per harness

1. Build the local images: `sablectl harness build <role>`. The command builds the role image and the proxy image from the digest-pinned base in the repository Dockerfile, using the Docker engine your Docker settings select. It does not push them. The `codex` image contains the Codex CLI; the `claude` and `agy` images contain no vendor software ([harness images and tool install](how-to/harness-images.md)).
2. Log in: `sablectl harness login <role>`. For `claude` and `agy` the command first installs the pinned tool into the volume `sablecourt-harness-<role>-tool` when the login volume does not exist yet or the tool is missing, of another version or does not match its SHA-256 (`sablectl harness install <role>` runs only this step). It then creates the volume `sablecourt-harness-<role>` if it does not exist, with the labels Sablecourt looks for, and starts the harness login in a container with your terminal attached. The container joins an internal network and reaches the network only through a login proxy that allows that provider's hosts on port 443. The login is stored in the volume. The command forwards the text you type to the container and does not copy any login file from the host.

`sablectl harness login` talks to Docker itself. The daemon is not involved and no terminal stream crosses its API. After the session it runs the credential check below and reports whether a credential file is present. Run it again to log in again.

## What the preflight checks

[![A council request passes three checks for each of claude, codex and agy: the image is present, the login volume is valid, and a credential file is found. A failed check refuses the request with the command to run; when all pass, the daemon runs a dispatch check and then creates the council.](diagrams/council-harness-preflight.svg)](diagrams/council-harness-preflight.svg "Open full size")

The preflight runs for a new council request in container mode. A repeated request with the same idempotency key and the same content returns the stored council before the preflight. For each role in the order `claude`, `codex`, `agy`:

1. **Image.** The image for the role must exist in the local Docker engine and have the expected configuration. Docker never pulls it. If the image is not found, the request is refused with `sablectl harness build <role>`.
2. **Volume.** The login volume is inspected and must use the `local` driver, have no driver options, and carry the Sablecourt labels for that role. A missing volume is refused with `sablectl harness login <role>`. The preflight itself never creates a volume. A volume that exists but fails these conditions is refused with a note to remove it by hand and run the login command. For `claude` and `agy` the tool volume `sablecourt-harness-<role>-tool` is checked the same way (name, `local` driver, no options and its own label `sablecourt.io/harness-tool=<role>`), except that a tool volume must carry exactly its two expected labels while a login volume may carry more; a missing one is refused with `sablectl harness install <role>`.
3. **Login.** A short container from the role image runs with no network, all capabilities dropped, a read-only root filesystem and the volume mounted read-only. The volume is inspected again after the container is created. The container tests whether any expected path is a regular file, not a symlink, with a size above zero. The expected paths are `/home/harness/.claude/.credentials.json` for `claude`, `/home/harness/.codex/auth.json` for `codex`, and `oauth_tokens.json` under `/home/harness/.gemini/` or `/home/harness/.gemini/antigravity-cli/` for `agy`. The check does not open the file. If no path qualifies, the request is refused with `sablectl harness login <role>`.

   For `claude` and `agy` the probe container also mounts the tool volume read-only, and the daemon inspects that volume again after it creates the container and before it starts it. Before the credential test the probe checks that the tool is a regular executable file and not a symlink, that `VERSION` names the pinned version and that its SHA-256 is the pinned one, without running it. A failure is refused with `sablectl harness install <role>`.

The probe makes no call to a provider, so it cannot tell an expired or revoked login from a valid one. Such a login passes the preflight and fails at the turn. The probe container is removed afterwards, on a best-effort basis.

A refused request writes no council, room or task record. After all three roles pass, the daemon checks that the panel workloads can be dispatched, and then creates the council. The checks run under their own 45 second limit, separate from the 10 second limit of the checkout check, and each probe container has a 20 second limit of its own.

## Errors

| Case | HTTP status | Message |
| --- | --- | --- |
| Image not found | 412 | names the role and `sablectl harness build <role>` |
| Volume missing, or no qualifying credential path | 412 | names the role and `sablectl harness login <role>` |
| Tool missing, of another version, or not matching its pinned SHA-256 (`claude`, `agy`) | 412 | names the role and `sablectl harness install <role>` |
| Tool volume exists but fails the driver, options or label conditions | 409 | says the tool volume is not a local Sablecourt tool volume, to remove it by hand and run `sablectl harness install <role>` |
| Volume exists but fails the driver, options or label conditions | 409 | says the volume is not a local Sablecourt volume for this harness, to remove it by hand and run `sablectl harness login <role>` |
| Docker or the harness configuration fails | 503 | names the role and says Docker or the configuration is unavailable, with no command |
| The check ran out of time or was cancelled | 503 | names the role, says the preflight timed out or was cancelled, with no command |

The messages are fixed text built from the role name. They do not include Docker error text or any file content.

## Panel workloads

In container mode the three built-in panel workloads record `container` as their sandbox, and the council request path skips the host executable lookup and the Seatbelt check, because the container is the confinement. The daemon still runs the host sandbox availability check when it starts. Without a Docker runtime, the council request path keeps the host executable and Seatbelt checks unchanged.
