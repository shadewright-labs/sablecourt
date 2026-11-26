#!/usr/bin/env bash
# Uploads the release files of a tag to its draft GitHub release (the upload job of release.yml): the four
# archives, checksums.txt and checksums.txt.sigstore.json. Waits for the draft (release-please creates it right
# after the tag), then uploads with --clobber, so a rerun replaces the files of an earlier attempt. The release
# is read before the first attempt and again before every retry, and anything but a draft (published, a state
# that is not a boolean, or an error other than "release not found") stops the job before it writes: --clobber
# must never replace an asset of a published release. Usage:
#   hack/upload-release-assets.sh <owner/repo> <tag> <local dist dir>     (needs gh and GH_TOKEN)
# ATTEMPTS (20) and POLL_SECONDS (15) bound the waiting. Tests: hack/test/release-scripts_test.sh (with a gh stub).
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <owner/repo> <tag> <local dist dir>" >&2
  exit 2
fi
repo="$1" tag="$2" dist="$3"
attempts="${ATTEMPTS:-20}" poll="${POLL_SECONDS:-15}"
version="${tag#v}"
files=()
for os in linux darwin; do
  for arch in amd64 arm64; do
    files+=("${dist}/sablecourt_${version}_${os}_${arch}.tar.gz")
  done
done
files+=("${dist}/checksums.txt" "${dist}/checksums.txt.sigstore.json")
for f in "${files[@]}"; do
  [ -s "${f}" ] || { echo "missing local file ${f}" >&2; exit 1; }
done
err="$(mktemp "${TMPDIR:-/tmp}/upload-release.XXXXXX")"
trap 'rm -f "${err}"' EXIT

# draft_state: prints draft, missing, or fails (exit 1) for a published release or any other error.
draft_state() {
  local state
  if state="$(gh release view "${tag}" --repo "${repo}" --json isDraft --jq '.isDraft | if type == "boolean" then tostring else "invalid" end' 2> "${err}")"; then
    case "${state}" in
      true) echo draft; return 0 ;;
      false) echo "release ${tag} is already published; refusing to upload" >&2; return 1 ;;
      *) echo "release ${tag} has no boolean draft state; refusing to upload" >&2; return 1 ;;
    esac
  fi
  if [ "$(tr -d '\r' < "${err}" | sed -e '/^[[:space:]]*$/d')" = "release not found" ]; then
    echo missing
    return 0
  fi
  cat "${err}" >&2
  echo "cannot read the release of ${tag}; refusing to upload" >&2
  return 1
}

for i in $(seq 1 "${attempts}"); do
  state="$(draft_state)" || exit 1
  if [ "${state}" = draft ]; then
    if gh release upload "${tag}" --repo "${repo}" --clobber "${files[@]}" 2> "${err}"; then
      echo "release ${tag}: archives, checksums.txt and its signature uploaded"
      exit 0
    fi
    echo "could not upload to the release ${tag} yet ($(tail -n 1 "${err}")), attempt ${i} of ${attempts}" >&2
  else
    echo "no draft release for ${tag} yet, attempt ${i} of ${attempts}" >&2
  fi
  if [ "${i}" -lt "${attempts}" ]; then sleep "${poll}"; fi
done
echo "no upload to the release ${tag} after ${attempts} attempts (last state: ${state}); check the draft and re-run this job" >&2
exit 1
