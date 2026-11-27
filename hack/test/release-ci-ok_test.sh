#!/usr/bin/env bash
# Table tests of hack/release-ci-ok.sh with recorded API answers: a release needs the newest push run of
# .github/workflows/ci.yml on main of this repository for its commit to be completed with success, and the newest
# ci-ok check run of that run's check suite to be completed with success. Every other answer (a fork pull request
# from a branch named main, another branch, another event or workflow, pending, failed, cut short by paging,
# unreadable) refuses it.
# Usage: hack/test/release-ci-ok_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/release-ci-ok.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0
repo=example-org/sablecourt
sha=0123456789abcdef0123456789abcdef01234567
other=fedcba9876543210fedcba9876543210fedcba98

# wrun_ <run id> <check suite id> <status> <conclusion> [event] [head_branch] [path] [head repository] [sha] [repository]:
# one workflow run as GET repos/<repo>/actions/runs returns it (the fields the script reads).
wrun_() {
  printf '{"id":%s,"check_suite_id":%s,"status":"%s","conclusion":%s,"event":"%s","head_branch":"%s","path":"%s","head_sha":"%s","head_repository":{"full_name":"%s"},"repository":{"full_name":"%s"}}' \
    "$1" "$2" "$3" "$4" "${5:-push}" "${6:-main}" "${7:-.github/workflows/ci.yml}" "${9:-${sha}}" "${8:-${repo}}" "${10:-${repo}}"
}
# crun_ <check run id> <check suite id> <status> <conclusion> [name] [app] [sha]: one check run.
crun_() {
  printf '{"id":%s,"name":"%s","status":"%s","conclusion":%s,"head_sha":"%s","app":{"slug":"%s"},"check_suite":{"id":%s}}' \
    "$1" "${5:-ci-ok}" "$3" "$4" "${7:-${sha}}" "${6:-github-actions}" "$2"
}
wruns() { local IFS=,; printf '{"total_count":%s,"workflow_runs":[%s]}' "$#" "$*"; }
cruns() { local IFS=,; printf '{"total_count":%s,"check_runs":[%s]}' "$#" "$*"; }
S='"success"'

# <name> <workflow runs JSON> <check runs JSON> <wanted exit status>
case_() {
  local name="$1" rc=0
  printf '%s' "$2" > "${work}/wruns.json"
  printf '%s' "$3" > "${work}/cruns.json"
  "${script}" "${repo}" "${sha}" "${work}/wruns.json" "${work}/cruns.json" > /dev/null 2>&1 || rc=$?
  if [ "${rc}" = "$4" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got exit ${rc}, want $4"
    fails=$((fails + 1))
  fi
}

ok_run="$(wruns "$(wrun_ 10 11 completed "$S")")"
ok_check="$(cruns "$(crun_ 100 11 completed "$S")")"

case_ "a passed push run of ci.yml on main with a passed ci-ok passes" "${ok_run}" "${ok_check}" 0
# Recorded shape of a pull request from a fork whose branch is called main: GitHub reports head_branch main.
case_ "a fork pull request from a branch named main refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S" pull_request main .github/workflows/ci.yml someone/sablecourt)")" "${ok_check}" 1
case_ "a pull request run from this repository's main branch refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S" pull_request main)")" "${ok_check}" 1
case_ "a push run whose head repository is another repository refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S" push main .github/workflows/ci.yml someone/sablecourt)")" "${ok_check}" 1
case_ "a run listed for another repository refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S" push main .github/workflows/ci.yml "${repo}" "${sha}" someone/sablecourt)")" "${ok_check}" 1
case_ "a push run on another branch refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S" push feature)")" "${ok_check}" 1
case_ "a workflow_dispatch run on main refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S" workflow_dispatch)")" "${ok_check}" 1
case_ "a push run of another workflow with a ci-ok job refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S" push main .github/workflows/other.yml)")" "${ok_check}" 1
case_ "a push run for another commit refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S" push main .github/workflows/ci.yml "${repo}" "${other}")")" "${ok_check}" 1
case_ "no run refuses" "$(wruns)" "${ok_check}" 1
case_ "a pending run refuses" \
  "$(wruns "$(wrun_ 10 11 in_progress null)")" "$(cruns "$(crun_ 100 11 in_progress null)")" 1
