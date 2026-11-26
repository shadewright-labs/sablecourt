#!/usr/bin/env bash
# Prints every Go fuzz target of the repository as one line of JSON, ["<package dir>:<FuzzName>", ...], for the
# matrix of .github/workflows/fuzz.yml. The list is read from the tree: every top-level `func FuzzX(<name>
# *testing.F)` in a _test.go file under cmd/, internal/ and examples/, whatever the parameter is called, so a new
# fuzz test is fuzzed without a list to keep in step. Fails when the search fails or finds nothing, so a broken
# search cannot pass by fuzzing nothing. Usage: hack/fuzz-targets.sh [repository root]
# Tests: hack/test/fuzz-targets_test.sh.
set -euo pipefail

cd "${1:-.}"
dirs=()
for d in cmd internal examples; do
  if [ -d "${d}" ]; then dirs+=("${d}"); fi
done
if [ "${#dirs[@]}" -eq 0 ]; then
  echo "no fuzz targets found: no cmd/, internal/ or examples/ directory" >&2
  exit 1
fi
hits="$(mktemp "${TMPDIR:-/tmp}/fuzz-targets.XXXXXX")"
trap 'rm -f "${hits}"' EXIT
rc=0
grep -rEH --include='*_test.go' \
  '^func Fuzz[A-Za-z0-9_]*\([[:space:]]*[A-Za-z_][A-Za-z0-9_]*[[:space:]]+\*testing\.F[[:space:]]*\)' \
  "${dirs[@]}" > "${hits}" || rc=$?
if [ "${rc}" -gt 1 ]; then
  echo "searching for fuzz targets failed (grep exit ${rc})" >&2
  exit 1
fi
targets=()
while IFS= read -r line; do
  file="${line%%:*}" rest="${line#*:}"
  name="${rest#func }"
  name="${name%%(*}"
  targets+=("$(dirname "${file}"):${name}")
done < "${hits}"
if [ "${#targets[@]}" -eq 0 ]; then
  echo "no fuzz targets found" >&2
  exit 1
fi
printf '%s\n' "${targets[@]}" | LC_ALL=C sort -u | jq -R . | jq -cs .
