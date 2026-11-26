# Harness images and tool install

Status: built for local images. Publishing the images to a registry is not built yet.

In container mode, each council turn runs in its own container from an image named `sablecourt-harness-<role>`, next to a per-turn proxy container from the image `sablecourt-harness-proxy`. The images are defined in one Dockerfile embedded in `sablectl` (`internal/harnessimages/Dockerfile`). What each image may contain follows how Sablecourt classifies the licence of the vendor tool (the notes are in `internal/harnessimages/licenses/image/`). That classification is not legal advice.

## What each image contains

| Harness | Licence of the tool | Image contents | Where the tool comes from |
| --- | --- | --- | --- |
| Codex CLI (`codex`) | Apache-2.0 | The unmodified Codex CLI 0.172.0 at `/usr/local/bin/codex`, with its `LICENSE` and `NOTICE` in `/licenses/codex/` | Built into the image from the GitHub release `rust-v0.172.0`; the build checks the archive against a pinned SHA-256 before it extracts anything and fails on a mismatch |
| Claude Code (`claude`) | Proprietary | The base only: no vendor software, an empty `/opt/harness-tool` | `sablectl harness install`, or `sablectl harness login` when its condition below holds, downloads and checks it from Anthropic's download host, then installs it into its own volume |
| Antigravity CLI (`agy`) | Proprietary | The base only: no vendor software, an empty `/opt/harness-tool` | `sablectl harness install`, or `sablectl harness login` when its condition below holds, downloads and checks it from Google's download host, then installs it into its own volume |
| Council proxy | Apache-2.0 (Sablecourt), BSD-style (Go) | The proxy compiled from source, with `/licenses/sablecourt/LICENSE` and `/licenses/go/LICENSE` | Compiled locally by `sablectl harness build` |

The base in every image is the digest-pinned Debian `bookworm-slim` with packages from the snapshot.debian.org archive of 2026-11-23. The build removes the apt package lists after installing the packages. Every image has a `/licenses/README` that lists what it contains, and four OCI labels:

- `org.opencontainers.image.title`: the image name, for example `sablecourt-harness-claude`.
- `org.opencontainers.image.description`: for `codex`, ends with "Contains the OpenAI Codex CLI 0.172.0, unmodified."; for `claude` and `agy`, ends with "Contains no vendor software; the tool is installed at login."
- `org.opencontainers.image.licenses`: `Apache-2.0` for `codex`, `Apache-2.0 AND BSD-3-Clause` for the proxy, `NOASSERTION` for `claude` and `agy`.
- `org.opencontainers.image.source`: the Sablecourt repository.

Why: Apache-2.0 lets the Codex CLI be passed on with its licence and notice. Sablecourt treats Claude Code and the Antigravity CLI as proprietary software that gives no such permission, so their images carry no vendor software and the install of those two tools downloads them from the vendor.

## Install the tool at login

```sh
sablectl harness build claude      # or codex, agy
sablectl harness login claude
```

For `claude` and `agy`, `sablectl harness login` first installs the pinned tool when the login volume does not exist yet, or when its check reports the tool volume or tool missing, the tool of another version or not matching its digest. In no other case does it install. Then it starts the login. `sablectl harness install <role>` runs only the install step, for `claude` and `agy`, and downloads the tool without first checking what is installed. `codex` needs no install: `sablectl harness install codex` refuses with "the codex image carries its tool".

The install of the `claude` or `agy` tool:

