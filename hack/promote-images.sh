#!/usr/bin/env bash
# Applies the tags of a run to the verified daemon and agent images (the promote job of images.yml, which runs
# only after the verify job has checked both image indexes by digest). The images are pushed by digest only, so
# before this runs no tag of this run exists. Both images are promoted in one run of this script, in two passes:
# first the run's own tags (main and sha-<commit> for main; X.Y.Z or X.Y.Z-rc.N for a tag) on both images, then
# the floating tags X.Y and latest on both, as hack/floating-tags.sh decides from the tags of the checkout at
# this moment (the job fetches them just before; its concurrency group lets one promotion run at a time, so a
# release cannot take a floating tag after a newer release was promoted). Each tag is created with
# `docker buildx imagetools create` from the digest and read back; any failure stops the script with exit 1.
# The two images cannot be tagged atomically: a failure after the first write leaves the other image without the
# tag until the job is re-run, which applies the same tags again. Usage:
#   hack/promote-images.sh <owner> <tag|branch> <ref name> <commit sha> <daemon digest> <agent digest>
# Tests: hack/test/release-scripts_test.sh (with a docker stub).
set -euo pipefail

if [ "$#" -ne 6 ]; then
  echo "usage: $0 <owner> <tag|branch> <ref name> <commit sha> <daemon digest> <agent digest>" >&2
  exit 2
fi
owner="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" ref_type="$2" ref_name="$3" commit="$4"
daemon="$5" agent="$6"
for d in "${daemon}" "${agent}"; do
  printf '%s' "${d}" | grep -Eq '^sha256:[a-f0-9]{64}$' || { echo "not a digest: '${d}'" >&2; exit 2; }
done
printf '%s' "${commit}" | grep -Eq '^[0-9a-f]{40}$' || { echo "bad commit '${commit}'" >&2; exit 2; }
printf '%s' "${owner}" | grep -Eq '^[a-z0-9_.-]+$' || { echo "bad owner '${owner}'" >&2; exit 2; }

own=() floating=()
case "${ref_type}" in
  branch)
    [ "${ref_name}" = main ] || { echo "refusing to promote branch ${ref_name}: only main is published" >&2; exit 1; }
    own=(main "sha-${commit:0:7}")
    ;;
  tag)
    [[ "${ref_name}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$ ]] \
      || { echo "refusing to promote ${ref_name}: not a release tag" >&2; exit 1; }
    minor_tag="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}"
    own=("${ref_name#v}")
    decision="$("$(dirname "$0")/floating-tags.sh" "${ref_name}")" || { echo "cannot decide the floating tags" >&2; exit 1; }
    if grep -qx 'minor=true' <<< "${decision}"; then
      floating+=("${minor_tag}")
    fi
    if grep -qx 'latest=true' <<< "${decision}"; then
      floating+=(latest)
    fi
    ;;
  *)
    echo "ref type must be tag or branch, not '${ref_type}'" >&2
    exit 2
    ;;
esac

# tag_both <tag>...: each tag on both images, created from the digest and read back.
tag_both() {
  local t pair image digest got
  for t in "$@"; do
    for pair in "sablecourt@${daemon}" "sablecourt-agent@${agent}"; do
      image="ghcr.io/${owner}/${pair%@*}" digest="${pair#*@}"
      docker buildx imagetools create --tag "${image}:${t}" "${image}@${digest}" \
        || { echo "could not tag ${image}@${digest} as ${t}" >&2; return 1; }
      got="$(docker buildx imagetools inspect "${image}:${t}" --format '{{json .Manifest.Digest}}' | tr -d '"')" \
        || { echo "could not read back ${image}:${t}" >&2; return 1; }
      [ "${got}" = "${digest}" ] || { echo "${image}:${t} is ${got}, expected ${digest}" >&2; return 1; }
      echo "${image}:${t} -> ${digest}"
    done
  done
}

tag_both "${own[@]}"
if [ "${#floating[@]}" -gt 0 ]; then
  tag_both "${floating[@]}"
fi
echo "promoted ${ref_name}: ${own[*]}${floating[*]:+ and ${floating[*]}}"
