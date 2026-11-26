#!/usr/bin/env bash
# Resolves the digests of the sablecourt and sablecourt-agent images of a release tag and verifies each one with
# hack/verify-image.sh: the cosign signature, the SPDX SBOM attestation of each platform image and the GitHub
# build provenance attestation of the index, against images.yml on that very tag, with provenance built from
# the expected commit (--source-ref and --source-digest), so an image that another commit published under the
# same tag does not count. Prints `daemon=<digest>` and `agent=<digest>` on stdout (the format of
# $GITHUB_OUTPUT); progress goes to stderr. Run by release.yml (images and verify-published jobs) and by
# images.yml (verify-published job). Usage:
#   hack/resolve-image-digests.sh <tag, with v> <owner/repository> <owner> <expected commit sha>
# images.yml applies the version tag only after it has verified both images, so this waits for each tag and for
# images that verify: every POLL_SECONDS (30), at most WAIT_SECONDS (1800) in all, then fails naming the image
# it waited for. Needs docker (buildx imagetools), jq, cosign and gh (GH_TOKEN), logged in to ghcr.io where the
# packages are private. Tests: hack/test/release-scripts_test.sh (with stubs).
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <tag> <owner/repository> <owner> <expected commit sha>" >&2
  exit 2
fi
tag="$1" repository="$2" commit="$4"
owner="$(printf '%s' "$3" | tr '[:upper:]' '[:lower:]')"
printf '%s' "${commit}" | grep -Eq '^[0-9a-f]{40}$' || { echo "bad commit '${commit}'" >&2; exit 2; }
printf '%s' "${tag}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$' || { echo "bad tag '${tag}'" >&2; exit 2; }
poll="${POLL_SECONDS:-30}" wait_for="${WAIT_SECONDS:-1800}"
deadline=$(( $(date +%s) + wait_for ))
here="$(cd "$(dirname "$0")" && pwd)"

found_daemon="" found_agent=""
for pair in daemon:sablecourt agent:sablecourt-agent; do
  key="${pair%%:*}" ref="ghcr.io/${owner}/${pair#*:}"
  d=""
  until [ -n "${d}" ]; do
    got="$(docker buildx imagetools inspect "${ref}:${tag#v}" --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"' || true)"
    if printf '%s' "${got}" | grep -Eq '^sha256:[a-f0-9]{64}$' \
        && QUIET=1 "${here}/verify-image.sh" "${repository}" "refs/tags/${tag}" "${commit}" "${ref}@${got}" 2>/dev/null; then
      d="${got}"
    elif [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "no image ${ref}:${tag#v} signed, SBOM-attested and with provenance from images.yml on ${tag} at ${commit} after $((wait_for / 60)) minutes; re-run this job once the Images workflow of ${tag} has finished" >&2
      if [ -n "${got}" ]; then QUIET=1 "${here}/verify-image.sh" "${repository}" "refs/tags/${tag}" "${commit}" "${ref}@${got}" || true; fi
      exit 1
    else
      echo "waiting for ${ref}:${tag#v} and its verification" >&2
      sleep "${poll}"
    fi
  done
  echo "${ref}@${d}" >&2
  printf -v "found_${key}" '%s' "${d}"
done
echo "daemon=${found_daemon}"
echo "agent=${found_agent}"
