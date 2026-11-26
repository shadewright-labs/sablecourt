#!/usr/bin/env bash
# Builds sablecourtd and sablectl for the release targets and packs one archive per target, for release.yml:
#   <out dir>/dist/sablecourt_<version>_<os>_<arch>.tar.gz   (sablecourtd and sablectl at the archive root)
#   <out dir>/bin/<os>_<arch>/                               (the same binaries, unpacked, for the SBOM)
# The inputs are fixed so that two runs on the same commit with the same Go version give the same bytes: CGO off,
# -trimpath, the version stamped into sablectl (main.version), and archives with sorted entries, owner 0:0, mode
# 0755, the time SOURCE_DATE_EPOCH (by default the commit time of HEAD) and no name or time in the gzip header.
# Usage: hack/package-binaries.sh <version> <out dir>
#   version is X.Y.Z or X.Y.Z-rc.N; RELEASE_TARGETS (space-separated os/arch) replaces the four targets, for tests.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <version> <out dir>" >&2
  exit 2
fi
version="$1" out="$2"
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
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -ldflags="-s -w" \
    -o "${bin}/sablecourtd" ./cmd/sablecourtd
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -ldflags="-s -w -X main.version=${version}" \
    -o "${bin}/sablectl" ./cmd/sablectl
  python3 -I - "${bin}" "${out}/dist/sablecourt_${version}_${goos}_${goarch}.tar.gz" "${epoch}" <<'PY'
import gzip, os, sys, tarfile

src, dest, epoch = sys.argv[1], sys.argv[2], int(sys.argv[3])
with open(dest, "wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as gz, \
        tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as tar:
    for name in sorted(os.listdir(src)):
        info = tar.gettarinfo(os.path.join(src, name), arcname=name)
        info.uid = info.gid = 0
        info.uname = info.gname = ""
        info.mode = 0o755
        info.mtime = epoch
        with open(os.path.join(src, name), "rb") as f:
            tar.addfile(info, f)
PY
done
echo "$(go env GOVERSION): $(cd "${out}/dist" && ls)"
