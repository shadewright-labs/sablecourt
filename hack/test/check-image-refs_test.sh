#!/usr/bin/env bash
# Tests of hack/check-image-refs.sh on fixture repositories. Usage: hack/test/check-image-refs_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0 n=0

case_() { # name want content
  local repo="${work}/r$((n += 1))" got=0
  mkdir -p "${repo}"
  printf '%s\n' "$3" > "${repo}/file.yaml"
  git -C "${repo}" init -q && git -C "${repo}" add -A
  "${root}/hack/check-image-refs.sh" "${repo}" > /dev/null 2>&1 || got=$?
  if [ "${got}" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1: exit ${got}, want $2"; fails=$((fails + 1)); fi
}
reg="ghcr.io/shadewright-labs"
case_ "published daemon image and version" 0 "image: ${reg}/sablecourt:0.1.0"
case_ "published agent image, candidate tag" 0 "image: ${reg}/sablecourt-agent:0.2.0-rc.1"
case_ "digest reference" 0 "image: ${reg}/sablecourt@sha256:$(printf 'a%.0s' $(seq 64))"
case_ "no tag" 0 "see ${reg}/sablecourt-agent for details"
case_ "latest" 0 "image: ${reg}/sablecourt:latest"
case_ "minor-only tag (not pushed)" 1 "image: ${reg}/sablecourt:0.1"
case_ "main tag (not pushed)" 1 "image: ${reg}/sablecourt:main"
case_ "commit tag (not pushed)" 1 "image: ${reg}/sablecourt:sha-0123abc"
case_ "unpublished name" 1 "image: ${reg}/sablecourtd:0.1.0"
case_ "tag with a v" 1 "image: ${reg}/sablecourt-agent:v0.1.0"
case_ "unknown fictional image" 1 "image: ${reg}/triage:0.1.0"
case_ "short digest" 1 "image: ${reg}/sablecourt@sha256:short"
case_ "unknown digest algorithm" 1 "image: ${reg}/sablecourt@bogus:07"
case_ "trailing colon" 1 "image: ${reg}/sablecourt:"
case_ "digest one digit short" 1 "image: ${reg}/sablecourt@sha256:$(printf 'a%.0s' $(seq 63))"
case_ "upper-case digest" 1 "image: ${reg}/sablecourt-agent@sha256:$(printf 'A%.0s' $(seq 64))"
case_ "digest followed by a tag" 1 "image: ${reg}/sablecourt@sha256:$(printf 'a%.0s' $(seq 64)):0.1.0"
case_ "tag and digest" 0 "image: ${reg}/sablecourt:0.1.0@sha256:$(printf 'a%.0s' $(seq 64))"
case_ "valid tag, short digest" 1 "image: ${reg}/sablecourt:0.1.0@sha256:short"
case_ "valid tag, upper-case digest" 1 "image: ${reg}/sablecourt-agent:latest@sha256:$(printf 'A%.0s' $(seq 64))"
case_ "bad tag, valid digest" 1 "image: ${reg}/sablecourt:main@sha256:$(printf 'a%.0s' $(seq 64))"
case_ "empty digest" 1 "image: ${reg}/sablecourt@"
case_ "colon at the end of a sentence" 1 "pull ${reg}/sablecourt-agent: it is published"

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
