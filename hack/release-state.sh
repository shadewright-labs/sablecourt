#!/usr/bin/env bash
# Prints `published=true` when the GitHub release of a tag exists and is published, `published=false` when it is
# a draft or there is none, for the gate jobs of images.yml and release.yml. "None" is only gh's own answer for a
# missing release (stderr exactly `release not found`); the job token of a read-only job does not see drafts, so
# a draft reads the same way, which is the case in which the run goes on. Any other failure of gh (no
# configuration, a network or server error, a bad token) and a draft state that is not a boolean fail closed
# (exit 1), so an error never lets a run write to a release it could not see. published=true makes the
# workflows write nothing; they then verify the published release instead (release.yml: verify-published,
# images.yml: verify-published), so a rerun after publication ends green only when that release checks out.
# Usage: hack/release-state.sh <owner/repo> <tag>    (needs gh and GH_TOKEN)
# Tests: hack/test/release-scripts_test.sh (with a gh stub).
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <owner/repo> <tag>" >&2
  exit 2
fi
repo="$1" tag="$2"
err="$(mktemp "${TMPDIR:-/tmp}/release-state.XXXXXX")"
trap 'rm -f "${err}"' EXIT
if ! state="$(gh release view "${tag}" --repo "${repo}" --json isDraft --jq '.isDraft | if type == "boolean" then tostring else "invalid" end' 2> "${err}")"; then
  # Exactly gh's not-found message and nothing else; "configuration file not found" and the like are errors.
  if [ "$(tr -d '\r' < "${err}" | sed -e '/^[[:space:]]*$/d')" = "release not found" ]; then
    echo "published=false"
    exit 0
  fi
  cat "${err}" >&2
  echo "cannot read the release of ${tag}; refusing to go on" >&2
  exit 1
fi
case "${state}" in
  true) echo "published=false" ;;
  false) echo "published=true" ;;
  *) echo "release ${tag} has no boolean draft state ('${state}')" >&2; exit 1 ;;
esac
