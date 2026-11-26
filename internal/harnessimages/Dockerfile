FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e AS base

# A dated package index keeps rebuilds independent of Debian mirror changes.
RUN rm -f /etc/apt/sources.list.d/debian.sources \
    && printf 'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261123T000000Z/ bookworm main\ndeb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261123T000000Z/ bookworm-updates main\ndeb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/20261123T000000Z/ bookworm-security main\n' > /etc/apt/sources.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl git gzip libstdc++6 tar xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 harness \
    && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/harness --shell /bin/bash harness \
    && mkdir -p /workspace /scratch /home/harness/.codex \
    && chown -R 10001:10001 /home/harness /scratch

ENV HOME=/home/harness TMPDIR=/scratch PATH=/usr/local/bin:/usr/bin:/bin
WORKDIR /workspace
LABEL org.opencontainers.image.source="https://github.com/shadewright-labs/sablecourt"

# The images for proprietary tools carry no vendor software. sablectl installs
# the pinned tool at login into a separate volume that is mounted here read-only.
FROM base AS tool-less
RUN install -d -o 10001 -g 10001 -m 0755 /opt/harness-tool
USER 10001:10001
CMD ["/bin/bash", "-c", "cat /licenses/README"]

FROM tool-less AS claude
COPY licenses/image/claude.txt /licenses/README
LABEL org.opencontainers.image.title="sablecourt-harness-claude" \
      org.opencontainers.image.description="Sablecourt council harness base for the claude role. Contains no vendor software; the tool is installed at login." \
      org.opencontainers.image.licenses="NOASSERTION"

FROM tool-less AS agy
COPY licenses/image/agy.txt /licenses/README
LABEL org.opencontainers.image.title="sablecourt-harness-agy" \
      org.opencontainers.image.description="Sablecourt council harness base for the agy role. Contains no vendor software; the tool is installed at login." \
      org.opencontainers.image.licenses="NOASSERTION"

FROM base AS codex
ARG TARGETARCH
# The Codex CLI release archive is pinned by SHA-256 per architecture, with the
# same values as the codex entry in images.go. The arm64 digest is the one
# GitHub lists for the asset codex-aarch64-unknown-linux-musl.tar.gz of tag
# rust-v0.172.0, read from the release API on 2026-11-26 and checked against a
# download. No other architecture is pinned, so any other target fails here.
RUN set -eu; \
    case "${TARGETARCH}" in \
      arm64) codex_target=aarch64-unknown-linux-musl codex_sha256=0b99526c185a4425fe96a2214fb7b6a6a7f7c1471883b7afbebe97e15e83e79e ;; \
      *) echo "no Codex CLI is pinned for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --proto '=https' "https://github.com/openai/codex/releases/download/rust-v0.172.0/codex-${codex_target}.tar.gz" -o /scratch/codex.tar.gz; \
    printf '%s  %s\n' "${codex_sha256}" /scratch/codex.tar.gz | sha256sum --check --strict --quiet \
      || { echo 'the Codex CLI download does not match its pinned SHA-256' >&2; exit 1; }; \
    mkdir -p /opt/codex; \
    tar -xzf /scratch/codex.tar.gz -C /opt/codex; \
    install -m 0755 "/opt/codex/codex-${codex_target}" /usr/local/bin/codex; \
    rm -rf /scratch/codex.tar.gz /opt/codex
COPY licenses/codex/LICENSE licenses/codex/NOTICE /licenses/codex/
COPY licenses/image/codex.txt /licenses/README
LABEL org.opencontainers.image.title="sablecourt-harness-codex" \
      org.opencontainers.image.description="Sablecourt council harness for the codex role. Contains the OpenAI Codex CLI 0.172.0, unmodified." \
      org.opencontainers.image.licenses="Apache-2.0"
USER 10001:10001
RUN test "$(codex --version)" = 'codex-cli 0.172.0'
CMD ["codex", "--version"]

FROM base AS proxy
COPY council-proxy /usr/local/bin/council-proxy
COPY licenses/sablecourt/LICENSE /licenses/sablecourt/LICENSE
COPY licenses/go/LICENSE /licenses/go/LICENSE
COPY licenses/image/proxy.txt /licenses/README
LABEL org.opencontainers.image.title="sablecourt-harness-proxy" \
      org.opencontainers.image.description="Sablecourt council proxy, built from source. Contains no vendor software." \
      org.opencontainers.image.licenses="Apache-2.0 AND BSD-3-Clause"
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/council-proxy"]
