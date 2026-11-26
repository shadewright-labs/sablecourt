#!/usr/bin/env bash
# Decides whether a release may move `latest` (the image tag and the release's latest flag), for release.yml.
# Prints true only when <version> is a full release (X.Y.Z, not X.Y.Z-rc.N) and no existing full release tag is
# higher by semver (major, minor, patch compared as numbers, so 0.10.0 is above 0.9.0). Prints false otherwise.
# The tags come on stdin, one per line, as `git tag` or the matching-refs API lists them (vX.Y.Z, refs/tags/
# stripped). Tags that are not vX.Y.Z or vX.Y.Z-rc.N are ignored; release candidates never count as higher.
# The tag of <version> itself must be in the list: a list without it is incomplete, and the script fails.
# Usage: hack/release-latest.sh <version> < tags
#   exit 0 with true or false on stdout; 1 when the list does not hold v<version>; 2 on a bad argument.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <version> < tags" >&2
  exit 2
fi
version="$1"
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$'
if ! [[ "${version}" =~ ${semver} ]]; then
  echo "version '${version}' is not X.Y.Z or X.Y.Z-rc.N" >&2
  exit 2
fi
mine=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
prerelease="${BASH_REMATCH[4]}"

# Numeric a > b for digit strings without leading zeros, compared by length then text, so no number overflows.
num_gt() { # <a> <b>
  [ "${#1}" -gt "${#2}" ] || { [ "${#1}" -eq "${#2}" ] && [[ "$1" > "$2" ]]; }
}

found=false higher=""
while IFS= read -r tag || [ -n "${tag}" ]; do
  tag="${tag%$'\r'}"
  [ "${tag}" = "v${version}" ] && found=true
  [[ "${tag}" == v* ]] || continue
  [[ "${tag#v}" =~ ${semver} ]] || continue
  [ -z "${BASH_REMATCH[4]}" ] || continue
  t=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
  for i in 0 1 2; do
    if num_gt "${t[i]}" "${mine[i]}"; then
      higher="${tag}"
      break
    fi
    [ "${t[i]}" = "${mine[i]}" ] || break
  done
done

if [ "${found}" != true ]; then
  echo "the tag list does not hold v${version}; refusing to decide on an incomplete list" >&2
  exit 1
fi
if [ -n "${prerelease}" ]; then
  echo "v${version} is a release candidate: latest stays where it is" >&2
  echo false
elif [ -n "${higher}" ]; then
  echo "${higher} is a higher full release: latest stays where it is" >&2
  echo false
else
  echo true
fi
