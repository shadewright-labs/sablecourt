# Harness images and tool install

Status: built for local images. Publishing the images to a registry is not built yet (#146 leaves it to the release workflow).

Each council seat runs in a container from an image named `sablecourt-harness-<role>`, next to a proxy image `sablecourt-harness-proxy`. The images are defined in one Dockerfile embedded in `sablectl` (`internal/harnessimages/Dockerfile`). What each image may contain follows the licence of the vendor tool, as read on 2026-11-26; that reading is not legal advice.

## What each image contains

| Harness | Licence of the tool | Image contents | Where the tool comes from |
| --- | --- | --- | --- |
| Codex CLI (`codex`) | Apache-2.0 | The unmodified Codex CLI 0.172.0 at `/usr/local/bin/codex`, with its `LICENSE` and `NOTICE` in `/licenses/codex/` | Built into the image from the GitHub release `rust-v0.172.0`; the build checks the archive against a pinned SHA-256 before it extracts anything and fails on a mismatch |
| Claude Code (`claude`) | Proprietary | The base only: no vendor software, an empty `/opt/harness-tool` | Installed on your machine at login, from Anthropic's download host, into its own volume |
| Antigravity CLI (`agy`) | Proprietary | The base only: no vendor software, an empty `/opt/harness-tool` | Installed on your machine at login, from Google's download host, into its own volume |
| Council proxy | Apache-2.0 (Sablecourt), BSD-style (Go) | The proxy compiled from source, with `/licenses/sablecourt/LICENSE` and `/licenses/go/LICENSE` | Compiled locally by `sablectl harness build` |

The base in every image is the digest-pinned Debian `bookworm-slim` with packages from the snapshot.debian.org archive of 2026-11-23; each package's copyright file stays under `/usr/share/doc`. Every image has a `/licenses/README` that lists what it contains, and three OCI labels:

- `org.opencontainers.image.description`: for `codex`, "Contains the OpenAI Codex CLI 0.172.0, unmodified."; for `claude` and `agy`, "Contains no vendor software; the tool is installed at login."
- `org.opencontainers.image.licenses`: `Apache-2.0` for `codex`, `Apache-2.0 AND BSD-3-Clause` for the proxy, `NOASSERTION` for `claude` and `agy`.
- `org.opencontainers.image.source`: the Sablecourt repository.

Why: Apache-2.0 lets the Codex CLI be passed on with its licence and notice. Claude Code ("All rights reserved") and the Antigravity CLI (Google's terms forbid copying or distributing the software) give no such permission, so an image meant for a registry carries neither, and each user downloads the tool from the vendor.

## Install the tool at login

```sh
sablectl harness build claude      # or codex, agy
sablectl harness login claude
```

For `claude` and `agy`, `sablectl harness login` first installs the pinned tool when it is missing, of another version, or does not match its digest, then starts the login. `sablectl harness install <role>` runs only the install step, for example after an upgrade changes the pinned version. `codex` needs no install: `sablectl harness install codex` refuses with "the codex image carries its tool".

The install:

1. Downloads one file over HTTPS from the vendor's host, on your machine, with no cookies, no credentials and no redirect followed. The URL is fixed in `internal/harnessimages/images.go`; only `downloads.claude.ai` and `storage.googleapis.com` are accepted. `storage.googleapis.com` serves every public Google Cloud Storage bucket, so the host check alone does not tie the download to Google: the constant URL and the checksum in step 2 do. The download honours `HTTPS_PROXY` so that it works behind a corporate proxy; TLS still runs end to end to the vendor's host, and the checksum holds whatever the proxy does.
   - `claude`: `https://downloads.claude.ai/claude-code-releases/2.1.332/linux-arm64/claude`, the binary itself.
   - `agy`: `https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.17-4823843970351104/linux-arm/cli_linux_arm64.tar.gz`.
2. Checks it on your machine before Docker sees it. `claude`: SHA-256 and size of the binary. `agy`: SHA-512 and size of the archive, then SHA-256 and size of the one member `antigravity`. `sablectl` reads at most one byte more than the pinned size, so a server that sends more cannot make it write much more than the artefact; that extra byte counts as a mismatch.
3. Creates the volume `sablecourt-harness-<role>-tool` with the labels `app.kubernetes.io/managed-by=sablecourt` and `sablecourt.io/harness-tool=<role>` if it does not exist. It refuses a volume whose labels are not exactly these two, and a volume with another driver or with driver options. Volume labels cannot change after creation and Docker adds none to a volume created through its API, so an extra label means something other than Sablecourt created the volume.
4. Streams the binary into a short install container from the harness image: user `10001:10001`, no network, all capabilities dropped, `no-new-privileges`, read-only root filesystem, only the base environment (`PATH`, `HOME`, `TMPDIR`), and one mount, the tool volume at `/opt/harness-tool`. The login volume is not mounted. The container checks the SHA-256 again, then renames the file into place as mode 0555 and writes `VERSION`.

What is verified for `claude`: the bytes equal the SHA-256 and size that Anthropic's release manifest for 2.1.332 listed on 2026-11-26, which is pinned in the code. What is not verified: the release manifest's own signature (`manifest.json.sig`), because Sablecourt does not use the manifest at install time. Sablecourt does not run Anthropic's `install.sh`: that script fetches the latest release, runs it, and lets it install the requested version, so code that is not the pinned version would run on your machine.

For `codex`, the image build checks the release archive against the SHA-256 that GitHub listed for the asset of `rust-v0.172.0` on 2026-11-26, pinned in the Dockerfile and in `images.go`. Only `linux/arm64` is pinned; a build for another target architecture fails. The Sigstore bundle that OpenAI publishes next to the asset is not verified.

The tool never updates itself in the container. The tool volume is read-only there, and `claude` runs with `DISABLE_UPDATES=1`, the variable Claude Code documents to block both background and manual updates. A new version of a tool arrives only as a new pin in a Sablecourt release; `sablectl harness install <role>` then installs it.

## What a turn checks

The tool volume is mounted read-only (and with `volume-nocopy`) at `/opt/harness-tool` in the login container and in every turn. Before the tool starts, a guard in the container checks that `/opt/harness-tool/<tool>` is a regular executable file and not a symlink, that `VERSION` names the pinned version, and that the file's SHA-256 is the pinned one. A failure stops the turn with exit status 125 and a message such as `council tool claude is not the pinned version 2.1.332: run sablectl harness install claude`. The guard runs after the lease and question are read from standard input and before the lease file is written.

Before a council is created, the daemon's preflight runs the same check in its probe container and refuses the request with the install command:

| Case | HTTP status | Message names |
| --- | --- | --- |
| Tool volume or tool missing | 412 | `sablectl harness install <role>` |
| Another version installed | 412 | `sablectl harness install <role>` |
| SHA-256 differs from the pin | 412 | `sablectl harness install <role>` |
| Tool volume has other labels, driver or options | 409 | remove it by hand, then `sablectl harness install <role>` |

## Fully local build

`sablectl harness build <role>` builds the same images on your machine and never pushes them. For `claude` and `agy` it builds the image without the tool, and `sablectl harness login` installs the tool as above. There is one path for the tool whether the image was built locally or pulled, so the runtime check is the same in both cases.

## Antigravity terms

Google's terms for the Antigravity CLI restrict third-party tools that access the service. Running `agy` as a council seat through Sablecourt is such a use, however the CLI is packaged. Whether to use that seat is your decision under your own agreement with Google.

## In depth

- `internal/harnessimages/Dockerfile`, `internal/harnessimages/images.go`, `internal/harnessimages/tool.go`, `internal/harnessimages/licenses/`.
- `internal/runtime/docker/council_tool.go` (install container and guard), `internal/runtime/docker/council_preflight.go`, `cmd/sablectl/harness_install.go`.
- [Council harness images and logins](../council-harnesses.md), [harness containers](../how-it-works/harness-containers.md), [run a council](run-a-council.md).
