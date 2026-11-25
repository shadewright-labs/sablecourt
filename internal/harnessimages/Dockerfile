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

FROM base AS claude
RUN curl -fsSL https://claude.ai/install.sh -o /scratch/install.sh \
    && HOME=/opt/claude-install bash /scratch/install.sh 2.1.332 \
    && install -m 0755 "$(readlink -f /opt/claude-install/.local/bin/claude)" /usr/local/bin/claude \
    && rm -rf /opt/claude-install /scratch/install.sh
USER 10001:10001
RUN test "$(claude --version)" = '2.1.332 (Claude Code)'
CMD ["claude", "--version"]

FROM base AS codex
RUN mkdir -p /opt/codex \
    && curl -fsSL https://github.com/openai/codex/releases/download/rust-v0.172.0/codex-aarch64-unknown-linux-musl.tar.gz -o /scratch/codex.tar.gz \
    && tar -xzf /scratch/codex.tar.gz -C /opt/codex \
    && install -m 0755 /opt/codex/codex-aarch64-unknown-linux-musl /usr/local/bin/codex \
    && rm /scratch/codex.tar.gz
USER 10001:10001
RUN test "$(codex --version)" = 'codex-cli 0.172.0'
CMD ["codex", "--version"]

FROM base AS agy
RUN curl -fsSL https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.17-4823843970351104/linux-arm/cli_linux_arm64.tar.gz -o /scratch/agy.tar.gz \
    && echo 'e4f086d0730b325cfd438ea90396ee78eb23a3110ef7f792824c8817b8d0432462b7b5f91b3d933fc1932a6f4ba73de505e2e96f44448c9fb2427292ead716f7  /scratch/agy.tar.gz' | sha512sum -c - \
    && tar -xzf /scratch/agy.tar.gz -C /scratch antigravity \
    && install -m 0755 /scratch/antigravity /usr/local/bin/agy \
    && rm -f /scratch/agy.tar.gz /scratch/antigravity
USER 10001:10001
RUN test "$(agy --version)" = '1.5.17'
CMD ["agy", "--version"]

FROM base AS proxy
COPY council-proxy /usr/local/bin/council-proxy
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/council-proxy"]
