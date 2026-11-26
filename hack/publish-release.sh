#!/usr/bin/env bash
# Publishes the draft release of a tag only when it is complete, the last job of .github/workflows/release.yml:
#   - the release has a boolean draft state and its pre-release flag matches the tag (set for vX.Y.Z-rc.N,
#     clear for vX.Y.Z);
#   - its body holds the exact verification section for these image digests (hack/release-verify-notes.sh);
#   - its assets are exactly the four archives, checksums.txt and checksums.txt.sigstore.json (an extra asset
#     fails too: nobody verified it), each uploaded with the sha256 digest of the file in the local directory.
# A draft that passes is read again immediately before `gh release edit --draft=false`, and is published only if
# it is still a draft and still passes. Missing or different pieces leave the release a draft and fail the job
# (a draft is waited for up to RELEASE_WAIT_SECONDS, 300, for the other jobs to finish). A release that is
# already published is never written: complete, it passes (exit 0); incomplete, it fails at once.
# With `verify-only` as the sixth argument nothing is ever written: the release must already be published and
# complete (the verify-published job of release.yml, whose local directory holds the downloaded assets after
# their signature, checksums and provenance were verified). Usage:
#   hack/publish-release.sh <owner/repo> <tag> <daemon image digest> <agent image digest> <local dir> [verify-only]
# Tests: hack/test/release-scripts_test.sh (with a gh stub).
set -euo pipefail

if [ "$#" -lt 5 ] || [ "$#" -gt 6 ] || { [ "$#" -eq 6 ] && [ "$6" != verify-only ]; }; then
  echo "usage: $0 <owner/repo> <tag> <daemon image digest> <agent image digest> <local dist dir> [verify-only]" >&2
  exit 2
fi
repo="$1" tag="$2" dist="$5" mode="${6:-publish}"
# Reuse the argument validation and require the exact verification section for this tag and these digests.
expected="$("$(dirname "$0")/release-verify-notes.sh" "$1" "$2" "$3" "$4" < /dev/null)"
version="${tag#v}"
prerelease=false
if [ "${version}" != "${version%%-*}" ]; then prerelease=true; fi

# name -> sha256 of the local file, as a JSON object.
want="{}"
names=()
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

# view: the release as JSON, or fail.
view() { gh release view "${tag}" --repo "${repo}" --json tagName,isDraft,isPrerelease,body,assets; }

# problems <release json>: prints what is wrong with the release (nothing when it is complete).
problems() {
  jq -r --arg tag "${tag}" --arg notes "${expected}" --argjson want "${want}" --argjson pre "${prerelease}" '
    . as $rel
    | if (.isDraft | type) != "boolean" or (.isPrerelease | type) != "boolean" then "no boolean draft or pre-release state"
      else
        (if .isPrerelease != $pre then "isPrerelease is \(.isPrerelease), the tag needs \($pre)" else empty end),
        (if .tagName != $tag then "the tag name is \(.tagName)" else empty end),
        (if (.body | type) != "string" or ((.body | contains($notes)) | not)
         then "the verification section for these digests is missing" else empty end),
        (([$rel.assets[]?.name] | sort) as $have | ($want | keys) as $need
          | (if ($have - $need) != [] then "unexpected assets: \(($have - $need) | join(", "))" else empty end),
            (if ($have | length) != ($have | unique | length) then "duplicate asset names" else empty end)),
        ($want | to_entries[] | . as $w
          | if ([$rel.assets[]? | select(.name == $w.key and .state == "uploaded" and .size > 0 and .digest == $w.value)] | length) != 1
            then "\($w.key) is missing, not uploaded or has another digest" else empty end)
      end' <<< "$1" | sort -u
}

deadline=$(( $(date +%s) + ${RELEASE_WAIT_SECONDS:-300} ))
while true; do
  if release="$(view)"; then
    issues="$(problems "${release}")" || { echo "cannot evaluate the release of ${tag}" >&2; exit 1; }
    draft="$(jq -r '.isDraft' <<< "${release}")"
    if [ "${draft}" = false ]; then
      if [ -n "${issues}" ]; then
        printf 'release %s is published but not complete:\n%s\n' "${tag}" "${issues}" >&2
        exit 1
      fi
      echo "release ${tag} is published, with the verification section and exactly the six assets the run verified; nothing written"
      exit 0
    fi
    if [ "${mode}" = verify-only ]; then
      echo "release ${tag} is not published (isDraft=${draft}); verify-only writes nothing and fails" >&2
      exit 1
    fi
    if [ "${draft}" = true ] && [ -z "${issues}" ]; then
      # Read again just before the write: the release must still be a complete draft.
      release="$(view)" || { echo "cannot read release ${tag} again before publishing" >&2; exit 1; }
      issues="$(problems "${release}")" || { echo "cannot evaluate the release of ${tag}" >&2; exit 1; }
      if [ "$(jq -r '.isDraft' <<< "${release}")" != true ] || [ -n "${issues}" ]; then
        printf 'release %s changed while it was checked; not publishing:\n%s\n' "${tag}" "${issues:-no longer a draft}" >&2
        exit 1
      fi
      gh release edit "${tag}" --repo "${repo}" --draft=false
      echo "published ${tag}: verified image digests and exactly the six binary assets, with the built digests"
      exit 0
    fi
    # A state that is not a boolean, or a wrong pre-release flag, will not fix itself.
    if grep -q -e 'no boolean' -e 'isPrerelease is' <<< "${issues}"; then
      printf 'release %s cannot be published:\n%s\n' "${tag}" "${issues}" >&2
      exit 1
    fi
  else
    issues="cannot read the release"
  fi
  if [ "$(date +%s)" -ge "${deadline}" ]; then
    printf 'release %s is incomplete; leaving the draft unpublished. Fix the failed job of the Release assets run of the tag and re-run it:\n%s\n' "${tag}" "${issues}" >&2
    exit 1
  fi
  echo "waiting for release ${tag}: ${issues//$'\n'/; }"
  sleep "${RELEASE_POLL_SECONDS:-15}"
done
