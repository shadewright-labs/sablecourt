#!/usr/bin/env bash
# Prints every Go fuzz target of the repository as one line of JSON, ["<package dir>:<FuzzName>", ...], for the
# matrix of .github/workflows/fuzz.yml. The list is read from the tree (every `func FuzzX(f *testing.F)` in a
# _test.go file under cmd/, internal/ and examples/), so a new fuzz test is fuzzed without a list to keep in
# step. Fails when it finds none, so a broken search cannot pass by fuzzing nothing.
# Usage: hack/fuzz-targets.sh [repository root]
set -euo pipefail

cd "${1:-.}"
found="$(grep -rEo --include='*_test.go' '^func Fuzz[A-Za-z0-9_]*\(f \*testing\.F\)' cmd internal examples 2> /dev/null |
  sed -E 's#^(.*)/[^/]+_test\.go:func (Fuzz[A-Za-z0-9_]*)\(.*$#\1:\2#' | sort -u || true)"
if [ -z "${found}" ]; then
  echo "no fuzz targets found" >&2
  exit 1
fi
printf '%s\n' "${found}" | jq -R . | jq -cs .
