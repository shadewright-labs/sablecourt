#!/usr/bin/env bash
# Publishes the draft release of a tag only when it is complete, the last job of .github/workflows/release.yml:
#   - the release has a boolean draft state and its pre-release flag matches the tag (set for vX.Y.Z-rc.N,
#     clear for vX.Y.Z);
#   - its body holds the exact verification section for these image digests (hack/release-verify-notes.sh);
#   - the four archives, checksums.txt and checksums.txt.sigstore.json are uploaded, and each uploaded asset's
#     sha256 digest equals the file the build and sign jobs made (the local directory).
# Missing or different pieces leave the release a draft and fail the job. A release that is already published
# and complete is left alone (exit 0). Usage:
#   hack/publish-release.sh <owner/repo> <tag> <daemon image digest> <agent image digest> <local dist dir>
# Tests: hack/test/release-scripts_test.sh (with a gh stub).
set -euo pipefail

if [ "$#" -ne 5 ]; then
  echo "usage: $0 <owner/repo> <tag> <daemon image digest> <agent image digest> <local dist dir>" >&2
  exit 2
fi
repo="$1" tag="$2" dist="$5"
# Reuse the argument validation and require the exact verification section for this tag and these digests.
expected="$("$(dirname "$0")/release-verify-notes.sh" "$1" "$2" "$3" "$4" < /dev/null)"
version="${tag#v}"
prerelease=false
if [ "${version}" != "${version%%-*}" ]; then prerelease=true; fi

# name -> sha256 of the local file, as a JSON object.
want="{}"
for os in linux darwin; do
  for arch in amd64 arm64; do
    names+=("sablecourt_${version}_${os}_${arch}.tar.gz")
  done
done
names+=(checksums.txt checksums.txt.sigstore.json)
for name in "${names[@]}"; do
  [ -s "${dist}/${name}" ] || { echo "missing local file ${dist}/${name}" >&2; exit 1; }
  sum="$(sha256sum "${dist}/${name}")"
  want="$(jq --arg n "${name}" --arg d "sha256:${sum%% *}" '. + {($n): $d}' <<< "${want}")"
done

deadline=$(( $(date +%s) + ${RELEASE_WAIT_SECONDS:-300} ))
while true; do
  if release="$(gh release view "${tag}" --repo "${repo}" --json tagName,isDraft,isPrerelease,body,assets)"; then
    if ! jq -e '(.isDraft | type) == "boolean" and (.isPrerelease | type) == "boolean"' <<< "${release}" > /dev/null; then
      echo "release ${tag} has no boolean draft or pre-release state; refusing to publish" >&2
      exit 1
    fi
    if [ "$(jq -r .isPrerelease <<< "${release}")" != "${prerelease}" ]; then
      echo "release ${tag} has isPrerelease=$(jq -r .isPrerelease <<< "${release}"), the tag needs ${prerelease}; fix the release and re-run" >&2
      exit 1
    fi
    if jq -e --arg tag "${tag}" --arg notes "${expected}" --argjson want "${want}" '
        .tagName == $tag and
        (.body | contains($notes)) and
        (.assets as $assets | $want | to_entries | all(. as $w |
          any($assets[]; .name == $w.key and .state == "uploaded" and .size > 0 and .digest == $w.value)))
      ' <<< "${release}" > /dev/null; then
      if [ "$(jq -r .isDraft <<< "${release}")" = true ]; then
        gh release edit "${tag}" --repo "${repo}" --draft=false
        echo "published ${tag}: verified image digests and all six binary assets present with matching digests"
      else
        echo "release ${tag} is already published with all required artifacts"
      fi
      exit 0
    fi
  fi
  if [ "$(date +%s)" -ge "${deadline}" ]; then
    echo "release ${tag} is incomplete: missing release, exact digest and verification notes, or uploaded binary assets with the built digests; leaving the draft unpublished. Fix the failed job of the tag's Release assets run and re-run it" >&2
    exit 1
  fi
  echo "waiting for release ${tag}: digest and verification notes and all six binary assets required"
  sleep "${RELEASE_POLL_SECONDS:-15}"
done
