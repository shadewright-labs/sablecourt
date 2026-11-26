#!/usr/bin/env bash
# Resolves the digests of the sablecourt and sablecourt-agent images of a release tag and verifies each one
# (cosign signature, the SPDX SBOM attestation of each platform image, and the GitHub build provenance
# attestation of the index) against the identity of the Images workflow on that very tag. The Images job records
# provenance last, so an image counts as ready only when all three verify. Prints `daemon=<digest>` and `agent=<digest>` on stdout (the format of
# $GITHUB_OUTPUT); progress goes to stderr. The images job of .github/workflows/release.yml runs this once and
# hands the digests to the release notes, so the release lists the images that were verified. Usage:
#   hack/resolve-image-digests.sh <tag, with v> <owner/repository> <owner>
# The Images workflow of the same tag runs in parallel, so this waits for each image and for a signature that
# verifies: every POLL_SECONDS (30), at most WAIT_SECONDS (1800) in all, then fails naming the image it waited for.
# Needs docker (buildx imagetools), jq, cosign and gh (GH_TOKEN), logged in to ghcr.io where the packages are
# private.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <tag> <owner/repository> <owner>" >&2
  exit 2
fi
tag="$1" repository="$2"
owner="$(printf '%s' "$3" | tr '[:upper:]' '[:lower:]')"
poll="${POLL_SECONDS:-30}" wait_for="${WAIT_SECONDS:-1800}"
issuer="https://token.actions.githubusercontent.com"
identity="https://github.com/${repository}/.github/workflows/images.yml@refs/tags/${tag}"
deadline=$(( $(date +%s) + wait_for ))
here="$(cd "$(dirname "$0")" && pwd)"

# The SBOM is attested per platform image: every child digest of the index must carry one.
sboms_verify() { # image reference, index digest
  local children child
  children="$("${here}/image-platform-digests.sh" "$1@$2" 2>/dev/null | sed 's/^[a-z0-9]*=//')" || return 1
  [ -n "${children}" ] || return 1
  for child in ${children}; do
    cosign verify-attestation --type spdxjson \
      --certificate-identity "${identity}" --certificate-oidc-issuer "${issuer}" \
      "$1@${child}" > /dev/null 2>&1 || return 1
  done
}

# The provenance must come from images.yml of this repository, built from this tag.
provenance_verify() { # image reference, index digest
  gh attestation verify "oci://$1@$2" --repo "${repository}" \
    --signer-workflow "${repository}/.github/workflows/images.yml" \
    --source-ref "refs/tags/${tag}" > /dev/null 2>&1
}

found_daemon="" found_agent=""
for pair in daemon:sablecourt agent:sablecourt-agent; do
  key="${pair%%:*}" ref="ghcr.io/${owner}/${pair#*:}"
  d=""
  until [ -n "${d}" ]; do
    got="$(docker buildx imagetools inspect "${ref}:${tag#v}" --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"' || true)"
    if printf '%s' "${got}" | grep -Eq '^sha256:[a-f0-9]{64}$' && cosign verify \
        --certificate-identity "${identity}" --certificate-oidc-issuer "${issuer}" \
        "${ref}@${got}" > /dev/null 2>&1 && sboms_verify "${ref}" "${got}" \
        && provenance_verify "${ref}" "${got}"; then
      d="${got}"
    elif [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "no image ${ref}:${tag#v} signed, SBOM-attested and with provenance from ${identity} after $((wait_for / 60)) minutes; re-run this job once the Images workflow of ${tag} has finished" >&2
      exit 1
    else
      echo "waiting for ${ref}:${tag#v} and its signature" >&2
      sleep "${poll}"
    fi
  done
  echo "${ref}@${d}" >&2
  printf -v "found_${key}" '%s' "${d}"
done
echo "daemon=${found_daemon}"
echo "agent=${found_agent}"
