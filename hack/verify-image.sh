#!/usr/bin/env bash
# Verifies one image index pushed by images.yml, by digest: the keyless cosign signature of the index, the signed
# SPDX SBOM attestation of each platform image (linux/amd64 and linux/arm64, the child digests of the index),
# and the GitHub build provenance attestation of the index, each against images.yml of this repository at
# exactly the given ref; the provenance must also name the given source ref and commit. Exit 0 only when all of
# them verify; any failure (of cosign, gh, docker, the registry) exits 1. Used by the verify job of images.yml
# before any tag is applied, and by hack/resolve-image-digests.sh. Needs cosign, gh (GH_TOKEN), docker (buildx
# imagetools) and jq. Usage:
#   hack/verify-image.sh <owner/repository> <git ref, e.g. refs/tags/v0.1.0> <commit sha> <image@sha256:digest>
# QUIET=1 drops the tools' own output (the resolver polls with it).
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <owner/repository> <git ref> <commit sha> <image@sha256:digest>" >&2
  exit 2
fi
repository="$1" ref="$2" commit="$3" image_ref="$4"
here="$(cd "$(dirname "$0")" && pwd)"
printf '%s' "${repository}" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || { echo "bad repository '${repository}'" >&2; exit 2; }
printf '%s' "${ref}" | grep -Eq '^refs/(tags|heads)/[A-Za-z0-9._/-]+$' || { echo "bad ref '${ref}'" >&2; exit 2; }
printf '%s' "${commit}" | grep -Eq '^[0-9a-f]{40}$' || { echo "bad commit '${commit}'" >&2; exit 2; }
printf '%s' "${image_ref}" | grep -Eq '^[a-z0-9._/-]+@sha256:[a-f0-9]{64}$' || { echo "not an image by digest: '${image_ref}'" >&2; exit 2; }
image="${image_ref%@*}"
identity="https://github.com/${repository}/.github/workflows/images.yml@${ref}"
issuer="https://token.actions.githubusercontent.com"
out=/dev/stderr
if [ "${QUIET:-}" = 1 ]; then out=/dev/null; fi

fail() { echo "$1" >&2; exit 1; }

cosign verify --certificate-identity "${identity}" --certificate-oidc-issuer "${issuer}" \
  "${image_ref}" > "${out}" 2>&1 || fail "${image_ref}: no signature from ${identity}"
children="$("${here}/image-platform-digests.sh" "${image_ref}" 2> "${out}")" || fail "${image_ref}: cannot read its linux/amd64 and linux/arm64 images"
while IFS= read -r line; do
  child="${line#*=}"
  cosign verify-attestation --type spdxjson \
    --certificate-identity "${identity}" --certificate-oidc-issuer "${issuer}" \
    "${image}@${child}" > "${out}" 2>&1 || fail "${image}@${child} (${line%%=*}): no SPDX SBOM attestation from ${identity}"
done <<< "${children}"
gh attestation verify "oci://${image_ref}" --repo "${repository}" \
  --signer-workflow "${repository}/.github/workflows/images.yml" \
  --source-ref "${ref}" --source-digest "${commit}" > "${out}" 2>&1 \
  || fail "${image_ref}: no build provenance from images.yml for ${ref} at ${commit}"
echo "${image_ref}: signature, SBOMs and provenance verified for ${ref} at ${commit}" >&2