case_ "a queued run refuses even with a passed ci-ok" "$(wruns "$(wrun_ 10 11 queued null)")" "${ok_check}" 1
case_ "a failed run refuses even with a passed ci-ok" "$(wruns "$(wrun_ 10 11 completed '"failure"')")" "${ok_check}" 1
case_ "a cancelled run refuses" "$(wruns "$(wrun_ 10 11 completed '"cancelled"')")" "${ok_check}" 1
case_ "a passed run with no ci-ok check run refuses" "${ok_run}" "$(cruns)" 1
case_ "a passed ci-ok in another check suite refuses" "${ok_run}" "$(cruns "$(crun_ 100 12 completed "$S")")" 1
case_ "a failed ci-ok refuses" "${ok_run}" "$(cruns "$(crun_ 100 11 completed '"failure"')")" 1
case_ "a skipped ci-ok refuses" "${ok_run}" "$(cruns "$(crun_ 100 11 completed '"skipped"')")" 1
case_ "a pending ci-ok refuses" "${ok_run}" "$(cruns "$(crun_ 100 11 in_progress null)")" 1
case_ "a check with another name refuses" "${ok_run}" "$(cruns "$(crun_ 100 11 completed "$S" ci-ok-x)")" 1
case_ "a ci-ok from another app refuses" "${ok_run}" "$(cruns "$(crun_ 100 11 completed "$S" ci-ok other-app)")" 1
case_ "a ci-ok for another commit refuses" \
  "${ok_run}" "$(cruns "$(crun_ 100 11 completed "$S" ci-ok github-actions "${other}")")" 1
# Two push runs of ci.yml on main for one commit (main pushed again to the same commit): the newest run decides,
# as it is the latest word on that commit. A release does not pick the run it likes.
case_ "two runs: a newer failed run wins over an older success and refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S")" "$(wrun_ 20 21 completed '"failure"')")" \
  "$(cruns "$(crun_ 100 11 completed "$S")" "$(crun_ 200 21 completed '"failure"')")" 1
case_ "two runs: a newer success wins over an older failure and passes" \
  "$(wruns "$(wrun_ 20 21 completed "$S")" "$(wrun_ 10 11 completed '"failure"')")" \
  "$(cruns "$(crun_ 100 11 completed '"failure"')" "$(crun_ 200 21 completed "$S")")" 0
case_ "two runs: a newer pending run refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S")" "$(wrun_ 20 21 in_progress null)")" "${ok_check}" 1
case_ "a newer pull request run does not count as the newest run" \
  "$(wruns "$(wrun_ 10 11 completed "$S")" "$(wrun_ 20 21 completed '"failure"' pull_request)")" "${ok_check}" 0
# A rerun of failed jobs keeps the run id and adds a ci-ok check run to the same suite: the newest check run decides.
case_ "a ci-ok rerun that passed after a failure passes" \
  "${ok_run}" "$(cruns "$(crun_ 100 11 completed '"failure"')" "$(crun_ 101 11 completed "$S")")" 0
case_ "a ci-ok rerun that failed after a pass refuses" \
  "$(wruns "$(wrun_ 10 11 completed "$S")")" "$(cruns "$(crun_ 100 11 completed "$S")" "$(crun_ 101 11 completed '"failure"')")" 1
case_ "runs cut short by paging refuse" \
  "{\"total_count\":101,\"workflow_runs\":[$(wrun_ 10 11 completed "$S")]}" "${ok_check}" 1
case_ "check runs cut short by paging refuse" \
  "${ok_run}" "{\"total_count\":101,\"check_runs\":[$(crun_ 100 11 completed "$S")]}" 1
