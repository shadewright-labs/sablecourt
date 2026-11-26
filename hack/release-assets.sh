#!/usr/bin/env bash
# Checks that the GitHub release of <tag> holds exactly the files of <dir>, byte for byte: the release lists the
# same asset names (none missing, none extra), and every asset, downloaded again, equals the local file. release.yml
# runs it on the draft before publishing and again right after. Exits 1 and names each difference otherwise.
# Usage: hack/release-assets.sh <tag> <dir>    (gh needs GH_REPO and GH_TOKEN)
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <tag> <dir>" >&2
  exit 2
fi
tag="$1" dir="$2"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

(cd "${dir}" && ls -A) | LC_ALL=C sort > "${work}/want"
gh release view "${tag}" --json assets --jq '.assets[].name' | LC_ALL=C sort > "${work}/listed"
mkdir "${work}/assets"
gh release download "${tag}" --dir "${work}/assets"
(cd "${work}/assets" && ls -A) | LC_ALL=C sort > "${work}/downloaded"

fails=0
if ! diff -u "${work}/want" "${work}/listed" > "${work}/diff"; then
  echo "release ${tag} does not list exactly the expected assets (- expected, + listed):" >&2
  cat "${work}/diff" >&2
  fails=1
fi
if ! diff -u "${work}/want" "${work}/downloaded" > "${work}/diff"; then
  echo "release ${tag} did not download exactly the expected assets (- expected, + downloaded):" >&2
  cat "${work}/diff" >&2
  fails=1
fi
while IFS= read -r name; do
  if [ -f "${work}/assets/${name}" ] && ! cmp -s "${dir}/${name}" "${work}/assets/${name}"; then
    echo "release ${tag}: ${name} differs from the verified file" >&2
    fails=1
  fi
done < "${work}/want"
if [ "${fails}" -ne 0 ]; then
  exit 1
fi
echo "release ${tag}: $(wc -l < "${work}/want" | tr -d ' ') assets, each equal to the verified file"
