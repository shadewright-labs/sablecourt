#!/usr/bin/env bash
# Copies the failing inputs a fuzz run left behind into <destination>, keeping their paths, for the artifact of
# .github/workflows/fuzz.yml. go test writes a failing input to testdata/fuzz/<FuzzName>/ in the package, so
# the files taken are the untracked ones under testdata/fuzz/ of any package, the module root included.
# Run from the module root. Usage: hack/fuzz-failures.sh <destination>
# Tests: hack/test/fuzz-targets_test.sh.
set -euo pipefail

dest="${1:?usage: hack/fuzz-failures.sh <destination>}"
mkdir -p "${dest}"
git ls-files -z --others --exclude-standard -- 'testdata/fuzz/*' '*/testdata/fuzz/*' |
  while IFS= read -r -d '' f; do
    mkdir -p "${dest}/$(dirname "${f}")"
    cp "${f}" "${dest}/${f}"
  done