case_ "an error answer for the runs refuses" '{"message":"Resource not accessible by integration"}' "${ok_check}" 1
case_ "an error answer for the check runs refuses" "${ok_run}" '{"message":"Not Found"}' 1
case_ "a run without a check suite id refuses" \
  '{"total_count":1,"workflow_runs":[{"id":10,"status":"completed","conclusion":"success","event":"push","head_branch":"main","path":".github/workflows/ci.yml","head_sha":"'"${sha}"'","head_repository":{"full_name":"'"${repo}"'"},"repository":{"full_name":"'"${repo}"'"}}]}' \
  "$(cruns "$(crun_ 100 null completed "$S")")" 1
case_ "an answer that is not JSON refuses" "<html>" "${ok_check}" 1
case_ "an empty answer refuses" "" "${ok_check}" 1

usage_() { # <name> <args...>: a usage error exits 2
  local name="$1" rc=0
  shift
  "${script}" "$@" > /dev/null 2>&1 || rc=$?
  if [ "${rc}" = 2 ]; then echo "ok   ${name}"; else echo "FAIL ${name}: got exit ${rc}, want 2"; fails=$((fails + 1)); fi
}
usage_ "a ref that is not a full sha is refused" "${repo}" HEAD "${work}/wruns.json" "${work}/cruns.json"
usage_ "a repository that is not owner/name is refused" "sablecourt" "${sha}" "${work}/wruns.json" "${work}/cruns.json"
usage_ "a missing argument is a usage error" "${repo}" "${sha}"
usage_ "wait mode rejects a non-numeric bound" --wait "${repo}" "${sha}" abc 30
usage_ "wait mode rejects a zero interval" --wait "${repo}" "${sha}" 60 0

# Polling mode (--wait): stub gh serves a sequence of recorded answers, fake sleep advances a fake clock file,
# and fake now reads that clock file.
mkdir -p "${work}/bin"
cat > "${work}/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = "api" ] || { echo "unexpected gh call: $*" >&2; exit 1; }
case "$2" in
  "repos/${STUB_REPO}/actions/runs?head_sha=${STUB_SHA}&event=push&branch=main&per_page=100")
    idx="$(cat "${STUB_DIR}/w_idx")"
    echo $((idx + 1)) > "${STUB_DIR}/w_idx"
    if [ -f "${STUB_DIR}/w_fail_${idx}" ]; then
      echo "HTTP 502: Bad Gateway" >&2
      exit 1
    fi
    if [ -f "${STUB_DIR}/w_${idx}.json" ]; then
      cat "${STUB_DIR}/w_${idx}.json"
    else
      cat "${STUB_DIR}/w_default.json"
    fi
    ;;
  "repos/${STUB_REPO}/commits/${STUB_SHA}/check-runs?check_name=ci-ok&per_page=100")
    idx="$(cat "${STUB_DIR}/c_idx")"
    echo $((idx + 1)) > "${STUB_DIR}/c_idx"
    if [ -f "${STUB_DIR}/c_fail_${idx}" ]; then
      echo "HTTP 502: Bad Gateway" >&2
      exit 1
    fi
    if [ -f "${STUB_DIR}/c_${idx}.json" ]; then
      cat "${STUB_DIR}/c_${idx}.json"
    else
      cat "${STUB_DIR}/c_default.json"
    fi
    ;;
  *)
    echo "unexpected gh api path: $2" >&2
    exit 1
    ;;
esac
SH
cat > "${work}/bin/fake-now" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
cat "${STUB_DIR}/clock"
SH
cat > "${work}/bin/fake-sleep" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
now="$(cat "${STUB_DIR}/clock")"
echo $((now + $1)) > "${STUB_DIR}/clock"
echo "$1" >> "${STUB_DIR}/sleeps"
SH
chmod +x "${work}/bin/gh" "${work}/bin/fake-now" "${work}/bin/fake-sleep"

