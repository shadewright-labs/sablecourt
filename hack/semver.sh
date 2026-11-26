# shellcheck shell=bash
# Version helpers sourced by hack/floating-tags.sh and hack/check-release-tag.sh. Version parts are compared as
# decimal strings (no leading zeros: the callers' patterns refuse them), first by length and then lexically, so
# a part of any length compares correctly; shell arithmetic would overflow past 2^63 and compare
# v18446744073709551616.0.0 as smaller than v1.0.0.

# The pattern of one version part, and of a stable vX.Y.Z tag (parts in BASH_REMATCH[1..3]).
SEMVER_NUM='(0|[1-9][0-9]*)'
SEMVER_STABLE_TAG="^v${SEMVER_NUM}\\.${SEMVER_NUM}\\.${SEMVER_NUM}\$"

# semver_part_cmp a b: prints -1, 0 or 1.
semver_part_cmp() {
  if [ "${#1}" -lt "${#2}" ]; then echo -1; return; fi
  if [ "${#1}" -gt "${#2}" ]; then echo 1; return; fi
  if [[ "$1" < "$2" ]]; then echo -1; return; fi
  if [[ "$1" > "$2" ]]; then echo 1; return; fi
  echo 0
}

# semver_cmp a b: a and b are X.Y.Z; prints -1, 0 or 1.
semver_cmp() {
  local IFS=. a b i c
  read -r -a a <<< "$1"
  read -r -a b <<< "$2"
  for i in 0 1 2; do
    c="$(LC_ALL=C semver_part_cmp "${a[i]}" "${b[i]}")"
    if [ "${c}" != 0 ]; then echo "${c}"; return; fi
  done
  echo 0
}

# semver_newest_stable_tag: prints the X.Y.Z of the newest vX.Y.Z tag of the repository in the current
# directory, or nothing when there is none. Fails when git fails.
semver_newest_stable_tag() {
  local tags t best=""
  tags="$(git tag -l 'v*')" || return 1
  while IFS= read -r t; do
    [[ "${t}" =~ ${SEMVER_STABLE_TAG} ]] || continue
    t="${t#v}"
    if [ -z "${best}" ] || [ "$(semver_cmp "${t}" "${best}")" = 1 ]; then best="${t}"; fi
  done <<< "${tags}"
  printf '%s' "${best}"
}
