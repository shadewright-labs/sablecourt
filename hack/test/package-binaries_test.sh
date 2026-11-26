#!/usr/bin/env bash
# Tests of hack/package-binaries.sh (needs go and python3): two builds of the same commit give the same bytes, the
# archive entries are fixed (names, owner, mode, time), sablectl prints the stamped version, and bad input is
# refused. It builds for the host only (RELEASE_TARGETS), so the built sablectl can run.
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
host="$(go env GOOS)/$(go env GOARCH)"
name="sablecourt_9.8.7-rc.6_$(go env GOOS)_$(go env GOARCH).tar.gz"
export RELEASE_TARGETS="${host}" SOURCE_DATE_EPOCH=1700000000
"${script}" 9.8.7-rc.6 "${work}/a" > /dev/null
"${script}" 9.8.7-rc.6 "${work}/b" > /dev/null

check "one archive per target, named sablecourt_<version>_<os>_<arch>.tar.gz" \
  "$([ "$(ls "${work}/a/dist")" = "${name}" ] && echo ok)"
check "two builds give the same archive bytes" \
  "$(cmp -s "${work}/a/dist/${name}" "${work}/b/dist/${name}" && echo ok)"
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
  "$([ -x "${work}/a/bin/$(go env GOOS)_$(go env GOARCH)/sablecourtd" ] && echo ok)"
check "sablectl prints the stamped version" \
  "$("${work}/a/bin/$(go env GOOS)_$(go env GOARCH)/sablectl" version 2>&1 | grep -qF 9.8.7-rc.6 && echo ok)"
check "the default targets are darwin and linux on amd64 and arm64" \
  "$(grep -qF 'RELEASE_TARGETS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64}' "${script}" && echo ok)"

expect() { # <name> <wanted exit status> <command...>
  local want="$2" got=0
  shift 2
  "$@" > /dev/null 2>&1 || got=$?
  [ "${got}" = "${want}" ] && echo ok
}
check "refuses a version that is not X.Y.Z or X.Y.Z-rc.N" "$(expect v 2 "${script}" 1.2.3-beta.1 "${work}/c")"
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
