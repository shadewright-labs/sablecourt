# Council harness images and logins

In Sanctum, a council runs each CLI harness (`claude`, `codex`, `agy`) in its own container, as ADR-0011 describes. Before a council exists, `sablecourtd` checks that every harness can start. The check never pulls an image, never reaches a provider, and never uses a login from the host.

## Setup, once per harness

1. Build the local images: `sablectl harness build <role>`. The role image and the proxy image are built from digest-pinned bases and stay on this machine.
2. Log in: `sablectl harness login <role>`. The command creates the harness volume if it does not exist, with the labels Sablecourt looks for, and starts the harness login in a container with your terminal attached. The container reaches the network only through a login proxy that allows that provider's hosts. The login stays in the volume `sablecourt-harness-<role>`. Nothing is copied from the host.

`sablectl harness login` talks to Docker itself. The daemon is not involved and no terminal stream crosses its API. Run it again to replace a login.

## What the preflight checks

[![A council request passes three checks for each of claude, codex and agy: the image exists, the login volume is the labelled one, and it holds a credential file. A failed check refuses the request with the command to run; when all pass, the council is created.](diagrams/council-harness-preflight.svg)](diagrams/council-harness-preflight.svg "Open full size")

For each of the three roles, in this order:

1. **Image.** The role image must exist locally. If not, the request is refused with `sablectl harness build <role>`.
2. **Volume.** The login volume must exist, use the `local` driver with no driver options, and carry the Sablecourt labels for that role. A missing volume is refused with `sablectl harness login <role>`. The daemon never creates a volume. A volume that exists without the labels is refused too, with a note to remove it by hand: Sablecourt does not trust or adopt a volume it did not create.
3. **Login.** A short container from the pinned image runs with no network, no capabilities, a read-only root filesystem, and the volume mounted read-only. It tests only whether a credential file exists and is not empty, so it reads file names and sizes, never contents. The files are `/home/harness/.claude/.credentials.json` for `claude`, `/home/harness/.codex/auth.json` for `codex`, and `oauth_tokens.json` under `/home/harness/.gemini/` or `/home/harness/.gemini/antigravity-cli/` for `agy`. No credential file means the request is refused with `sablectl harness login <role>`.

The probe makes no call to a provider, so it cannot tell an expired login from a valid one. An expired login passes the preflight and fails at the turn with the harness's own error.

A refusal happens before the council, its rooms or its tasks are recorded, so it leaves nothing behind. The checks use their own 45 second limit.

## Errors

| Case | HTTP status | Message names |
| --- | --- | --- |
| Image missing | 412 | the role and `sablectl harness build <role>` |
| Volume missing, or no credential file | 412 | the role and `sablectl harness login <role>` |
| Volume without the Sablecourt labels | 409 | the volume problem and `sablectl harness login <role>` |
| Docker or harness configuration fails | 503 | the role only, with no command |
| Check timed out | 503 | the role only |

A backend failure gives no command because nothing is known to be missing. No message carries a credential or file contents, and Docker error text is not copied into the response.

## Panel workloads

In container mode the three built-in panel workloads record `container` as their sandbox. The host executable and Seatbelt checks do not run, because the container is the confinement. Without a Docker runtime, `sablecourtd` keeps the host path, whose checks are unchanged.
