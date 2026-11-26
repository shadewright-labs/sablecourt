#!/usr/bin/env bash
# Decides whether the images of a release tag may take the floating tags `latest` and `X.Y`. Prints
# `latest=true|false` and `minor=true|false` (the format of $GITHUB_OUTPUT). `latest` only when no stable tag in
# the repository is newer than this one, `X.Y` only when no stable tag with the same X.Y has a higher patch.
# Release candidates and branch pushes take neither. Reads the tags of the checkout; the promote job of
# images.yml fetches them again just before it runs this, inside a concurrency group that lets one promotion
# run at a time, so the answer is about the tags that exist when the floating tags move. Usage:
#   hack/floating-tags.sh <git ref name>      (a tag such as v0.2.0, or a branch name)
# Tests: hack/test/release-scripts_test.sh.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <ref name>" >&2
  exit 2
fi
# shellcheck source=hack/semver.sh
. "$(dirname "$0")/semver.sh"
ref="$1"
if ! [[ "${ref}" =~ ${SEMVER_STABLE_TAG} ]]; then
  printf 'latest=false\nminor=false\n'
  exit 0
fi
major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}" patch="${BASH_REMATCH[3]}"

latest=true same_minor=true
tags="$(git tag -l 'v*')"
while IFS= read -r t; do
  [ -n "${t}" ] || continue
  [[ "${t}" =~ ${SEMVER_STABLE_TAG} ]] || continue
  M="${BASH_REMATCH[1]}" m="${BASH_REMATCH[2]}" p="${BASH_REMATCH[3]}"
  if [ "$(semver_cmp "${M}.${m}.${p}" "${major}.${minor}.${patch}")" = 1 ]; then
    latest=false
  fi
  if [ "${M}" = "${major}" ] && [ "${m}" = "${minor}" ] && [ "$(semver_part_cmp "${p}" "${patch}")" = 1 ]; then
    same_minor=false
  fi
done <<< "${tags}"
printf 'latest=%s\nminor=%s\n' "${latest}" "${same_minor}"
