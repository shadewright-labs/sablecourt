# Verify a release

You need Bash, GNU `sha256sum`, jq, cosign 3, the GitHub CLI (`gh`) logged in, and for the images Docker with Buildx.
A pass shows that the file or image is the one this repository's release workflow signed at the tag `v$VERSION`, and that its provenance names the source commit `$SHA`; it does not show who created the tag.

## Commands

Put the blocks in this order in one file and run it with `bash`. Take `SHA` and the image digests from the release notes.

```bash
set -euo pipefail
VERSION=0.1.0
SHA=0000000000000000000000000000000000000000
IDENTITY="https://github.com/shadewright-labs/sablecourt/.github/workflows/release.yml@refs/tags/v$VERSION"
ISSUER="https://token.actions.githubusercontent.com"
```

A pass sets the certificate identity that the cosign and gh checks below require, and the commit `$SHA` that the gh checks require.

```bash
mkdir "sablecourt-$VERSION"
cd "sablecourt-$VERSION"
gh release download "v$VERSION" --repo shadewright-labs/sablecourt
```

A pass puts the release's files in the folder `sablecourt-$VERSION`.

```bash
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" \
  checksums.txt
```

A pass shows that `checksums.txt` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`.

```bash
sha256sum --check --strict checksums.txt
LINES=$(wc -l < checksums.txt)
test "$LINES" -eq 9
```

A pass shows that `checksums.txt` lists nine files and that each has the SHA-256 recorded there.

```bash
for FILE in sablecourt_*.tar.gz sablecourt_*.spdx.json checksums.txt; do
  gh attestation verify "$FILE" \
    --repo shadewright-labs/sablecourt \
    --cert-identity "$IDENTITY" \
    --cert-oidc-issuer "$ISSUER" \
    --source-ref "refs/tags/v$VERSION" \
    --source-digest "$SHA" \
    --deny-self-hosted-runners > /dev/null
  gh attestation verify "$FILE" \
    --bundle "sablecourt_${VERSION}_provenance.intoto.jsonl" \
    --repo shadewright-labs/sablecourt \
    --cert-identity "$IDENTITY" \
    --cert-oidc-issuer "$ISSUER" \
    --source-ref "refs/tags/v$VERSION" \
    --source-digest "$SHA" \
    --deny-self-hosted-runners > /dev/null
done
```

A pass shows that each archive, SBOM and `checksums.txt` has build provenance for `release.yml` at the tag `v$VERSION` and the commit `$SHA`, from GitHub and from `sablecourt_${VERSION}_provenance.intoto.jsonl`.

For the agent image, set `IMAGE=ghcr.io/shadewright-labs/sablecourt-agent` and its digest in the next block.

```bash
IMAGE=ghcr.io/shadewright-labs/sablecourt
DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000
cosign verify \
  --certificate-identity "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" \
  "$IMAGE@$DIGEST" > /dev/null
```

A pass shows that the image index `$IMAGE@$DIGEST` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`.

```bash
for ARCH in amd64 arm64; do
  CHILD=$(docker buildx imagetools inspect --raw "$IMAGE@$DIGEST" | jq -er --arg a "$ARCH" \
    '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $a) | .digest] | if length == 1 then .[0] else error("not one \($a) image") end')
  cosign verify-attestation --type spdxjson \
    --certificate-identity "$IDENTITY" \
    --certificate-oidc-issuer "$ISSUER" \
    "$IMAGE@$CHILD" > "attestations.json"
  SBOM="sablecourt_${VERSION}_image_${IMAGE##*/}_linux_$ARCH.spdx.json"
  jq -se --slurpfile want "$SBOM" 'map(.payload | @base64d | fromjson | .predicate) | any([.] == $want)' \
    "attestations.json" > /dev/null
done
```

A pass shows that the linux/amd64 and linux/arm64 images of the index each carry a signed SPDX attestation equal to the release's SBOM file for that image, and that the file holds one JSON value.

```bash
gh attestation verify "oci://$IMAGE@$DIGEST" \
  --repo shadewright-labs/sablecourt \
  --cert-identity "$IDENTITY" \
  --cert-oidc-issuer "$ISSUER" \
  --source-ref "refs/tags/v$VERSION" \
  --source-digest "$SHA" \
  --deny-self-hosted-runners > /dev/null
gh attestation verify "oci://$IMAGE@$DIGEST" \
  --bundle "sablecourt_${VERSION}_image_${IMAGE##*/}.intoto.jsonl" \
  --repo shadewright-labs/sablecourt \
  --cert-identity "$IDENTITY" \
  --cert-oidc-issuer "$ISSUER" \
  --source-ref "refs/tags/v$VERSION" \
  --source-digest "$SHA" \
  --deny-self-hosted-runners > /dev/null
```

A pass shows that the image index has build provenance for `release.yml` at the tag `v$VERSION` and the commit `$SHA`, from GitHub and from `sablecourt_${VERSION}_image_${IMAGE##*/}.intoto.jsonl`.

## What this does not prove

- Who created the tag `v$VERSION`: the repository's tag ruleset decides that.
- Which checks the tagged commit's `release.yml` ran: the certificate names the workflow file and the tag, not the file's content.
- That the commit `$SHA` is on `main`.
- That an image tag such as `$VERSION` points at `$DIGEST`: deploy by digest.
- That the images have no known vulnerabilities.
