#!/usr/bin/env bash
# Refuses to move a version image tag, for release.yml: reads <image>:<tag> and compares its digest with <digest>.
#   exit 0, prints "absent": the registry answers that the tag does not exist (buildx: "<image>:<tag>: not found")
#   exit 0, prints "same":   the tag already points at <digest> (a rerun of the same release)
#   exit 1: the tag points at another digest, or the lookup failed in any other way (fail closed)
# Usage: hack/release-image-tag.sh <image> <tag> <digest>    (needs docker buildx and jq)
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <image> <tag> <digest>" >&2
  exit 2
fi
image="$1" tag="$2" digest="$3"
if ! [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "'${digest}' is not a sha256 digest" >&2
  exit 2
fi
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

if docker buildx imagetools inspect "${image}:${tag}" --format '{{json .Manifest}}' > "${work}/out" 2> "${work}/err"; then
  got="$(jq -er '.digest' "${work}/out")" || { echo "could not read the digest of ${image}:${tag}" >&2; exit 1; }
  if [ "${got}" = "${digest}" ]; then
    echo same
    exit 0
  fi
  echo "${image}:${tag} already points at ${got}, not at ${digest} of this run; refusing to move a version tag" >&2
  exit 1
fi
if grep -qxF "ERROR: ${image}:${tag}: not found" "${work}/err"; then
  echo absent
  exit 0
fi
echo "could not read ${image}:${tag}:" >&2
cat "${work}/err" >&2
exit 1
