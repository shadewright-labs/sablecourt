#!/usr/bin/env bash
# Tests of hack/fuzz-targets.sh on fixture trees. Usage: hack/test/fuzz-targets_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0

mkdir -p "${work}/t/internal/a" "${work}/t/cmd/b" "${work}/t/internal/c"
printf 'package a\nfunc FuzzOne(f *testing.F) {}\n' > "${work}/t/internal/a/a_test.go"
printf 'package b\nfunc FuzzTwo(input *testing.F) {}\nfunc FuzzThree( t  *testing.F ) {}\n' > "${work}/t/cmd/b/b_test.go"
printf 'package c\nfunc FuzzNotATest(f *testing.F) {}\n' > "${work}/t/internal/c/c.go"
printf 'package c\nfunc helperFuzz(f *testing.F) {}\n// func FuzzCommented(f *testing.F) {}\n' > "${work}/t/internal/c/c_test.go"

got="$("${root}/hack/fuzz-targets.sh" "${work}/t")"
want='["cmd/b:FuzzThree","cmd/b:FuzzTwo","internal/a:FuzzOne"]'
if [ "${got}" = "${want}" ]; then echo "ok   finds targets whatever the parameter name"; else echo "FAIL targets: got ${got}, want ${want}"; fails=$((fails + 1)); fi

mkdir -p "${work}/empty/internal"
if "${root}/hack/fuzz-targets.sh" "${work}/empty" > /dev/null 2>&1; then echo "FAIL empty tree passed"; fails=$((fails + 1)); else echo "ok   empty tree fails"; fi

got="$("${root}/hack/fuzz-targets.sh" "${root}")"
case "${got}" in
  *'"internal/gate/model:FuzzSC05_JoinUpstreamURLPreservesHostAndScheme"'*) echo "ok   finds the SC-05 target in this repository" ;;
  *) echo "FAIL repository targets: ${got}"; fails=$((fails + 1)) ;;
esac

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
