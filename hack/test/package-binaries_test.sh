#!/usr/bin/env bash
# Tests of hack/package-binaries.sh (needs go, git and python3 for the checks): two builds of the same commit give
# the same bytes, a clean copy of the tree and the same tree with an untracked file give the same bytes, packing
# needs no python3 (the archive bytes may not depend on a zlib build), the archive entries are fixed (names,
# owner, mode, time), sablectl prints exactly the stamped version, packing works with GOOS and GOARCH set to another
# platform, and bad input is refused. It builds for the host only (RELEASE_TARGETS, from GOHOSTOS and GOHOSTARCH),
# so the built sablectl can run.
# Usage: hack/test/package-binaries_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/package-binaries.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0

check() {
  if [ "$2" = ok ]; then echo "ok   $1"; else echo "FAIL $1"; fails=$((fails + 1)); fi
}

cd "${root}"
hostos="$(go env GOHOSTOS)"
hostarch="$(go env GOHOSTARCH)"
host="${hostos}/${hostarch}"
name="sablecourt_9.8.7-rc.6_${hostos}_${hostarch}.tar.gz"
export RELEASE_TARGETS="${host}" SOURCE_DATE_EPOCH=1700000000
"${script}" 9.8.7-rc.6 "${work}/a" > /dev/null
"${script}" 9.8.7-rc.6 "${work}/b" > /dev/null

check "one archive per target, named sablecourt_<version>_<os>_<arch>.tar.gz" \
  "$([ "$(ls "${work}/a/dist")" = "${name}" ] && echo ok)"
check "two builds give the same archive bytes" \
  "$(cmp -s "${work}/a/dist/${name}" "${work}/b/dist/${name}" && echo ok)"
# A clean copy of the tree (tracked files, plus the two scripts under test) in a new Git repository, built once
# clean and once with an untracked file: the binaries must not record the state of the tree.
copy="${work}/copy"
mkdir -p "${copy}"
git ls-files -z | while IFS= read -r -d '' f; do
  if [ -f "${f}" ]; then mkdir -p "${copy}/$(dirname "${f}")" && cp -p "${f}" "${copy}/${f}"; fi
done
cp -p hack/package-binaries.sh hack/targz.go "${copy}/hack/"
git -C "${copy}" init -q
git -C "${copy}" add -A
git -C "${copy}" -c user.name=test -c user.email=test -c commit.gpgsign=false commit -qm copy
(cd "${copy}" && hack/package-binaries.sh 9.8.7-rc.6 "${work}/clean" > /dev/null)
printf 'not tracked\n' > "${copy}/untracked.txt"
(cd "${copy}" && hack/package-binaries.sh 9.8.7-rc.6 "${work}/dirty" > /dev/null)
check "a clean tree and the same tree with an untracked file give the same archive bytes" \
  "$(cmp -s "${work}/clean/dist/${name}" "${work}/dirty/dist/${name}" && echo ok)"
check "a copy of the tree gives the same archive bytes as the checkout" \
  "$(cmp -s "${work}/a/dist/${name}" "${work}/clean/dist/${name}" && echo ok)"

# Packing must not call python3: a python3 that fails comes first in PATH.
mkdir -p "${work}/nopy"
printf '#!/bin/sh\necho "python3 must not be used" >&2\nexit 1\n' > "${work}/nopy/python3"
chmod +x "${work}/nopy/python3"
check "packing works without python3 and gives the same bytes" \
  "$(PATH="${work}/nopy:${PATH}" "${script}" 9.8.7-rc.6 "${work}/nopy-out" > /dev/null 2>&1 \
     && cmp -s "${work}/a/dist/${name}" "${work}/nopy-out/dist/${name}" && echo ok)"

# GOOS and GOARCH of another platform in the environment: the script sets them for each target build, and the
# archive packer must still run on the host.
case "${hostos}" in linux) other=darwin ;; *) other=linux ;; esac
check "packing works with GOOS and GOARCH of another platform set and gives the same bytes" \
  "$(GOOS="${other}" GOARCH=amd64 "${script}" 9.8.7-rc.6 "${work}/cross" > /dev/null 2>&1 \
     && cmp -s "${work}/a/dist/${name}" "${work}/cross/dist/${name}" && echo ok)"

check "archive entries: sorted names, owner 0:0, mode 0755, the fixed time, no name or time in gzip" "$(
  python3 -I - "${work}/a/dist/${name}" <<'PY'
import sys, tarfile
path = sys.argv[1]
raw = open(path, "rb").read()
ok = int.from_bytes(raw[4:8], "little") == 1700000000 and not raw[3] & 0x08
with tarfile.open(path) as tar:
    members = tar.getmembers()
ok = ok and [m.name for m in members] == ["sablecourtd", "sablectl"] and all(
    m.uid == 0 and m.gid == 0 and m.uname == "" and m.gname == "" and m.mode == 0o755 and m.mtime == 1700000000
    for m in members)
ok = ok and sorted(m.name for m in members) == [m.name for m in members]
print("ok" if ok else "bad")
PY
)"
check "the unpacked binaries are kept for the SBOM" \
  "$([ -x "${work}/a/bin/${hostos}_${hostarch}/sablecourtd" ] && echo ok)"
# As release.yml checks it: exit 0, standard output exactly "sablectl <version>" and one newline, standard error empty.
printf 'sablectl 9.8.7-rc.6\n' > "${work}/version.want"
check "sablectl prints exactly the stamped version and nothing on standard error" \
  "$("${work}/a/bin/${hostos}_${hostarch}/sablectl" version > "${work}/version.out" 2> "${work}/version.err" \
     && cmp -s "${work}/version.want" "${work}/version.out" && [ ! -s "${work}/version.err" ] && echo ok)"
check "the default targets are darwin and linux on amd64 and arm64" \
  "$(grep -qF 'RELEASE_TARGETS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64}' "${script}" && echo ok)"

expect() { # <name> <wanted exit status> <command...>
  local want="$2" got=0
  shift 2
  "$@" > /dev/null 2>&1 || got=$?
  [ "${got}" = "${want}" ] && echo ok
}
check "refuses a version that is not X.Y.Z or X.Y.Z-rc.N" "$(expect v 2 "${script}" 1.2.3-beta.1 "${work}/c")"
check "refuses a version with a second line" "$(expect v 2 "${script}" $'1.2.3\njunk' "${work}/c")"
check "refuses a leading v" "$(expect v 2 "${script}" v1.2.3 "${work}/c")"
check "refuses an output directory that is not empty" "$(expect d 1 "${script}" 1.2.3 "${work}/a")"
check "refuses a SOURCE_DATE_EPOCH that is not a number" \
  "$(SOURCE_DATE_EPOCH=yesterday expect e 1 "${script}" 1.2.3 "${work}/e")"
check "refuses a missing argument" "$(expect u 2 "${script}" 1.2.3)"

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
