# Verify a release

Check that a Sablecourt archive or image was built by this repository's release workflow from a release tag, and that it has not changed since.

Nothing has been released yet. The commands below apply to every release that [`release.yml`](../../.github/workflows/release.yml) publishes; the workflow runs the same checks, pinned to its own tag and commit, before the release becomes public ([CI, Releases](../ci.md#releases)).

## What is signed

Each release has:

- four archives, `sablecourt_<version>_<os>_<arch>.tar.gz` for darwin and linux on amd64 and arm64, each holding `sablecourtd` and `sablectl`;
- `sablecourt_<version>_sbom.spdx.json`, an SPDX SBOM of the eight binaries;
- `checksums.txt`, the sha256 of those five files, and `checksums.txt.sigstore.json`, a cosign keyless signature of `checksums.txt`;
- a GitHub build provenance attestation for each archive, the SBOM and `checksums.txt`;
- the images `ghcr.io/shadewright-labs/sablecourt` and `ghcr.io/shadewright-labs/sablecourt-agent` for linux/amd64 and linux/arm64. Each image index has a cosign keyless signature and a build provenance attestation, and each platform image has an SPDX SBOM attached as a signed attestation. The release notes list both index digests.

Keyless means the signing certificate was issued to the workflow run through GitHub's OIDC token; there is no key to download or trust. The certificate names the workflow file and the tag it ran on, and that name is what the commands below check.

## Prerequisites

- [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 3.x. cosign 2.x does not find the image signatures, which are stored as Sigstore bundles.
- The [GitHub CLI](https://cli.github.com/) (`gh`), logged in.
- For images: [Docker](https://docs.docker.com/engine/install/) (for `docker buildx imagetools`) and [jq](https://jqlang.org/).

Set these once:

```bash
VERSION=0.1.0    # the release without its leading v, for example 0.1.0 or 0.2.0-rc.1
IDENTITY='^https://github\.com/shadewright-labs/sablecourt/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?$'
ISSUER=https://token.actions.githubusercontent.com
```

`IDENTITY` accepts any release tag. To accept one release only, pass `--certificate-identity "https://github.com/shadewright-labs/sablecourt/.github/workflows/release.yml@refs/tags/v$VERSION"` in place of `--certificate-identity-regexp "$IDENTITY"`.

## Verify an archive

```bash
mkdir "sablecourt-$VERSION" && cd "sablecourt-$VERSION"
gh release download "v$VERSION" --repo shadewright-labs/sablecourt

# 1. checksums.txt was signed by release.yml on a release tag.
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" \
  checksums.txt

# 2. The archives and the SBOM match checksums.txt (on macOS: shasum -a 256 --check checksums.txt).
sha256sum --check --strict checksums.txt

# 3. Each file has build provenance from release.yml on this tag.
for FILE in sablecourt_* checksums.txt; do
  gh attestation verify "$FILE" \
    --repo shadewright-labs/sablecourt \
    --signer-workflow shadewright-labs/sablecourt/.github/workflows/release.yml \
    --source-ref "refs/tags/v$VERSION"
done
```

A good result: `cosign verify-blob` prints `Verified OK`, `sha256sum` prints `OK` after each of the five file names, and each `gh attestation verify` ends with `Verification succeeded!` and names `.github/workflows/release.yml` and `refs/tags/v<version>`. Every command exits 0. Any other result, or a non-zero exit, means the file does not come from this release: do not use it.

## Verify an image

Verify by digest, never by tag: a tag can be moved, a digest cannot. Take the digest from the release notes, or read it from the version tag and compare it with the notes:

```bash
IMAGE=ghcr.io/shadewright-labs/sablecourt    # or ghcr.io/shadewright-labs/sablecourt-agent
docker buildx imagetools inspect "$IMAGE:$VERSION" --format '{{json .Manifest}}' | jq -r .digest
DIGEST=sha256:<digest from the release notes>

# 1. The image index was signed by release.yml on a release tag.
cosign verify \
  --certificate-identity-regexp "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" \
  "$IMAGE@$DIGEST"

# 2. Each platform image carries a signed SPDX SBOM; this writes it to sbom-<arch>.spdx.json.
for ARCH in amd64 arm64; do
  CHILD=$(docker buildx imagetools inspect --raw "$IMAGE@$DIGEST" | jq -r --arg a "$ARCH" \
    '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $a) | .digest')
  cosign verify-attestation --type spdxjson \
    --certificate-identity-regexp "$IDENTITY" \
    --certificate-oidc-issuer "$ISSUER" \
    "$IMAGE@$CHILD" | jq -r '.payload | @base64d | fromjson | .predicate' > "sbom-$ARCH.spdx.json"
done

# 3. The index has build provenance from release.yml on this tag.
gh attestation verify "oci://$IMAGE@$DIGEST" \
  --repo shadewright-labs/sablecourt \
  --signer-workflow shadewright-labs/sablecourt/.github/workflows/release.yml \
  --source-ref "refs/tags/v$VERSION"
```

A good result: `cosign verify` prints `Verification for` and the image reference, followed by the checks it made (the claims, the transparency log entry, the certificate) and the signature as JSON; `cosign verify-attestation` prints the same checks for each platform image and the two SBOM files are not empty; `gh attestation verify` ends with `Verification succeeded!`. Every command exits 0. Then deploy `$IMAGE@$DIGEST`, not the tag.

## What the checks prove and what they do not

They prove that the file or image was built and signed by `.github/workflows/release.yml` of `shadewright-labs/sablecourt`, running on GitHub-hosted runners for a tag of the form `vX.Y.Z` or `vX.Y.Z-rc.N`, and that it has not changed since. `gh attestation verify` also records the commit; the workflow itself checks that this commit is on main before it builds anything.

They do not prove who pushed the tag. Creating `v*` tags is limited to repository admins by a repository ruleset, which no file in this repository enforces.
