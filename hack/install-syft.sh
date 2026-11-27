#!/usr/bin/env bash
# Installs the pinned syft release from the official GitHub release, checked against a sha256 kept in this file.
# Used by release.yml to write the SPDX SBOMs of the binaries and of the images. anchore/sbom-action is not used:
# it downloads and runs syft's install script with no checksum kept here. To bump: take the new version, read the
# sha256 of syft_<version>_linux_amd64.tar.gz from the release's syft_<version>_checksums.txt, and change both
# values together.
# Usage: hack/install-syft.sh <target dir>    (the binary is <target dir>/syft; linux amd64 only)
set -euo pipefail

SYFT_VERSION=1.57.1
SYFT_SHA256=bf52519eb57f675d47c07d43213cb6dff6449f3d6d1147ff6701ed91f44eef0c

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <target dir>" >&2
  exit 2
fi
target="$1"
archive="syft_${SYFT_VERSION}_linux_amd64.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# Retries ride out short outages of the release CDN.
curl -fsSL --proto '=https' --connect-timeout 20 --max-time 120 --retry 4 --retry-delay 10 \
  -o "${work}/${archive}" "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/${archive}"
echo "${SYFT_SHA256}  ${work}/${archive}" | sha256sum --check --strict
mkdir -p "${target}"
tar -xzf "${work}/${archive}" -C "${target}" syft
"${target}/syft" version
