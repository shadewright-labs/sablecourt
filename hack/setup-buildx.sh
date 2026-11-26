#!/usr/bin/env bash
# Installs the pinned Buildx release as the docker CLI plugin of this user, checked against a sha256 kept in this
# file, and creates and selects a docker-container builder that runs the BuildKit image pinned by digest. Used by
# images.yml and images-build.yml before docker/build-push-action (which builds with the selected builder), in
# place of docker/setup-buildx-action, so neither the Buildx binary nor the BuildKit image comes from a download
# without a checksum in this repository. To bump Buildx: take the new version, read the sha256 of
# buildx-v<version>.linux-amd64 from the release's checksums.txt, and change both values together. To bump
# BuildKit: `docker buildx imagetools inspect moby/buildkit:v<version>` prints the index digest.
# Usage: hack/setup-buildx.sh [--plugin-only]    (linux amd64 only; --plugin-only installs Buildx and creates no
# builder, for jobs that only run `docker buildx imagetools`)
set -euo pipefail

BUILDX_VERSION=0.39.0
BUILDX_SHA256=5fb31a097304fbf6dd4038649a0decf32c2b9510e4047acd7d68b9d700c46194
BUILDKIT_IMAGE=moby/buildkit:v0.35.0@sha256:aa632b921ef07411d2cb77292cd34213d4f7b384001d6fceef1e1f25bf9752bb

if [ "$#" -gt 1 ] || { [ "$#" -eq 1 ] && [ "$1" != --plugin-only ]; }; then
  echo "usage: $0 [--plugin-only]" >&2
  exit 2
fi
binary="buildx-v${BUILDX_VERSION}.linux-amd64"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

curl -fsSL --proto '=https' --connect-timeout 20 --max-time 120 --retry 4 --retry-delay 10 \
  -o "${work}/${binary}" "https://github.com/docker/buildx/releases/download/v${BUILDX_VERSION}/${binary}"
echo "${BUILDX_SHA256}  ${work}/${binary}" | sha256sum --check --strict
# The user's plugin directory comes before the system ones, so this binary is the docker buildx that runs.
plugins="${HOME}/.docker/cli-plugins"
mkdir -p "${plugins}"
install -m 0755 "${work}/${binary}" "${plugins}/docker-buildx"
docker buildx version | grep -F " v${BUILDX_VERSION} " || { echo "docker buildx is not v${BUILDX_VERSION}" >&2; exit 1; }

if [ "${1:-}" = --plugin-only ]; then
  exit 0
fi
docker buildx create --name sablecourt --driver docker-container --driver-opt "image=${BUILDKIT_IMAGE}" --use
docker buildx inspect --bootstrap
