# Harness images and tool install

Status: built for local images. Publishing the images to a registry is not built yet.

In container mode, each council turn runs in a container from the image `sablecourt-harness-<role>`, next to a proxy container from the image `sablecourt-harness-proxy`. `sablectl harness build` builds both from a Dockerfile embedded in `sablectl`. How these containers are isolated and what they check is described in [harness containers](../how-it-works/harness-containers.md) and in the 2026-11-26 amendment of [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md).

## What each image contains

| Harness | Licence of the tool | Image contents | Where the tool comes from |
| --- | --- | --- | --- |
| Codex CLI (`codex`) | Apache-2.0 | The unmodified Codex CLI 0.172.0 at `/usr/local/bin/codex`, with its `LICENSE` and `NOTICE` in `/licenses/codex/` | Built into the image from the GitHub release `rust-v0.172.0` |
| Claude Code (`claude`) | Proprietary | No vendor software, an empty `/opt/harness-tool` | Installed into its own volume from Anthropic's download host by `sablectl harness install claude` or `sablectl harness login claude` |
| Antigravity CLI (`agy`) | Proprietary | No vendor software, an empty `/opt/harness-tool` | Installed into its own volume from Google's download host by `sablectl harness install agy` or `sablectl harness login agy` |
| Council proxy | Apache-2.0 (Sablecourt), BSD-style (Go) | The proxy compiled from source, with `/licenses/sablecourt/LICENSE` and `/licenses/go/LICENSE` | Compiled locally by `sablectl harness build` |

Every image starts from the digest-pinned Debian `bookworm-slim` with packages from the snapshot.debian.org archive of 2026-11-23, and has a `/licenses/README` that lists what it contains. Its `org.opencontainers.image.licenses` label is `Apache-2.0` for `codex`, `Apache-2.0 AND BSD-3-Clause` for the proxy, and `NOASSERTION` for `claude` and `agy`.

Why: Apache-2.0 lets the Codex CLI be passed on with its licence and notice. Sablecourt treats Claude Code and the Antigravity CLI as proprietary software that gives no such permission (the notes are in `internal/harnessimages/licenses/image/`), so their images carry no vendor software and the tool is downloaded from the vendor. That classification is not legal advice.

## Requirements

- A Docker engine that can build and run `linux/arm64` images: an arm64 engine, or one with emulation available. No other platform is pinned.
- The Go toolchain and a Sablecourt source checkout, because the build compiles the council proxy (`--source`, default `.`).
- For `claude` and `agy`: HTTPS access from the machine that runs `sablectl` to `downloads.claude.ai` (`claude`) or `storage.googleapis.com` (`agy`). The download honours `HTTPS_PROXY` and `NO_PROXY` ([configuration](../reference/configuration.md)).

## Build and install

```sh
sablectl harness build <role>
sablectl harness install <role>    # claude and agy
sablectl harness login <role>
```

- `codex`: the build puts the Codex CLI into the image. There is nothing to install: `sablectl harness install codex` installs nothing and, once it has a Docker client, stops with `the codex image carries its tool; nothing to install`.
- `claude` and `agy`: `sablectl harness install <role>` downloads the pinned tool on the machine that runs `sablectl`, checks it, and installs it into the volume `sablecourt-harness-<role>-tool`. It does not check what is already installed before it downloads. On success it prints `Installed <tool> <version> (SHA-256 checked) into volume sablecourt-harness-<role>-tool`.
- `claude` and `agy`: `sablectl harness login <role>` runs the same install first when the login volume does not exist yet, or when its check finds the tool volume or the tool missing, of another version or not matching its digest. The login itself is described in [log in to harnesses](log-in-harnesses.md).

What the install verifies: for `claude`, the size and SHA-256 of the binary pinned for version 2.1.332; for `agy`, the size and SHA-512 of the archive, then the size and SHA-256 of the binary in it. The download follows no redirect. The pinned digests identify the bytes Sablecourt accepts, not who published them. Sablecourt does not verify the vendor's release manifest signature and does not run Anthropic's `install.sh`.

For `codex`, the build checks the release archive against a pinned SHA-256 before it extracts it, and checks the version the CLI reports. It does not verify a Sigstore bundle. A build of the `codex` target for another architecture fails.

Sablecourt sets `DISABLE_UPDATES=1` for `claude`; Claude Code's [environment variable reference](https://code.claude.com/docs/en/env-vars) says this setting blocks all updates, including manual `claude update` and `claude install`.

## Upgrade

`sablectl` and `sablecourtd` each carry their own compiled pins, so run both from the same release.

1. Run the new `sablecourtd` and `sablectl`.
2. For `codex`, run `sablectl harness build codex`.
3. For `claude` and `agy`, run `sablectl harness build <role>`, then `sablectl harness install <role>`. If the image of the new tag is not present, the install downloads and checks the tool and then stops with `image for <role> is missing: run sablectl harness build <role>`.

## When something is refused

The messages with which the daemon refuses a new container-mode council include the ones below. Do what the message names, then ask again.

| Message | HTTP status | What to do |
| --- | --- | --- |
| `council image for <role> is missing: run sablectl harness build <role>` | 412 | Build the image. |
| `council login for <role> is missing: run sablectl harness login <role>` | 412 | Log in. |
| `council tool for <role> is not installed: run sablectl harness install <role>` | 412 | Install the tool (`claude`, `agy`). |
| `council tool for <role> is not the pinned version: run sablectl harness install <role>` | 412 | Install the tool (`claude`, `agy`). |
| `council tool for <role> does not match its pinned SHA-256: run sablectl harness install <role>` | 412 | Install the tool (`claude`, `agy`). |
| `council tool volume for <role> exists but is not a local Sablecourt tool volume for this harness (...): remove it by hand, then run sablectl harness install <role>` | 409 | `docker volume rm sablecourt-harness-<role>-tool`, then install. |
| `council login volume for <role> exists but is not a local Sablecourt volume for this harness (...): remove it by hand, then run sablectl harness login <role>` | 409 | `docker volume rm sablecourt-harness-<role>`, then log in again. |

A `claude` or `agy` turn whose own tool check fails ends with exit status 125. Its recorded output normally holds a message such as `council tool claude is not the pinned version 2.1.332: run sablectl harness install claude`. Run the command it names.

`sablectl harness install` stops with `volume sablecourt-harness-<role>-tool exists but is not a local Sablecourt tool volume for <role> (...): remove it by hand and run this command again` for a volume of that name whose driver, options or labels are not the ones Sablecourt sets. When the tool or `VERSION` path in the volume is a directory or a symlink, the install fails. The install script reports `exists and is not a regular file: remove the tool volume and install again`, which the command shows when it could read the container's output and exit status. In both cases remove the volume with `docker volume rm` and run the install again.

For `codex`, the launch check covers the image tag and its configuration, not the binary inside the image (#151).

## Antigravity terms

Sablecourt assumes that Google's terms for the Antigravity CLI restrict third-party tools that access the service, and it does not check them. Every council has an `agy` seat, in container mode and in host mode, so what you decide, under your own agreement with Google, is whether to run councils.

## In depth

- `internal/harnessimages/Dockerfile`, `internal/harnessimages/images.go`, `internal/harnessimages/tool.go`, `internal/harnessimages/licenses/`.
- `internal/runtime/docker/council_tool.go`, `internal/runtime/docker/council_preflight.go`, `cmd/sablectl/harness_install.go`.
- [Council harness images and logins](../council-harnesses.md), [harness containers](../how-it-works/harness-containers.md), [log in to harnesses](log-in-harnesses.md), [run a council](run-a-council.md).
