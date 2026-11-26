#!/usr/bin/env bash
# Builds the release archives of sablecourtd and sablectl for darwin and linux, amd64 and arm64, and writes
# checksums.txt over them. Used by the build job of .github/workflows/release.yml. Each archive is
# sablecourt_<version>_<os>_<arch>.tar.gz and holds both binaries, built static (CGO_ENABLED=0) with -trimpath;
# sablectl's version is stamped from the tag. Usage:
#   hack/package-binaries.sh <tag, with v> <output dir>
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <tag> <output dir>" >&2
  exit 2
fi
tag="$1" out="$2"
printf '%s' "${tag}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || { echo "not a release tag: '${tag}'" >&2; exit 1; }
version="${tag#v}"

export GOFLAGS=-mod=readonly GOWORK=off CGO_ENABLED=0
mkdir -p "${out}"
stage="$(mktemp -d)"
trap 'rm -rf "${stage}"' EXIT

for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  goos="${target%/*}" goarch="${target#*/}"
  dir="${stage}/${goos}-${goarch}"
  mkdir -p "${dir}"
  GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -ldflags="-s -w" -o "${dir}/sablecourtd" ./cmd/sablecourtd
  GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -ldflags="-s -w -X main.version=${version}" -o "${dir}/sablectl" ./cmd/sablectl
  tar -C "${dir}" -czf "${out}/sablecourt_${version}_${goos}_${goarch}.tar.gz" sablecourtd sablectl
done
(cd "${out}" && sha256sum sablecourt_"${version}"_*.tar.gz > checksums.txt)
cat "${out}/checksums.txt"
