#!/usr/bin/env bash
# Adds the image digests of a release and the commands that verify the images and the binaries to the body of
# its GitHub release (the release-notes job of .github/workflows/release.yml). Usage:
#   hack/release-verify-notes.sh <owner/repo> <tag> <daemon image digest> <agent image digest> <commit sha> < body > new body
# The section sits between two HTML comment markers. A body that already has it gets it replaced, so a rerun
# of the job leaves exactly one copy; anything else in the body is kept as it is. A body whose markers are not
# exactly one start line followed by one end line is refused (exit 1) without output, so no rerun can pair a
# stray marker with a new one and delete the prose between them. The identities are pinned to
# the tag, so a signature of another tag or branch does not verify. Digests must be sha256: and 64 lower case
# hex digits; the repository, the tag and the digests are validated before they reach the text.
set -euo pipefail

if [ "$#" -ne 5 ]; then
  echo "usage: $0 <owner/repo> <tag> <daemon image digest> <agent image digest> <commit sha>" >&2
  exit 2
fi
repo="$1" tag="$2" daemon="$3" agent="$4" commit="$5"
printf '%s' "${commit}" | grep -Eq '^[0-9a-f]{40}$' || { echo "bad commit '${commit}'" >&2; exit 1; }

printf '%s' "${repo}" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || { echo "bad repository '${repo}'" >&2; exit 1; }
printf '%s' "${tag}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || { echo "bad tag '${tag}'" >&2; exit 1; }
for d in "${daemon}" "${agent}"; do
  printf '%s' "${d}" | grep -Eq '^sha256:[a-f0-9]{64}$' || { echo "not a digest: '${d}'" >&2; exit 1; }
done

owner="$(printf '%s' "${repo%%/*}" | tr '[:upper:]' '[:lower:]')"
repo_re="${repo//./\\.}"
tag_re="${tag//./\\.}"
version="${tag#v}"
start='<!-- sablecourt:verify:start -->'
end='<!-- sablecourt:verify:end -->'

pre=""
if [ "${version}" != "${version%%-*}" ]; then
  pre="This is a pre-release: it is not the latest release and its images are tagged \`${version}\` only (no \`latest\`, no \`X.Y\` tag). Pull them by digest as below.

"
fi

section="$(cat <<TEXT
${start}
## Artifacts and verification

${pre}| Artifact | Reference |
| --- | --- |
| Daemon image | \`ghcr.io/${owner}/sablecourt@${daemon}\` |
| Agent image | \`ghcr.io/${owner}/sablecourt-agent@${agent}\` |
| Binaries | \`sablecourt_${version}_<os>_<arch>.tar.gz\` (darwin and linux, amd64 and arm64), \`checksums.txt\`, \`checksums.txt.sigstore.json\` |

Before this section was written, the release workflow verified, for both images and against the Images workflow on this tag and commit (\`${commit}\`), the keyless cosign signature, the signed SPDX SBOM of each platform image (linux/amd64 and linux/arm64) and the GitHub build provenance attestation. \`checksums.txt\` lists every archive and is signed keyless; each archive has a build provenance attestation. Verify by digest, never by tag. You need [Docker](https://docs.docker.com/engine/install/), jq, [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 3.x or later and the [GitHub CLI](https://cli.github.com/). Details: [verify a release](https://github.com/${repo}/blob/main/docs/ci-and-releases.md#verify-a-release).

\`\`\`bash
ISSUER=https://token.actions.githubusercontent.com
IMAGES_IDENTITY='^https://github\.com/${repo_re}/\.github/workflows/images\.yml@refs/tags/${tag_re}\$'
RELEASE_IDENTITY='^https://github\.com/${repo_re}/\.github/workflows/release\.yml@refs/tags/${tag_re}\$'

for ref in \\
  ghcr.io/${owner}/sablecourt@${daemon} \\
  ghcr.io/${owner}/sablecourt-agent@${agent}; do
  cosign verify --certificate-identity-regexp "\$IMAGES_IDENTITY" --certificate-oidc-issuer "\$ISSUER" "\$ref"
  # One SBOM per platform image, attested on the digest of each (needs docker and jq).
  for arch in amd64 arm64; do
    child="\$(docker buildx imagetools inspect --raw "\$ref" | jq -r --arg a "\$arch" \\
      '.manifests[] | select(.platform.os == "linux" and .platform.architecture == \$a) | .digest')"
    cosign verify-attestation --type spdxjson --certificate-identity-regexp "\$IMAGES_IDENTITY" \\
      --certificate-oidc-issuer "\$ISSUER" "\${ref%@*}@\$child" > /dev/null
  done
  gh attestation verify "oci://\$ref" --repo ${repo} \\
    --signer-workflow ${repo}/.github/workflows/images.yml --source-ref refs/tags/${tag} --source-digest ${commit}
done

# Binaries: download the archives, checksums.txt and checksums.txt.sigstore.json of this release first.
gh release download ${tag} --repo ${repo}
cosign verify-blob --bundle checksums.txt.sigstore.json --certificate-identity-regexp "\$RELEASE_IDENTITY" \\
  --certificate-oidc-issuer "\$ISSUER" checksums.txt
sha256sum --check --strict checksums.txt
for archive in sablecourt_${version}_*.tar.gz; do
  gh attestation verify "\$archive" --repo ${repo} --signer-workflow ${repo}/.github/workflows/release.yml \\
    --source-ref refs/tags/${tag} --source-digest ${commit}
done
\`\`\`
${end}
TEXT
)"

body="$(cat)"
# The body may hold the section at most once, as two marker lines in order. Anything else (a lone marker, two
# sections, the end before the start, a marker inside a line of prose) is refused before anything is changed,
# because pairing such markers could drop prose that lies between them; fix the release body by hand.
if ! printf '%s\n' "${body}" | awk -v s="${start}" -v e="${end}" '
  { line = $0; sub(/\r$/, "", line) }
  index(line, s) || index(line, e) {
    if (line == s && !open && !done) { open = 1; next }
    if (line == e && open) { open = 0; done = 1; next }
    bad = 1
  }
  END { exit (bad || open) ? 1 : 0 }
'; then
  echo "the release body has unpaired, repeated or malformed ${start} / ${end} markers; fix the body by hand, then re-run" >&2
  exit 1
fi
# Drop the earlier copy of the section (markers included), then append the current one.
body="$(printf '%s\n' "${body}" | awk -v s="${start}" -v e="${end}" '
  { line = $0; sub(/\r$/, "", line) }
  !skip && line == s { skip = 1; next }
  skip && line == e { skip = 0; next }
  !skip { print }
')"
# Trim trailing blank lines.
body="$(printf '%s' "${body}" | sed -e :a -e '/^[[:space:]]*$/{$d;N;ba' -e '}')"
if [ -n "${body}" ]; then
  printf '%s\n\n%s\n' "${body}" "${section}"
else
  printf '%s\n' "${section}"
fi
