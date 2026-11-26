#!/usr/bin/env bash
# Tests of hack/ci-ok.sh, the predicate of the ci-ok job. Usage: hack/test/ci-ok_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
fails=0

# needs <go> <images> <docs> <codeql> then job=result pairs that override the default "success".
needs() {
  local go="$1" images="$2" docs="$3" codeql="$4"
  shift 4
  local json
  json="$(jq -n --arg go "${go}" --arg images "${images}" --arg docs "${docs}" --arg codeql "${codeql}" '{
    changes: {result: "success", outputs: {go: $go, images: $images, docs: $docs, codeql: $codeql}},
    lint: {result: "success"}, test: {result: "success"}, govulncheck: {result: "success"},
    gosec: {result: "success"}, images: {result: "success"}, "docs-check": {result: "success"},
    codeql: {result: "success"}, "workflow-lint": {result: "success"}, "secret-scan": {result: "success"},
    "repo-checks": {result: "success"}}')"
  local kv
  for kv in "$@"; do
    case "${kv}" in
      -*) json="$(jq --arg k "${kv#-}" 'del(.[$k])' <<< "${json}")" ;;
      changes.outputs.*=*) k="${kv%%=*}"; k="${k#changes.outputs.}"; json="$(jq --arg k "${k}" --arg v "${kv#*=}" '.changes.outputs[$k] = $v' <<< "${json}")" ;;
      changes.outputs.-*) json="$(jq --arg k "${kv#changes.outputs.-}" 'del(.changes.outputs[$k])' <<< "${json}")" ;;
      *) json="$(jq --arg k "${kv%%=*}" --arg v "${kv#*=}" '.[$k].result = $v' <<< "${json}")" ;;
    esac
  done
  printf '%s' "${json}"
}

# check <name> <want: pass|fail> <event> <needs json>
check() {
  local got
  if NEEDS="$4" EVENT_NAME="$3" "${root}/hack/ci-ok.sh" > /dev/null 2>&1; then got=pass; else got=fail; fi
  if [ "${got}" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1: got ${got}, want $2"; fails=$((fails + 1)); fi
}

check "all selected and successful" pass pull_request "$(needs true true true true)"
check "legitimately skipped by the decision" pass pull_request "$(needs false false false false lint=skipped test=skipped govulncheck=skipped gosec=skipped images=skipped docs-check=skipped codeql=skipped)"
check "images and codeql skipped on push" pass push "$(needs true true true true images=skipped codeql=skipped)"
check "selected job skipped" fail pull_request "$(needs true true true true test=skipped)"
check "selected codeql skipped" fail pull_request "$(needs true true true true codeql=skipped)"
check "always-run job skipped" fail pull_request "$(needs false false false false secret-scan=skipped)"
check "repo checks skipped" fail pull_request "$(needs false false false false repo-checks=skipped)"
check "missing job entry" fail pull_request "$(needs true true true true -test)"
check "missing deselected job entry" fail pull_request "$(needs false false false false -lint)"
check "failure" fail pull_request "$(needs true true true true lint=failure)"
check "failure of a deselected job" fail pull_request "$(needs false true true true lint=failure)"
check "cancelled" fail pull_request "$(needs true true true true gosec=cancelled)"
check "changes failed" fail pull_request "$(needs true true true true changes=failure)"
check "changes skipped" fail pull_request "$(needs true true true true changes=skipped)"
check "output missing" fail pull_request "$(needs true true true true changes.outputs.-go)"
check "output empty" fail pull_request "$(needs true true true true changes.outputs.go=)"
check "output not a boolean" fail pull_request "$(needs true true true true changes.outputs.docs=yes)"
check "not JSON" fail pull_request "not json"

# The table in hack/ci-ok.sh and the needs of the ci-ok job must name the same jobs.
root="$(cd "$(dirname "$0")/../.." && pwd)"
table="$(awk '/^  \| \{$/ { on = 1; next } /\} as \$selected/ { on = 0 } on { sub(/^ */, ""); sub(/:.*/, ""); gsub(/"/, ""); print }' "${root}/hack/ci-ok.sh" | LC_ALL=C sort | tr '\n' ' ')"
needs_line="$(grep -E '^    needs: \[changes, ' "${root}/.github/workflows/ci.yml")"
listed="$(sed 's/.*\[changes, //; s/\]//' <<< "${needs_line}" | tr -d ' ' | tr ',' '\n' | LC_ALL=C sort | tr '\n' ' ')"
if [ -n "${table}" ] && [ "${table}" = "${listed}" ]; then
  echo "ok   ci-ok needs match the table"
else
  echo "FAIL ci-ok needs (${listed}) differ from the table (${table})"
  fails=$((fails + 1))
fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
