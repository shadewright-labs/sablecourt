#!/usr/bin/env bash
# Installs the pinned Syft release from the official GitHub release, checked against a sha256 kept in this file.
# Used by images.yml to make the SPDX SBOM of each platform image. Syft is installed here and not through
# anchore/sbom-action, which on a tool-cache miss downloads Syft's install.sh and runs it, in a job that can
# write packages and mint OIDC tokens. To bump: take the new version, read the sha256 of
# syft_<version>_linux_amd64.tar.gz from the release's syft_<version>_checksums.txt, and change both values
# together. Usage: hack/install-syft.sh <target dir>    (the binary is <target dir>/syft; linux amd64 only)
set -euo pipefail

SYFT_VERSION=1.54.0
SYFT_SHA256=54a87372498168b2d033e876fd41fa4e8035b872699e525a57046e1f2f09c860

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <target dir>" >&2
  exit 2
fi
target="$1"
archive="syft_${SYFT_VERSION}_linux_amd64.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# Retries ride out short outages of the release CDN.
curl -fsSL --proto '=https' --connect-timeout 20 --max-time 60 --retry 4 --retry-delay 10 \
  -o "${work}/${archive}" "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/${archive}"
echo "${SYFT_SHA256}  ${work}/${archive}" | sha256sum --check --strict
mkdir -p "${target}"
tar -xzf "${work}/${archive}" -C "${target}" syft
"${target}/syft" version | grep -E '^Version:'
