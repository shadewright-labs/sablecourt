#!/usr/bin/env bash
# Fails when a tracked file names an image under ghcr.io/shadewright-labs/ that release.yml does not publish, or
# a tag form it never pushes. Published on a v* tag: ghcr.io/shadewright-labs/sablecourt and
# ghcr.io/shadewright-labs/sablecourt-agent, tagged X.Y.Z or X.Y.Z-rc.N (the Git tag without its v) and latest,
# or referenced by digest (@sha256: and exactly 64 lower-case hex digits; a short, upper-case or
# other-algorithm digest, an empty tag or a trailing colon fail). Run by repo-checks.yml. Usage: hack/check-image-refs.sh [repository root]
# Tests: hack/test/check-image-refs_test.sh.
set -euo pipefail

cd "${1:-.}"
hits="$(mktemp "${TMPDIR:-/tmp}/image-refs.XXXXXX")"
trap 'rm -f "${hits}"' EXIT
rc=0
git grep -nIoE 'ghcr\.io/shadewright-labs/[A-Za-z0-9._/-]+([:@][A-Za-z0-9._:-]*)?' -- . \
  ':(exclude)hack/check-image-refs.sh' ':(exclude)hack/test/check-image-refs_test.sh' > "${hits}" || rc=$?
if [ "${rc}" -gt 1 ]; then
  echo "git grep failed (exit ${rc})" >&2
  exit 1
fi
name_re='^ghcr\.io/shadewright-labs/(sablecourt|sablecourt-agent)$'
tag_re='^(latest|(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?)$'
bad=0
while IFS= read -r line; do
  where="${line%:ghcr.io/*}" ref="ghcr.io/${line#*:ghcr.io/}"
  name="${ref%%[:@]*}" rest="${ref#"${name}"}"
  if ! [[ "${name}" =~ ${name_re} ]]; then
    echo "${where}: ${ref}: not a published image (sablecourt, sablecourt-agent)" >&2
    bad=$((bad + 1))
    continue
  fi
  case "${rest}" in
    "") ;;
    @*)
      if ! [[ "${rest}" =~ ^@sha256:[a-f0-9]{64}$ ]]; then
        echo "${where}: ${ref}: not a sha256 digest (@sha256: and 64 lower-case hex digits)" >&2
        bad=$((bad + 1))
      fi
      ;;
    :*)
      if ! [[ "${rest#:}" =~ ${tag_re} ]]; then
        echo "${where}: ${ref}: release.yml never pushes the tag '${rest#:}'" >&2
        bad=$((bad + 1))
      fi
      ;;
  esac
done < "${hits}"
if [ "${bad}" -ne 0 ]; then
  echo "${bad} image reference(s) to fix" >&2
  exit 1
fi
echo "image references: ok"