# wait_case <name> <max> <interval> <want_rc> <want_sleeps> <setup_fn>
wait_case() {
  local name="$1" max="$2" interval="$3" want_rc="$4" want_sleeps="$5" setup="$6"
  local dir rc=0 sleeps=""
  dir="$(mktemp -d "${work}/wait.XXXXXX")"
  echo 0 > "${dir}/w_idx"
  echo 0 > "${dir}/c_idx"
  echo 1000 > "${dir}/clock"
  : > "${dir}/sleeps"
  "${setup}" "${dir}"
  env PATH="${work}/bin:${PATH}" STUB_DIR="${dir}" STUB_REPO="${repo}" STUB_SHA="${sha}" \
    RELEASE_CI_OK_NOW="${work}/bin/fake-now" RELEASE_CI_OK_SLEEP="${work}/bin/fake-sleep" \
    "${script}" --wait "${repo}" "${sha}" "${max}" "${interval}" > /dev/null 2>&1 || rc=$?
  sleeps="$(paste -sd, "${dir}/sleeps")"
  if [ "${rc}" = "${want_rc}" ] && [ "${sleeps}" = "${want_sleeps}" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got exit ${rc} sleeps '${sleeps}', want exit ${want_rc} sleeps '${want_sleeps}'"
    fails=$((fails + 1))
  fi
}

pending_run="$(wruns "$(wrun_ 10 11 in_progress null)")"
queued_run="$(wruns "$(wrun_ 10 11 queued null)")"
failed_run="$(wruns "$(wrun_ 10 11 completed '"failure"')")"
pending_check="$(cruns "$(crun_ 100 11 in_progress null)")"
failed_check="$(cruns "$(crun_ 100 11 completed '"failure"')")"

setup_in_progress_then_pass() {
  printf '%s' "${pending_run}" > "$1/w_0.json"
  printf '%s' "${pending_run}" > "$1/w_1.json"
  printf '%s' "${ok_run}" > "$1/w_default.json"
  printf '%s' "${pending_check}" > "$1/c_0.json"
  printf '%s' "${pending_check}" > "$1/c_1.json"
  printf '%s' "${ok_check}" > "$1/c_default.json"
}
setup_queued_then_pending_check_then_pass() {
  printf '%s' "${queued_run}" > "$1/w_0.json"
  printf '%s' "${ok_run}" > "$1/w_default.json"
  printf '%s' "${pending_check}" > "$1/c_0.json"
  printf '%s' "${pending_check}" > "$1/c_1.json"
  printf '%s' "${ok_check}" > "$1/c_default.json"
}
setup_in_progress_forever() {
  printf '%s' "${pending_run}" > "$1/w_default.json"
  printf '%s' "${pending_check}" > "$1/c_default.json"
}
setup_failed_run() {
  printf '%s' "${failed_run}" > "$1/w_default.json"
  printf '%s' "${ok_check}" > "$1/c_default.json"
}
setup_failed_check() {
  printf '%s' "${ok_run}" > "$1/w_default.json"
  printf '%s' "${failed_check}" > "$1/c_default.json"
}
setup_in_progress_then_fail() {
  printf '%s' "${pending_run}" > "$1/w_0.json"
  printf '%s' "${failed_run}" > "$1/w_default.json"
  printf '%s' "${pending_check}" > "$1/c_0.json"
  printf '%s' "${failed_check}" > "$1/c_default.json"
}
setup_no_run() {
  printf '%s' "$(wruns)" > "$1/w_default.json"
  printf '%s' "${ok_check}" > "$1/c_default.json"
}
setup_gh_error() {
  touch "$1/w_fail_0"
  printf '%s' "${ok_run}" > "$1/w_default.json"
  printf '%s' "${ok_check}" > "$1/c_default.json"
}

wait_case "wait: in progress then success passes" 120 30 0 "30,30" setup_in_progress_then_pass
wait_case "wait: queued run then pending ci-ok then success passes" 120 30 0 "30,30" setup_queued_then_pending_check_then_pass
wait_case "wait: in progress until the bound refuses" 75 30 1 "30,30,15" setup_in_progress_forever
wait_case "wait: failed run refuses at once without sleeping" 120 30 1 "" setup_failed_run
wait_case "wait: failed ci-ok refuses at once without sleeping" 120 30 1 "" setup_failed_check
wait_case "wait: in progress then failed run refuses at once" 120 30 1 "30" setup_in_progress_then_fail
wait_case "wait: no run refuses at once without sleeping" 120 30 1 "" setup_no_run
wait_case "wait: gh api failure refuses at once without sleeping" 120 30 1 "" setup_gh_error

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
