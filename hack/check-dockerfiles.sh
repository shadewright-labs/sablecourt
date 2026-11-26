#!/usr/bin/env bash
# Fails when a tracked Dockerfile runs the go command in a build stage that has not set ENV GOWORK=off first.
# CI builds and tests with GOWORK=off; a Docker build with automatic workspace discovery would read a go.work in
# the build context and could build something CI never checked (or fail where CI passed). Run by
# repo-checks.yml. Usage: hack/check-dockerfiles.sh [repository root]    Tests: hack/test/dockerfiles_test.sh.
set -euo pipefail

cd "${1:-.}"
files="$(git ls-files -- 'Dockerfile' '*/Dockerfile' '*.Dockerfile' 'Dockerfile.*')" || { echo "git ls-files failed" >&2; exit 1; }
bad=0
while IFS= read -r f; do
  [ -n "${f}" ] || continue
  # Per stage (each FROM starts one): remember ENV GOWORK=off, flag a RUN that calls go before it.
  out="$(awk '
    toupper($1) == "FROM" { off = 0; next }
    toupper($1) == "ENV" && $0 ~ /(^|[[:space:]])GOWORK=off([[:space:]]|$)/ { off = 1; next }
    toupper($1) == "RUN" && $0 ~ /(^|[[:space:];&|])go[[:space:]]/ && !off { print FILENAME ":" FNR ": go runs before ENV GOWORK=off in this stage" }
  ' "${f}")" || { echo "cannot read ${f}" >&2; exit 1; }
  if [ -n "${out}" ]; then
    echo "${out}" >&2
    bad=$((bad + 1))
  fi
done <<< "${files}"
if [ "${bad}" -ne 0 ]; then
  echo "${bad} Dockerfile(s) build without GOWORK=off" >&2
  exit 1
fi
echo "Dockerfiles: ok"
