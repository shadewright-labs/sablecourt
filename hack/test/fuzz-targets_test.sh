#!/usr/bin/env bash
# Tests of hack/fuzz-targets.sh on fixture modules (needs go). Usage: hack/test/fuzz-targets_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0

t="${work}/t"
mkdir -p "${t}/internal/a" "${t}/cmd/b" "${t}/internal/c" "${t}/internal/d" "${t}/internal/e"
printf 'module example.invalid/fz\n\ngo 1.22\n' > "${t}/go.mod"
printf 'package fz\n\nimport "testing"\n\nfunc FuzzRoot(f *testing.F) { f.Fuzz(func(*testing.T, int) {}) }\n' > "${t}/root_test.go"
printf 'package a\n\nimport "testing"\n\nfunc FuzzOne(f *testing.F) {}\n' > "${t}/internal/a/a_test.go"
# Go identifiers may hold any Unicode letter.
printf 'package a\n\nimport "testing"\n\nfunc Fuzz\305\201\303\263d\305\272(f *testing.F) {}\n' > "${t}/internal/a/u_test.go"
cat > "${t}/cmd/b/b_test.go" <<'GO'
package b

import "testing"

func FuzzTwo(input *testing.F) {}

func FuzzThree( t  *testing.F ) {}

// An unnamed parameter.
func FuzzUnnamed(*testing.F) {}

// A signature over several lines.
func FuzzMultiLine(
	f *testing.F,
) {
}
GO
cat > "${t}/internal/d/d_test.go" <<'GO'
package d

import tt "testing"

// testing under an alias.
func FuzzAliased(f *tt.F) {}
GO
cat > "${t}/internal/e/e_test.go" <<'GO'
package e_test

import (
	. "testing"
)

// An external test package with a dot import.
func FuzzDotImport(f *F) {}
GO
printf 'package c\n\nimport "testing"\n\nfunc FuzzNotATest(f *testing.F) {}\n' > "${t}/internal/c/c.go"
printf 'package c\n\nimport "testing"\n\nfunc helperFuzz(f *testing.F) {}\n\n// func FuzzCommented(f *testing.F) {}\n' > "${t}/internal/c/c_test.go"

got="$("${root}/hack/fuzz-targets.sh" "${t}")" || got="exit $?"
want="$(printf '%s' '[".:FuzzRoot","cmd/b:FuzzMultiLine","cmd/b:FuzzThree","cmd/b:FuzzTwo","cmd/b:FuzzUnnamed","internal/a:FuzzOne","internal/a:Fuzz\u0141\u00f3d\u017a","internal/d:FuzzAliased","internal/e:FuzzDotImport"]' | jq -c .)"
if [ "${got}" = "${want}" ]; then echo "ok   finds every target go test finds"; else echo "FAIL targets: got ${got}, want ${want}"; fails=$((fails + 1)); fi

mkdir -p "${work}/empty/internal/x"
printf 'module example.invalid/empty\n\ngo 1.22\n' > "${work}/empty/go.mod"
printf 'package x\n' > "${work}/empty/internal/x/x.go"
if "${root}/hack/fuzz-targets.sh" "${work}/empty" > /dev/null 2>&1; then echo "FAIL a module without targets passed"; fails=$((fails + 1)); else echo "ok   a module without targets fails"; fi

cp -R "${t}" "${work}/broken"
printf 'package a\n\nfunc broken( {\n' > "${work}/broken/internal/a/broken.go"
if "${root}/hack/fuzz-targets.sh" "${work}/broken" > /dev/null 2>&1; then echo "FAIL a package that does not build passed"; fails=$((fails + 1)); else echo "ok   a package that does not build fails"; fi

got="$("${root}/hack/fuzz-targets.sh" "${root}")" || got="exit $?"
case "${got}" in
  *'"internal/gate/model:FuzzSC05_JoinUpstreamURLPreservesHostAndScheme"'*) echo "ok   finds the SC-05 target in this repository" ;;
  *) echo "FAIL repository targets: ${got}"; fails=$((fails + 1)) ;;
esac

# hack/fuzz-failures.sh copies the new files under testdata/fuzz/ of any package, the module root included.
c="${work}/collect"
mkdir -p "${c}/testdata/fuzz/FuzzRoot" "${c}/internal/a/testdata/fuzz/FuzzOne" "${c}/internal/b/testdata/fuzz/FuzzOld"
printf 'go test fuzz v1\n' | tee "${c}/testdata/fuzz/FuzzRoot/r1" "${c}/internal/a/testdata/fuzz/FuzzOne/a1" \
  "${c}/internal/b/testdata/fuzz/FuzzOld/seed" "${c}/other.txt" > /dev/null
git -C "${c}" init -q && git -C "${c}" add internal/b && git -C "${c}" -c user.name=t -c user.email=t@example.invalid commit -qm seed
(cd "${c}" && "${root}/hack/fuzz-failures.sh" out) > /dev/null 2>&1 || true
got="$(cd "${c}/out" 2> /dev/null && find . -type f | LC_ALL=C sort | tr '\n' ' ')" || got="no output"
want="./internal/a/testdata/fuzz/FuzzOne/a1 ./testdata/fuzz/FuzzRoot/r1 "
if [ "${got}" = "${want}" ]; then echo "ok   collects new failing inputs, root package included"; else echo "FAIL collected: got '${got}', want '${want}'"; fails=$((fails + 1)); fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
