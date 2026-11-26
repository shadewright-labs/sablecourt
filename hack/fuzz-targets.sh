#!/usr/bin/env bash
# Prints every Go fuzz target of the module as one line of JSON, ["<package dir>:<FuzzName>", ...], for the
# matrix of .github/workflows/fuzz.yml. The list comes from Go's own discovery, `go test -list '^Fuzz' ./...`,
# so whatever the go command fuzzes is listed (an unnamed parameter, a signature over several lines, an import
# alias of testing, build constraints), with no list to keep in step. It compiles the test binaries, so it needs
# the module's Go toolchain. Fails when go test fails (a package that does not build) or finds nothing, so a
# broken search cannot pass by fuzzing nothing. Usage: hack/fuzz-targets.sh [module root]
# Tests: hack/test/fuzz-targets_test.sh.
set -euo pipefail

cd "${1:-.}"
module="$(GOWORK=off go list -m)" || { echo "cannot read the module path" >&2; exit 1; }
out="$(mktemp "${TMPDIR:-/tmp}/fuzz-targets.XXXXXX")"
trap 'rm -f "${out}"' EXIT
if ! GOWORK=off go test -list '^Fuzz' ./... > "${out}"; then
  echo "go test -list failed; no fuzz target list" >&2
  exit 1
fi
# go test prints a package's matching names, then "ok  <import path>  <time>" for that package.
targets=() names=()
while IFS= read -r line; do
  if [[ "${line}" =~ ^Fuzz[A-Za-z0-9_]*$ ]]; then
    names+=("${line}")
  elif [[ "${line}" =~ ^ok[[:space:]]+([^[:space:]]+) ]]; then
    pkg="${BASH_REMATCH[1]}"
    if [ "${pkg}" = "${module}" ]; then
      dir=.
    elif [ "${pkg#"${module}/"}" != "${pkg}" ]; then
      dir="${pkg#"${module}/"}"
    else
      echo "package ${pkg} is outside module ${module}" >&2
      exit 1
    fi
    for name in ${names[@]+"${names[@]}"}; do targets+=("${dir}:${name}"); done
    names=()
  fi
done < "${out}"
if [ "${#names[@]}" -ne 0 ]; then
  echo "fuzz targets with no package line: ${names[*]}" >&2
  exit 1
fi
if [ "${#targets[@]}" -eq 0 ]; then
  echo "no fuzz targets found" >&2
  exit 1
fi
printf '%s\n' "${targets[@]}" | LC_ALL=C sort -u | jq -R . | jq -cs .