1. Downloads one file over HTTPS from the vendor's host, on the machine that runs `sablectl`, with no cookies, no vendor credentials and no redirect followed. The URL is fixed in `internal/harnessimages/images.go`; only `downloads.claude.ai` and `storage.googleapis.com` are accepted. The host name `storage.googleapis.com` is shared by many buckets, so the host check alone does not pick Google's file. The constant URL selects the bucket and object, and the pinned digest in step 2 identifies the bytes Sablecourt accepts. Neither proves who published them. The download honours `HTTPS_PROXY` so that it works behind a corporate proxy, and Go can send proxy credentials to an authenticated proxy set in the environment. Go checks the TLS certificate for the vendor's host against the trusted roots of the machine that runs `sablectl`, so a proxy that machine trusts can inspect the traffic. The checks in step 2 are the same with or without a proxy.
   - `claude`: `https://downloads.claude.ai/claude-code-releases/2.1.332/linux-arm64/claude`, the binary itself.
   - `agy`: `https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.17-4823843970351104/linux-arm/cli_linux_arm64.tar.gz`.
2. Checks it on that machine before the file is passed to Docker. `claude`: SHA-256 and size of the binary. `agy`: SHA-512 and size of the archive, then SHA-256 and size of the one member `antigravity`. `sablectl` reads at most one byte more than the pinned size, so a server that sends more makes it write at most that one extra byte, which counts as a mismatch.
3. Creates the volume `sablecourt-harness-<role>-tool` with the labels `app.kubernetes.io/managed-by=sablecourt` and `sablecourt.io/harness-tool=<role>` if it does not exist. It refuses a volume whose labels are not exactly these two, and a volume with another driver or with driver options. Labels of a local volume cannot be changed through Docker's volume API after creation, and Sablecourt creates the volume with exactly these two labels, so an extra label is enough reason to refuse the volume.
4. Streams the binary into a short-lived install container from the harness image: user `10001:10001`, no network, all capabilities dropped, `no-new-privileges`, read-only root filesystem, the environment variables `PATH`, `HOME` and `TMPDIR` set by Sablecourt (Docker adds its usual runtime variables), and one requested mount, the writable tool volume at `/opt/harness-tool` (Docker adds its usual runtime mounts such as `/proc`). The login volume is not mounted. The container writes the binary to a temporary file of its own, checks its SHA-256 again, sets mode 0555, renames it into place, and then renames a new `VERSION` file into place.

What is verified for `claude`: the bytes equal the SHA-256 and size pinned in `images.go` for version 2.1.332. What is not verified: the release manifest's own signature (`manifest.json.sig`), because Sablecourt does not fetch the manifest at install time. Sablecourt does not run Anthropic's `install.sh` either. It downloads the pinned binary directly.

