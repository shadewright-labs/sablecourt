#!/usr/bin/env bash
# Prints `published=true` when the GitHub release of a tag exists and is published, `published=false` when it is
# a draft or not visible (the job token of a read-only job does not see drafts), for the gate jobs of images.yml
# and release.yml: a rerun after publication must change nothing, so they stop there. Fails when the answer is
# not a boolean. Usage: hack/release-state.sh <owner/repo> <tag>    (needs gh and GH_TOKEN)
# Tests: hack/test/release-scripts_test.sh (with a gh stub).
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <owner/repo> <tag>" >&2
  exit 2
fi
repo="$1" tag="$2"
if ! state="$(gh release view "${tag}" --repo "${repo}" --json isDraft --jq '.isDraft | if type == "boolean" then tostring else "invalid" end' 2> "${TMPDIR:-/tmp}/release-state.err.$$")"; then
  if grep -qi 'not found' "${TMPDIR:-/tmp}/release-state.err.$$"; then
    rm -f "${TMPDIR:-/tmp}/release-state.err.$$"
    echo "published=false"
    exit 0
  fi
  cat "${TMPDIR:-/tmp}/release-state.err.$$" >&2
  rm -f "${TMPDIR:-/tmp}/release-state.err.$$"
  echo "cannot read the release of ${tag}" >&2
  exit 1
fi
rm -f "${TMPDIR:-/tmp}/release-state.err.$$"
case "${state}" in
  true) echo "published=false" ;;
  false) echo "published=true" ;;
  *) echo "release ${tag} has no boolean draft state ('${state}')" >&2; exit 1 ;;
esac
