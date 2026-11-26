#!/usr/bin/env bash
# Refuses a release tag that the release process did not produce, as far as the checked-out tree can tell. Run
# by the gate jobs of images.yml and release.yml on every v* tag, after hack/require-on-main.sh. Usage:
#   hack/check-release-tag.sh <tag, with v> [manifest file]     (default .release-please-manifest.json)
# The manifest must be a JSON object that is either empty ({}: no release yet, the one bootstrap case) or holds
# exactly the key "." with a version X.Y.Z (no leading zeros). An empty file, null, another type, other keys or
# a malformed version are refused. Accepted tags:
#   vX.Y.Z        a stable release: X.Y.Z must equal the manifest's version at the tagged commit, which only the
#                 merged release pull request writes.
#   vX.Y.Z-rc.N   a release candidate: X.Y.Z must be newer than the manifest's version (if any) and newer than
#                 the newest vX.Y.Z tag of the repository now (`git tag -l`, so the checkout needs the tags), so
#                 a candidate is always for a release that has not been made, even on an old commit.
# Anything else (another pre-release form, build metadata, leading zeros) is refused. This is a second line of
# defence: the tag at an old commit runs that commit's workflow, which may not run this script at all, so the
# trust anchor is the repository's tag rulesets (docs/ci-and-releases.md). Tests: hack/test/release-scripts_test.sh.
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 <tag> [manifest file]" >&2
  exit 2
fi
# shellcheck source=hack/semver.sh
. "$(dirname "$0")/semver.sh"
tag="$1" manifest="${2:-.release-please-manifest.json}"
if ! [[ "${tag}" =~ ^v${SEMVER_NUM}\.${SEMVER_NUM}\.${SEMVER_NUM}(-rc\.([1-9][0-9]*))?$ ]]; then
  echo "refusing ${tag}: release tags are vX.Y.Z or vX.Y.Z-rc.N" >&2
  exit 1
fi
version="${tag#v}" core="${tag#v}"
core="${core%%-*}"

# The manifest: {} or {".": "X.Y.Z"}, nothing else.
[ -s "${manifest}" ] || { echo "refusing ${tag}: ${manifest} is missing or empty" >&2; exit 1; }
if ! shape="$(jq -r '
    if type != "object" then "bad"
    elif (keys | length) == 0 then "empty"
    elif keys == ["."] and (.["."] | type) == "string" then "version"
    else "bad" end' "${manifest}")"; then
  echo "refusing ${tag}: ${manifest} is not valid JSON" >&2
  exit 1
fi
current=""
case "${shape}" in
  empty) ;;
  version)
    current="$(jq -r '.["."]' "${manifest}")"
    if ! [[ "${current}" =~ ^${SEMVER_NUM}\.${SEMVER_NUM}\.${SEMVER_NUM}$ ]]; then
      echo "refusing ${tag}: ${manifest} has the malformed version '${current}'" >&2
      exit 1
    fi
    ;;
  *)
    echo "refusing ${tag}: ${manifest} must be {} or {\".\": \"X.Y.Z\"}" >&2
    exit 1
    ;;
esac

if [ "${version}" = "${core}" ]; then
  if [ "${current}" != "${version}" ]; then
    echo "refusing ${tag}: the manifest at this commit says '${current:-no version}'; a stable tag comes only from merging the release pull request" >&2
    exit 1
  fi
  echo "${tag}: stable release, matches the manifest"
  exit 0
fi

if [ -n "${current}" ] && [ "$(semver_cmp "${core}" "${current}")" != 1 ]; then
  echo "refusing ${tag}: ${core} is not newer than ${current} in the manifest at this commit" >&2
  exit 1
fi
newest="$(semver_newest_stable_tag)" || { echo "refusing ${tag}: cannot list the tags" >&2; exit 1; }
if [ -n "${newest}" ] && [ "$(semver_cmp "${core}" "${newest}")" != 1 ]; then
  echo "refusing ${tag}: ${core} is not newer than the released v${newest}" >&2
  exit 1
fi
echo "${tag}: release candidate for ${core} (manifest: ${current:-no version yet}, newest release: ${newest:-none})"
