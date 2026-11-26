#!/usr/bin/env bash
# Decides whether CI passed on main for the commit a release tag points at, for release.yml. Reads two GitHub API
# answers saved as files: the check runs of the commit (GET repos/<repo>/commits/<sha>/check-runs?check_name=ci-ok)
# and its check suites (GET repos/<repo>/commits/<sha>/check-suites). Fails closed:
#   - only check runs named ci-ok, made by the github-actions app, for exactly <sha>, in a check suite of the
#     github-actions app whose head_branch is main count (a run for a pull request branch does not);
#   - the newest of them (highest id) must be completed with the conclusion success;
#   - no such run, a newest run still queued or in progress, any other conclusion, a list cut short by paging
#     (total_count above the runs returned) or an answer that is not the expected JSON is a refusal.
# Usage: hack/release-ci-ok.sh <sha> <check runs file> <check suites file>
#   exit 0 when ci-ok passed on main for <sha>; 1 with the reason on stderr otherwise; 2 on a bad argument.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <sha> <check runs file> <check suites file>" >&2
  exit 2
fi
sha="$1" runs="$2" suites="$3"
if ! [[ "${sha}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "'${sha}' is not a full commit sha" >&2
  exit 2
fi

if ! verdict="$(jq -nr --arg sha "${sha}" --slurpfile runs "${runs}" --slurpfile suites "${suites}" '
  ($runs | if length == 1 then .[0] else error("check runs: not one JSON document") end) as $r
  | ($suites | if length == 1 then .[0] else error("check suites: not one JSON document") end) as $s
  | if ($r.check_runs | type) != "array" or ($r.total_count | type) != "number" then error("check runs: unexpected answer")
    elif ($s.check_suites | type) != "array" or ($s.total_count | type) != "number" then error("check suites: unexpected answer")
    elif $r.total_count > ($r.check_runs | length) or $s.total_count > ($s.check_suites | length) then
      "refuse more check runs or suites than one page holds; refusing to decide on part of the list"
    else
      [$s.check_suites[] | select(.app.slug == "github-actions" and .head_branch == "main" and .head_sha == $sha) | .id] as $main
      | [$r.check_runs[] | select(.name == "ci-ok" and .app.slug == "github-actions" and .head_sha == $sha
          and (.check_suite.id as $id | $main | index($id) != null))] | sort_by(.id) as $ok
      | if ($ok | length) == 0 then "refuse no ci-ok check run from a CI run on main exists for \($sha)"
        elif $ok[-1].status != "completed" then "refuse ci-ok on main for \($sha) is still \($ok[-1].status)"
        elif $ok[-1].conclusion != "success" then "refuse ci-ok on main for \($sha) concluded \($ok[-1].conclusion)"
        else "pass ci-ok passed on main for \($sha) (check run \($ok[-1].id))" end
    end')"; then
  echo "the check runs or suites of ${sha} could not be read; refusing to release" >&2
  exit 1
fi
case "${verdict}" in
  "pass "*) echo "${verdict#pass }" ;;
  "refuse "*)
    echo "${verdict#refuse }; refusing to release. Wait for the CI run on main to pass, then rerun this workflow." >&2
    exit 1 ;;
  *) echo "unexpected verdict '${verdict}'" >&2; exit 1 ;;
esac
