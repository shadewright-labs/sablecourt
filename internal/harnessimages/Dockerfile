FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e AS base

# A dated package index keeps rebuilds independent of Debian mirror changes.
RUN rm -f /etc/apt/sources.list.d/debian.sources \
    && printf 'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20250224T000000Z/ bookworm main\n' > /etc/apt/sources.list \
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
RUN curl -fsSL https://antigravity.google/cli/install.sh -o /scratch/install.sh \
    && HOME=/root bash /scratch/install.sh --version 1.5.16 --dir /usr/local/bin \
    && chmod 0755 /usr/local/bin/agy \
    && rm -rf /root/.cache/antigravity /scratch/install.sh
USER 10001:10001
RUN test "$(agy --version)" = '1.5.16'
CMD ["agy", "--version"]

FROM base AS proxy
COPY council-proxy /usr/local/bin/council-proxy
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/council-proxy"]
