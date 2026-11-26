#!/usr/bin/env bash
# Publishes the draft release of a tag only when it is complete: the exact verification section for these
# image digests (hack/release-verify-notes.sh) is in its body, and the four archives, checksums.txt and its
# signature bundle are uploaded. Missing pieces leave the release a draft and fail the job. A rerun on a
# release that is already published and complete is a no-op. The last job of .github/workflows/release.yml.
# Usage: hack/publish-release.sh <owner/repo> <tag> <daemon image digest> <agent image digest>
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <owner/repo> <tag> <daemon image digest> <agent image digest>" >&2
  exit 2
fi
repo="$1" tag="$2"
# Reuse the argument validation and require the exact verification section for this tag and these digests.
expected="$("$(dirname "$0")/release-verify-notes.sh" "$@" < /dev/null)"
assets="$(jq -cn --arg v "${tag#v}" '
  ["linux", "darwin"] as $os | ["amd64", "arm64"] as $arch |
  [($os[] as $o | $arch[] as $a | "sablecourt_\($v)_\($o)_\($a).tar.gz"),
   "checksums.txt", "checksums.txt.sigstore.json"]')"
# The jobs before this one uploaded the assets and wrote the notes; a short wait rides out API lag.
deadline=$(( $(date +%s) + ${RELEASE_WAIT_SECONDS:-300} ))
while true; do
  if release="$(gh release view "${tag}" --repo "${repo}" --json tagName,isDraft,body,assets)" &&
    jq -e --arg tag "${tag}" --arg notes "${expected}" --argjson names "${assets}" '
      .tagName == $tag and
      (.body | contains($notes)) and
      (.assets as $assets | all($names[]; . as $name |
        any($assets[]; .name == $name and .state == "uploaded" and .size > 0)))
    ' <<< "${release}" > /dev/null; then
    if [ "$(jq -r .isDraft <<< "${release}")" = true ]; then
      gh release edit "${tag}" --repo "${repo}" --draft=false
      echo "published ${tag}: verified image digests and all six binary assets present"
    else
      echo "release ${tag} is already published with all required artifacts"
    fi
    exit 0
  fi
  if [ "$(date +%s)" -ge "${deadline}" ]; then
    echo "release ${tag} is incomplete: missing release, exact digest and verification notes, or uploaded binary assets; leaving the draft unpublished. Fix the failed job of the tag's Release assets run and re-run it" >&2
    exit 1
  fi
  echo "waiting for release ${tag}: digest and verification notes and all six binary assets required"
  sleep 15
done
