#!/usr/bin/env bash
# Refuses a release tag that the release process did not produce, as far as the checked-out tree can tell. Run
# by the gate jobs of images.yml and release.yml on every v* tag, after hack/require-on-main.sh. Usage:
#   hack/check-release-tag.sh <tag, with v> [manifest file]     (default .release-please-manifest.json)
# Accepted:
#   vX.Y.Z        a stable release: X.Y.Z must equal the version of "." in the manifest at the tagged commit,
#                 which only the merged release pull request writes.
#   vX.Y.Z-rc.N   a release candidate: X.Y.Z must be greater than the manifest's version (or the manifest has
#                 no version yet), so a candidate is always for a release that has not been made.
# Anything else (another pre-release form, build metadata, leading zeros) is refused. The workflows cannot stop
# someone allowed to push tags from tagging other code; the repository ruleset that limits v* tags is described
# in docs/ci-and-releases.md. Tests: hack/test/release-scripts_test.sh.
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 <tag> [manifest file]" >&2
  exit 2
fi
tag="$1" manifest="${2:-.release-please-manifest.json}"
num='(0|[1-9][0-9]*)'
if ! [[ "${tag}" =~ ^v${num}\.${num}\.${num}(-rc\.([1-9][0-9]*))?$ ]]; then
  echo "refusing ${tag}: release tags are vX.Y.Z or vX.Y.Z-rc.N" >&2
  exit 1
fi
version="${tag#v}" core="${tag#v}"
core="${core%%-*}"
current="$(jq -r '.["."] // empty' "${manifest}")" || { echo "cannot read ${manifest}" >&2; exit 1; }

# ver_gt a b: true when version a (X.Y.Z) is greater than b.
ver_gt() {
  local IFS=. a b i
  read -r -a a <<< "$1"
  read -r -a b <<< "$2"
  for i in 0 1 2; do
    if (( 10#${a[i]} > 10#${b[i]} )); then return 0; fi
    if (( 10#${a[i]} < 10#${b[i]} )); then return 1; fi
  done
  return 1
}

if [ "${version}" = "${core}" ]; then
  if [ "${current}" != "${version}" ]; then
    echo "refusing ${tag}: the manifest at this commit says '${current:-no version}'; a stable tag comes only from merging the release pull request" >&2
    exit 1
  fi
  echo "${tag}: stable release, matches the manifest"
else
  if [ -n "${current}" ] && ! ver_gt "${core}" "${current}"; then
    echo "refusing ${tag}: ${core} is not newer than the released ${current}" >&2
    exit 1
  fi
  echo "${tag}: release candidate for ${core} (manifest: ${current:-no version yet})"
fi
