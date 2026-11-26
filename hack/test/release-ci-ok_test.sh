#!/usr/bin/env bash
# Table tests of hack/release-ci-ok.sh: a release needs a passed ci-ok from a CI run on main for its commit, and
# every other answer (none, pending, failed, from a pull request branch, unreadable) refuses it.
# Usage: hack/test/release-ci-ok_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/release-ci-ok.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0
sha=0123456789abcdef0123456789abcdef01234567
other=fedcba9876543210fedcba9876543210fedcba98

# run_ <suite id> <name> <status> <conclusion> [app] [sha] [run id]: one check run as JSON
run_() {
  printf '{"id":%s,"name":"%s","status":"%s","conclusion":%s,"head_sha":"%s","app":{"slug":"%s"},"check_suite":{"id":%s}}' \
    "${7:-100}" "$2" "$3" "$4" "${6:-${sha}}" "${5:-github-actions}" "$1"
}
suite_() { # <id> <head_branch> [app] [sha]
  printf '{"id":%s,"head_branch":"%s","head_sha":"%s","app":{"slug":"%s"}}' "$1" "$2" "${4:-${sha}}" "${3:-github-actions}"
}
main_suites="{\"total_count\":2,\"check_suites\":[$(suite_ 1 main),$(suite_ 2 feature)]}"

# <name> <runs JSON> <suites JSON> <wanted exit status>
case_() {
  local name="$1" rc=0
  printf '%s' "$2" > "${work}/runs.json"
  printf '%s' "$3" > "${work}/suites.json"
  "${script}" "${sha}" "${work}/runs.json" "${work}/suites.json" > /dev/null 2>&1 || rc=$?
  if [ "${rc}" = "$4" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got exit ${rc}, want $4"
    fails=$((fails + 1))
  fi
}
runs() { local IFS=,; printf '{"total_count":%s,"check_runs":[%s]}' "$#" "$*"; }

case_ "a passed ci-ok on main passes" "$(runs "$(run_ 1 ci-ok completed '"success"')")" "${main_suites}" 0
case_ "no check run refuses" "$(runs)" "${main_suites}" 1
case_ "a queued ci-ok refuses" "$(runs "$(run_ 1 ci-ok queued null)")" "${main_suites}" 1
case_ "an in-progress ci-ok refuses" "$(runs "$(run_ 1 ci-ok in_progress null)")" "${main_suites}" 1
case_ "a failed ci-ok refuses" "$(runs "$(run_ 1 ci-ok completed '"failure"')")" "${main_suites}" 1
case_ "a cancelled ci-ok refuses" "$(runs "$(run_ 1 ci-ok completed '"cancelled"')")" "${main_suites}" 1
case_ "a skipped ci-ok refuses" "$(runs "$(run_ 1 ci-ok completed '"skipped"')")" "${main_suites}" 1
case_ "a passed ci-ok from a pull request branch only refuses" \
  "$(runs "$(run_ 2 ci-ok completed '"success"')")" "${main_suites}" 1
case_ "a passed check with another name refuses" "$(runs "$(run_ 1 ci-ok-x completed '"success"')")" "${main_suites}" 1
case_ "a passed ci-ok from another app refuses" \
  "$(runs "$(run_ 1 ci-ok completed '"success"' other-app)")" "${main_suites}" 1
case_ "a passed ci-ok in a main suite of another app refuses" \
  "$(runs "$(run_ 1 ci-ok completed '"success"')")" "{\"total_count\":1,\"check_suites\":[$(suite_ 1 main other-app)]}" 1
case_ "a passed ci-ok for another commit refuses" \
  "$(runs "$(run_ 1 ci-ok completed '"success"' github-actions "${other}")")" "${main_suites}" 1
case_ "a main suite of another commit refuses" \
  "$(runs "$(run_ 1 ci-ok completed '"success"')")" \
  "{\"total_count\":1,\"check_suites\":[$(suite_ 1 main github-actions "${other}")]}" 1
case_ "the newest run decides: a rerun that passed after a failure passes" \
  "$(runs "$(run_ 1 ci-ok completed '"failure"' github-actions "${sha}" 100)" "$(run_ 1 ci-ok completed '"success"' github-actions "${sha}" 101)")" \
  "${main_suites}" 0
case_ "the newest run decides: a pending rerun after a pass refuses" \
  "$(runs "$(run_ 1 ci-ok completed '"success"' github-actions "${sha}" 100)" "$(run_ 1 ci-ok in_progress null github-actions "${sha}" 101)")" \
  "${main_suites}" 1
case_ "a list cut short by paging refuses" \
  "{\"total_count\":101,\"check_runs\":[$(run_ 1 ci-ok completed '"success"')]}" "${main_suites}" 1
case_ "suites cut short by paging refuse" \
  "$(runs "$(run_ 1 ci-ok completed '"success"')")" "{\"total_count\":200,\"check_suites\":[$(suite_ 1 main)]}" 1
case_ "an error answer refuses" '{"message":"Resource not accessible by integration"}' "${main_suites}" 1
case_ "an answer that is not JSON refuses" "<html>" "${main_suites}" 1
case_ "an empty answer refuses" "" "${main_suites}" 1

rc=0
"${script}" HEAD "${work}/runs.json" "${work}/suites.json" > /dev/null 2>&1 || rc=$?
if [ "${rc}" = 2 ]; then echo "ok   a ref that is not a full sha is refused"; else echo "FAIL a ref that is not a full sha is refused"; fails=$((fails + 1)); fi
rc=0
"${script}" "${sha}" > /dev/null 2>&1 || rc=$?
if [ "${rc}" = 2 ]; then echo "ok   a missing argument is a usage error"; else echo "FAIL a missing argument is a usage error"; fails=$((fails + 1)); fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
