#!/usr/bin/env bash
# Builds sablecourtd and sablectl for the release targets and packs one archive per target, for release.yml:
#   <out dir>/dist/sablecourt_<version>_<os>_<arch>.tar.gz   (sablecourtd and sablectl at the archive root)
#   <out dir>/bin/<os>_<arch>/                               (the same binaries, unpacked, for the SBOM)
# The inputs are fixed so that two runs on the same commit with the same Go toolchain give the same bytes: CGO off,
# -trimpath, -buildvcs=false (so an untracked or changed file in the tree does not change the binaries), the version
# stamped into sablectl (main.version), and archives packed by hack/targz.go with the same Go toolchain: sorted
# entries, owner 0:0, mode 0755, the time SOURCE_DATE_EPOCH (by default the commit time of HEAD) and no name in the
# gzip header. No other compressor is used, so the bytes do not depend on a zlib build. Run it from the repository
# root.
# Usage: hack/package-binaries.sh <version> <out dir>
#   version is X.Y.Z or X.Y.Z-rc.N; RELEASE_TARGETS (space-separated os/arch) replaces the four targets, for tests.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <version> <out dir>" >&2
  exit 2
fi
version="$1" out="$2"
root="$(cd "$(dirname "$0")/.." && pwd)"
if ! printf '%s' "${version}" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$'; then
  echo "version '${version}' is not X.Y.Z or X.Y.Z-rc.N" >&2
  exit 2
fi
if [ -e "${out}" ] && [ -n "$(ls -A "${out}")" ]; then
  echo "${out} is not empty" >&2
  exit 1
fi
epoch="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct HEAD)}"
case "${epoch}" in '' | *[!0-9]*) echo "SOURCE_DATE_EPOCH '${epoch}' is not a number of seconds" >&2; exit 1 ;; esac

mkdir -p "${out}/dist" "${out}/bin"
for target in ${RELEASE_TARGETS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64}; do
  goos="${target%/*}" goarch="${target#*/}"
  bin="${out}/bin/${goos}_${goarch}"
  mkdir -p "${bin}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o "${bin}/sablecourtd" ./cmd/sablecourtd
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=${version}" -o "${bin}/sablectl" ./cmd/sablectl
  go run "${root}/hack/targz.go" "${bin}" "${out}/dist/sablecourt_${version}_${goos}_${goarch}.tar.gz" "${epoch}"
done
echo "$(go env GOVERSION): $(cd "${out}/dist" && ls)"
