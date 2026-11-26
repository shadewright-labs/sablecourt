# Verify a release

Check that a Sablecourt archive or image was built and signed by this repository's release workflow for a given release tag, and that its bytes have not changed since.

No release existed when this page was written. The commands below apply to releases made by the signed release workflow described in [CI, Releases](../ci.md#releases). Before a release becomes public, that workflow runs the same kinds of checks on the files of its run, pinned to its own tag and commit, plus a few more.

## What is signed

Each release has these files:

- four archives, `sablecourt_<version>_<os>_<arch>.tar.gz` for darwin and linux on amd64 and arm64, each holding `sablecourtd` and `sablectl`;
- five SPDX SBOMs: `sablecourt_<version>_sbom.spdx.json` for the eight binaries, and `sablecourt_<version>_image_<image>_linux_<arch>.spdx.json` for each platform image of the two images;
- `checksums.txt`, the sha256 of the four archives and the five SBOMs, and `checksums.txt.sigstore.json`, a cosign keyless signature bundle of `checksums.txt`;
- `sablecourt_<version>_provenance.intoto.jsonl`, a GitHub build provenance attestation for the archives, the SBOMs and `checksums.txt`, and `sablecourt_<version>_image_<image>.intoto.jsonl`, the provenance of each image index.

The images `ghcr.io/shadewright-labs/sablecourt` and `ghcr.io/shadewright-labs/sablecourt-agent` are built for linux/amd64 and linux/arm64. Each image index has a cosign keyless signature and a build provenance attestation in the registry, and each platform image has its SPDX SBOM attached as a signed attestation. The release notes list both index digests.

Keyless means the signing certificate was issued to the workflow run through GitHub's OIDC token, so you need no Sablecourt public key. cosign and gh check that short-lived certificate against the Sigstore and GitHub trust roots. The certificate names the repository, the workflow file and the tag it ran for, and the commands below require that exact name.

## Prerequisites

- [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 3.x. The workflow uses 3.1.4; cosign 3 reads the Sigstore bundle format of the image signatures by default, and older versions were not tried.
- The [GitHub CLI](https://cli.github.com/) (`gh`), logged in, with `gh attestation verify --cert-identity` and `--deny-self-hosted-runners` (gh 2.105.0 was used for this page).
- For images: [Docker](https://docs.docker.com/engine/install/) with Buildx (for `docker buildx imagetools`) and [jq](https://jqlang.org/). If GHCR asks for credentials, log in with `docker login ghcr.io`.
- Bash. Run the blocks below after `set -euo pipefail`, so that a failed command in a loop or a pipe stops the block instead of being hidden.

Set these once:

```bash
set -euo pipefail
VERSION=0.1.0    # the release without its leading v, for example 0.1.0 or 0.2.0-rc.1
IDENTITY="https://github.com/shadewright-labs/sablecourt/.github/workflows/release.yml@refs/tags/v$VERSION"
ISSUER=https://token.actions.githubusercontent.com
```

`IDENTITY` is the exact certificate identity of `release.yml` on the tag `v$VERSION`: no other workflow, repository or tag matches it.

## Verify an archive

```bash
mkdir "sablecourt-$VERSION" && cd "sablecourt-$VERSION"
gh release download "v$VERSION" --repo shadewright-labs/sablecourt

# 1. checksums.txt was signed by release.yml on the tag v$VERSION.
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" \
  checksums.txt

# 2. The archives and the SBOMs match checksums.txt (on macOS: shasum -a 256 --check checksums.txt).
sha256sum --check --strict checksums.txt

# 3. Each file has build provenance from release.yml on this tag, made on a GitHub-hosted runner.
for FILE in sablecourt_*.tar.gz sablecourt_*.spdx.json checksums.txt; do
  gh attestation verify "$FILE" \
    --repo shadewright-labs/sablecourt \
    --cert-identity "$IDENTITY" \
    --cert-oidc-issuer "$ISSUER" \
    --source-ref "refs/tags/v$VERSION" \
    --deny-self-hosted-runners
done
```

To check the provenance against the release's own file instead of GitHub's attestation API, add `--bundle "sablecourt_${VERSION}_provenance.intoto.jsonl"` to the `gh attestation verify` command.

A good result: every command exits 0. `cosign verify-blob` prints `Verified OK`, `sha256sum` prints `OK` after each of the nine file names, and each `gh attestation verify` prints `Verification succeeded!` followed by the attestations it matched and the workflow that signed them. If a check fails, do not use the file. A failure can also come from the network, a login or the tool; fix that and run the check again, because only a check that passes tells you something about the file.

## Check that the commit is on main

The release workflow refuses to build a commit that is not on main, but that check lives in the tagged commit's own `release.yml` (see [What the checks prove](#what-the-checks-prove-and-what-they-do-not)). To check it yourself, read the source commit from the provenance and ask GitHub whether main contains it:

```bash
SHA=$(gh attestation verify checksums.txt \
  --repo shadewright-labs/sablecourt \
  --cert-identity "$IDENTITY" \
  --cert-oidc-issuer "$ISSUER" \
  --source-ref "refs/tags/v$VERSION" \
  --deny-self-hosted-runners \
  --format json --jq '.[0].verificationResult.signature.certificate.sourceRepositoryDigest')
gh api "repos/shadewright-labs/sablecourt/compare/main...$SHA" --jq .status
```

`identical` or `behind` means the commit is on main. `ahead` or `diverged` means it is not: do not use the release. In a clone, `git merge-base --is-ancestor "$SHA" origin/main` answers the same question. Pass `--source-digest "$SHA"` to later `gh attestation verify` calls to require that commit.

## Verify an image

Verify by digest, never by tag: a tag can be moved, a digest cannot. Take the digest from the release notes, and check that the version tag points at it:

```bash
IMAGE=ghcr.io/shadewright-labs/sablecourt    # or ghcr.io/shadewright-labs/sablecourt-agent
DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000    # replace with the digest from the release notes
test "$(docker buildx imagetools inspect "$IMAGE:$VERSION" --format '{{json .Manifest}}' | jq -r .digest)" = "$DIGEST"

# 1. The image index was signed by release.yml on the tag v$VERSION.
cosign verify \
  --certificate-identity "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" \
  "$IMAGE@$DIGEST" > /dev/null

# 2. Each platform image carries a signed SPDX SBOM; this writes it to sbom-<arch>.spdx.json.
for ARCH in amd64 arm64; do
  CHILD=$(docker buildx imagetools inspect --raw "$IMAGE@$DIGEST" | jq -er --arg a "$ARCH" \
    '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $a) | .digest] | if length == 1 then .[0] else error("not one \($a) image") end')
  cosign verify-attestation --type spdxjson \
    --certificate-identity "$IDENTITY" \
    --certificate-oidc-issuer "$ISSUER" \
    "$IMAGE@$CHILD" > "attestations-$ARCH.json"
  jq -se 'map(.payload | @base64d | fromjson | .predicate) | .[0]' "attestations-$ARCH.json" > "sbom-$ARCH.spdx.json"
  jq -e .spdxVersion "sbom-$ARCH.spdx.json"
done

# 3. The index has build provenance from release.yml on this tag, made on a GitHub-hosted runner.
gh attestation verify "oci://$IMAGE@$DIGEST" \
  --repo shadewright-labs/sablecourt \
  --cert-identity "$IDENTITY" \
  --cert-oidc-issuer "$ISSUER" \
  --source-ref "refs/tags/v$VERSION" \
  --deny-self-hosted-runners
```

A good result: every command exits 0, the `test` line included. `cosign verify` and `cosign verify-attestation` report the image reference and the checks they made on standard error; the verified payload they write on standard output is kept in `attestations-<arch>.json` or discarded. `jq -e .spdxVersion` prints the SPDX version of each SBOM. `gh attestation verify` prints `Verification succeeded!` and details. Then deploy `$IMAGE@$DIGEST`, not the tag. The same SBOMs are release files, `sablecourt_<version>_image_<image>_linux_<arch>.spdx.json`, covered by `checksums.txt`.

## What the checks prove and what they do not

What you learn: the repository (`shadewright-labs/sablecourt`), the workflow path (`.github/workflows/release.yml`), the tag ref (`refs/tags/v$VERSION`) and, from the provenance, the source commit digest. cosign checks the first three for `checksums.txt` and the image indexes; an archive or an SBOM is covered through its hash in the signed `checksums.txt`. `gh attestation verify` checks the same identity for the provenance of each file and image index, and records the commit, which `--source-digest` can require.

Only the `gh attestation verify` commands with `--deny-self-hosted-runners` prove that the signing run used a GitHub-hosted runner. The cosign commands and `sha256sum` do not check the runner. The workflow's own `gh attestation verify` calls pass the flag too.

What you do not learn: who pushed the tag, and what that run's `release.yml` checked. GitHub runs the workflow file of the tagged commit, so a tag placed on a commit whose `release.yml` lacks the main-branch check, the scan or the verification still produces files signed as `release.yml@refs/tags/v$VERSION`. The trust anchor is the repository's tag ruleset, which must limit who may create `v*` tags; it is a repository setting, and no file in this repository shows or enforces it. The step in [Check that the commit is on main](#check-that-the-commit-is-on-main) is the part you can check yourself.
