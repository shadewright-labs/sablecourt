# Verify a release

You need Bash, `cut`, `sort`, a `sha256sum` with `--check` and `--strict` (GNU coreutils has both), jq, cosign 3 and the GitHub CLI (`gh`) logged in to GitHub. For the images you also need Docker with Buildx and a login to `ghcr.io`, which `gh attestation verify` requires for an `oci://` image ([gh manual](https://cli.github.com/manual/gh_attestation_verify)).
A pass shows that `checksums.txt` and the image index you check carry signatures from a certificate for `release.yml` at the tag `v$VERSION`, that the archives and SBOMs have the SHA-256 values listed in `checksums.txt`, and that the build provenance of these files, of `checksums.txt` and of the index was signed with a certificate whose source repository digest is `$SHA`. It does not show who created the tag.

## Commands

Put the blocks in this order in one file and run it with `bash`. Take `SHA` and the image digests from the release notes, where the release workflow writes the commit it built and both image digests.

```bash
set -euo pipefail
VERSION=0.1.0
SHA=0000000000000000000000000000000000000000
IDENTITY="https://github.com/shadewright-labs/sablecourt/.github/workflows/release.yml@refs/tags/v$VERSION"
ISSUER="https://token.actions.githubusercontent.com"
```

A pass sets the certificate identity and issuer that the cosign and gh commands below require, and the source repository digest `$SHA` that the gh commands require.

```bash
mkdir "sablecourt-$VERSION"
cd "sablecourt-$VERSION"
gh release download "v$VERSION" --repo shadewright-labs/sablecourt
EXPECTED=$(printf '%s\n' checksums.txt checksums.txt.sigstore.json \
  "sablecourt_${VERSION}_sbom.spdx.json" "sablecourt_${VERSION}_provenance.intoto.jsonl" \
  "sablecourt_${VERSION}_"{darwin,linux}_{amd64,arm64}.tar.gz \
  "sablecourt_${VERSION}_image_"{sablecourt,sablecourt-agent}_linux_{amd64,arm64}.spdx.json \
  "sablecourt_${VERSION}_image_"{sablecourt,sablecourt-agent}.intoto.jsonl | LC_ALL=C sort)
FOUND=$(find . -mindepth 1 -maxdepth 1 -exec basename {} \; | LC_ALL=C sort)
test "$FOUND" = "$EXPECTED"
```

A pass puts the release's files in the folder `sablecourt-$VERSION` and shows that the folder holds the fourteen files the release workflow attaches and no others.

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
WANT=$(printf '%s\n' "sablecourt_${VERSION}_sbom.spdx.json" \
  "sablecourt_${VERSION}_"{darwin,linux}_{amd64,arm64}.tar.gz \
  "sablecourt_${VERSION}_image_"{sablecourt,sablecourt-agent}_linux_{amd64,arm64}.spdx.json | LC_ALL=C sort)
NAMES=$(cut -c 67- checksums.txt | LC_ALL=C sort)
test "$NAMES" = "$WANT"
```

A pass shows that each file listed in `checksums.txt` is in the folder with the SHA-256 listed for it, and that the list names the four archives and the five SBOMs of `$VERSION`, each once.

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

A pass shows that each archive and SBOM in the folder and `checksums.txt` has an SLSA build provenance attestation signed with a certificate for `release.yml` at the tag `v$VERSION`, with the source ref `refs/tags/v$VERSION`, the source repository digest `$SHA` and no self-hosted runner, once as fetched from GitHub's API and once from `sablecourt_${VERSION}_provenance.intoto.jsonl`.

The next three blocks check the daemon image. To check the agent image too, add them to the end of the file a second time, with `IMAGE=ghcr.io/shadewright-labs/sablecourt-agent` and the agent's digest.

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

A pass shows that, for linux/amd64 and for linux/arm64, the index lists one image, that image has SPDX attestations verified with a certificate for `release.yml` at the tag `v$VERSION`, and the predicate of one of them equals the release's SBOM file for that image, which holds one JSON value.

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

A pass shows that the image index has an SLSA build provenance attestation signed with a certificate for `release.yml` at the tag `v$VERSION`, with the source ref `refs/tags/v$VERSION`, the source repository digest `$SHA` and no self-hosted runner, once as fetched from GitHub's API and once from `sablecourt_${VERSION}_image_${IMAGE##*/}.intoto.jsonl`.

## What this does not prove

- Who created the tag `v$VERSION`. An active tag ruleset with a creation restriction that matches the tag limits who may create it, but `release.yml` does not check for one, and a ruleset does not show who created this tag.
- Which checks the release workflow ran. The commands check the certificate's identity, issuer, source ref and source repository digest; they do not read `release.yml` at `$SHA`.
- That the commit `$SHA` is on `main`.
- That an image tag such as `$VERSION` points at `$DIGEST`: deploy by digest.
- That the images have no known vulnerabilities.