For `codex`, the image build checks the release archive against the SHA-256 pinned in the Dockerfile and in `images.go` for the asset of `rust-v0.172.0` (the Dockerfile comment records that it was read from GitHub's release data on 2026-11-26). Only `linux/arm64` is pinned; a build of the `codex` target for another architecture fails. The build does not verify a Sigstore bundle.

For `claude` and `agy`, the installed tool is on a read-only volume in the login container and in every container-mode turn, so the harness cannot overwrite it through that mount. The harness home and scratch space stay writable. Sablecourt also sets `DISABLE_UPDATES=1` for `claude`; Claude Code's [environment variable reference](https://code.claude.com/docs/en/env-vars) says this setting blocks all updates, including manual `claude update` and `claude install`.

## Upgrade

`sablectl` and `sablecourtd` each carry the pins compiled into them. `sablectl harness install` installs the `claude` or `agy` version pinned in `sablectl`. For `claude` and `agy`, the daemon checks the installed tool against its own pins (version marker and SHA-256) in preflight and before each container-mode turn. For `codex`, the pin is checked only when the image is built: the Dockerfile checks the release archive's SHA-256 and the build checks the version output. At admission and launch the daemon checks only that an image with the expected tag exists in its local Docker engine, declares no volumes and sets no environment defaults other than `HOME`, `PATH` and `TMPDIR`; it does not check the `codex` binary inside. Use both binaries from the same release. To move to a new release:

1. Run the new `sablecourtd` and `sablectl`.
2. For `codex`, run `sablectl harness build codex`. The build puts the Codex CLI of the new pin into the image. There is nothing to install: `sablectl harness install codex` is refused.
3. For `claude` and `agy`, run `sablectl harness build <role>`, then `sablectl harness install <role>`. Build first: the install downloads and checks the tool, then starts its container from the role image. If the download or the check fails, that error comes first; otherwise, if the image of the new tag is not present, it stops with `image for <role> is missing: run sablectl harness build <role>`. Instead of the install, `sablectl harness login <role>` installs the tool when its check finds another version, and then starts a new login.

## What a turn checks

For `claude` and `agy`, the tool volume is mounted read-only (and with `volume-nocopy`) at `/opt/harness-tool` in the login container and in every container-mode turn. `codex` has no tool volume and no such check. Before the tool starts, a guard in the container checks that `/opt/harness-tool/<tool>` is a regular executable file and not a symlink, that `VERSION` names the pinned version, and that the file's SHA-256 is the pinned one. A failure stops the container with exit status 125 and a message such as `council tool claude is not the pinned version 2.1.332: run sablectl harness install claude`. In a turn, the `claude` or `agy` guard runs after the lease and question are read from standard input and before the lease file is written. The guard and the start of the tool each resolve the path, so an install that finishes between the two can replace the file; that file matched the pin of the `sablectl` that ran the install (see the 2026-11-26 amendment of [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)).

Before a new container-mode council is created, the daemon's preflight runs the same check for `claude` and `agy` in its probe container and refuses the request as follows:

| Case | HTTP status | Message names |
| --- | --- | --- |
| Tool volume or tool missing | 412 | `sablectl harness install <role>` |
| Another version installed | 412 | `sablectl harness install <role>` |
| SHA-256 differs from the pin | 412 | `sablectl harness install <role>` |
| Tool volume has other labels, driver or options | 409 | remove it by hand, then `sablectl harness install <role>` |

In container mode, preflight checks the roles in the order `claude`, `codex`, `agy` and stops at the first refusal. For every role it checks the image first and the login volume next. For `claude` and `agy` it then checks the tool volume and the tool. Last, for every role, it checks the login file. The image tag contains the pinned version, so after an upgrade that changes it, a role whose image of the new tag is not present on the daemon's Docker engine is refused with `sablectl harness build <role>`, before any later check of that role.

## Fully local build

`sablectl harness build <role>` compiles the proxy on the machine that runs `sablectl`, builds the role and proxy images on the Docker engine your Docker settings select (`DOCKER_HOST`, `DOCKER_CONTEXT`), and does not push them. For `claude` and `agy` it builds the image without the tool, which `sablectl harness install` or `sablectl harness login` installs as above. The daemon looks the image up by its tag in the local Docker engine and does not check how it got there, so for `claude` and `agy` the tool path and the guard are the same for a built image and for one that was pulled or loaded under that tag, and for `codex` the binary in whichever image carries that tag and passes the image checks above is the one that runs.

## Antigravity terms

Sablecourt assumes that Google's terms for the Antigravity CLI restrict third-party tools that access the service, and it does not check them. In container mode, an `agy` council seat runs the CLI in a container whose only configured route out is the Sablecourt proxy. In host mode (no Docker runtime configured), it runs the host's `agy` CLI under a macOS Seatbelt profile that allows outbound network. Sablecourt treats both as such a use, however the CLI is packaged. Every council has an `agy` seat (the panel is fixed to `claude`, `codex` and `agy`), so what you decide, under your own agreement with Google, is whether to run councils.

## In depth

- `internal/harnessimages/Dockerfile`, `internal/harnessimages/images.go`, `internal/harnessimages/tool.go`, `internal/harnessimages/licenses/`.
- `internal/runtime/docker/council_tool.go` (install container and guard), `internal/runtime/docker/council_preflight.go`, `cmd/sablectl/harness_install.go`.
- [Council harness images and logins](../council-harnesses.md), [harness containers](../how-it-works/harness-containers.md), [run a council](run-a-council.md).
