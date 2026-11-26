#!/usr/bin/env bash
# Table tests of hack/release-latest.sh: which release may move `latest`.
# Usage: hack/test/release-latest_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/release-latest.sh"
fails=0

# <name> <version> <tags, space-separated> <wanted exit status> <wanted stdout>
case_() {
  local name="$1" version="$2" tags="$3" want_rc="$4" want_out="$5" out rc=0
  out="$(printf '%s' "${tags}" | tr ' ' '\n' | "${script}" "${version}" 2> /dev/null)" || rc=$?
  if [ "${rc}" = "${want_rc}" ] && [ "${out}" = "${want_out}" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got exit ${rc} and '${out}', want exit ${want_rc} and '${want_out}'"
    fails=$((fails + 1))
  fi
}

case_ "the first release is latest" 0.1.0 "v0.1.0" 0 true
case_ "0.2.0 after 0.1.1 is latest" 0.2.0 "v0.1.0 v0.1.1 v0.2.0" 0 true
case_ "0.1.1 after 0.2.0 is not latest" 0.1.1 "v0.1.0 v0.2.0 v0.1.1" 0 false
case_ "a release candidate is never latest" 0.3.0-rc.1 "v0.2.0 v0.3.0-rc.1" 0 false
case_ "the first release candidate is not latest" 0.1.0-rc.1 "v0.1.0-rc.1" 0 false
case_ "a higher release candidate does not count" 1.0.0 "v0.9.0 v1.0.0 v1.0.1-rc.1 v2.0.0-rc.3" 0 true
case_ "versions compare as numbers, not text" 0.10.0 "v0.9.0 v0.10.0" 0 true
case_ "0.9.0 after 0.10.0 is not latest" 0.9.0 "v0.10.0 v0.9.0" 0 false
case_ "a higher major wins over higher minor and patch" 1.0.0 "v0.99.99 v1.0.0" 0 true
case_ "a higher patch on the same minor blocks it" 1.2.3 "v1.2.3 v1.2.10" 0 false
case_ "tags that are not release versions are ignored" 0.2.0 "v0.2.0 v9.0.0-beta.1 vnext v09.0.0 release-9 9.0.0" 0 true
case_ "the same version twice is still latest" 0.2.0 "v0.2.0 v0.2.0 v0.1.0" 0 true
case_ "a list without the version's own tag fails" 0.2.0 "v0.1.0" 1 ""
case_ "an empty list fails" 0.2.0 "" 1 ""
case_ "a version that is not X.Y.Z or X.Y.Z-rc.N is refused" 0.2 "v0.2" 2 ""
case_ "a leading v is refused" v0.2.0 "v0.2.0" 2 ""

if printf 'v0.2.0\r\nv0.3.0\r\n' | "${script}" 0.2.0 2> /dev/null | grep -qx false; then
  echo "ok   lines ending in CR LF are read"
else
  echo "FAIL lines ending in CR LF are read"
  fails=$((fails + 1))
fi
rc=0
"${script}" > /dev/null 2>&1 < /dev/null || rc=$?
if [ "${rc}" = 2 ]; then echo "ok   a missing argument is a usage error"; else echo "FAIL a missing argument is a usage error"; fails=$((fails + 1)); fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
