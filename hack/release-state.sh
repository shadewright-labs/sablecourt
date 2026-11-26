#!/usr/bin/env bash
# Reads the GitHub releases of <tag> for release.yml and fails closed: a failed or malformed lookup is an error,
# never "no release". Lists every release of the repository (gh api --paginate) and keeps those whose tag_name is
# <tag>. A token with contents: read sees published releases only; one with contents: write also sees drafts.
#   exit 0: no published release of <tag>; prints the id of each draft release of <tag>, one per line
#   exit 3: a published release of <tag> exists (prints nothing)
#   exit 1: the lookup failed or did not return at least one page that is a list of releases
# Usage: hack/release-state.sh <tag>    (gh needs GH_REPO and GH_TOKEN)
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <tag>" >&2
  exit 2
fi
tag="$1"
: "${GH_REPO:?GH_REPO is not set}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

if ! gh api --paginate "repos/${GH_REPO}/releases?per_page=100" > "${work}/pages"; then
  echo "could not list the releases of ${GH_REPO}; refusing to go on without knowing whether ${tag} is published" >&2
  exit 1
fi
# At least one page, and each page a JSON array of objects with a numeric id, a tag_name and a boolean draft. An
# empty answer (no page at all) is not an empty list: gh prints [] for a repository without releases.
if ! jq -rs --arg tag "${tag}" '
    if length == 0 then error("no page in the answer") else .[] end
    | if type != "array" then error("not a list of releases") else .[] end
    | if (.id | type) != "number" or (.tag_name | type) != "string" or (.draft | type) != "boolean"
      then error("a release without id, tag_name or draft") else . end
    | select(.tag_name == $tag) | "\(.id) \(.draft)"' "${work}/pages" > "${work}/found"; then
  echo "the release list of ${GH_REPO} is not what the GitHub API returns; refusing to go on" >&2
  exit 1
fi
if grep -q ' false$' "${work}/found"; then
  echo "release ${tag} is already published; this run changes nothing" >&2
  exit 3
fi
sed -n 's/ true$//p' "${work}/found"
