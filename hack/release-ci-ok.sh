#!/usr/bin/env bash
# Decides whether CI passed on main for the commit a release tag points at, for release.yml. Reads two GitHub API
# answers saved as files: the workflow runs of the commit
# (GET repos/<repo>/actions/runs?head_sha=<sha>&event=push&branch=main) and its ci-ok check runs
# (GET repos/<repo>/commits/<sha>/check-runs?check_name=ci-ok). Fails closed:
#   - only workflow runs of .github/workflows/ci.yml, for the event push, on the branch main, for exactly <sha>,
#     listed for <repo> and with <repo> as head repository count (a pull request from a fork whose branch is called
#     main, a manual run, another branch or another workflow does not), whatever the query asked for;
#   - the newest of them (highest run id) decides: it must be completed with the conclusion success. A newer run
#     that failed wins over an older one that passed, because it is the latest result for that commit and a release
#     does not pick the run it likes. A rerun keeps its run id and reports its latest attempt;
#   - in that run's check suite, the newest check run named ci-ok from the github-actions app for <sha> (highest id,
#     so a rerun of the job counts) must be completed with the conclusion success;
#   - no such run or check run, anything still queued or in progress, any other conclusion, a list cut short by
#     paging (total_count above the entries returned) or an answer that is not the expected JSON is a refusal.
# Usage: hack/release-ci-ok.sh <owner/repo> <sha> <workflow runs file> <check runs file>
#   exit 0 when ci-ok passed on main for <sha>; 1 with the reason on stderr otherwise; 2 on a bad argument.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <owner/repo> <sha> <workflow runs file> <check runs file>" >&2
  exit 2
fi
repo="$1" sha="$2" wruns="$3" cruns="$4"
if ! [[ "${repo}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "'${repo}' is not owner/repo" >&2
  exit 2
fi
if ! [[ "${sha}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "'${sha}' is not a full commit sha" >&2
  exit 2
fi

if ! verdict="$(jq -nr --arg repo "${repo}" --arg sha "${sha}" --slurpfile w "${wruns}" --slurpfile c "${cruns}" '
  ($w | if length == 1 then .[0] else error("workflow runs: not one JSON document") end) as $w
  | ($c | if length == 1 then .[0] else error("check runs: not one JSON document") end) as $c
  | if ($w.workflow_runs | type) != "array" or ($w.total_count | type) != "number" then error("workflow runs: unexpected answer")
    elif ($c.check_runs | type) != "array" or ($c.total_count | type) != "number" then error("check runs: unexpected answer")
    elif $w.total_count > ($w.workflow_runs | length) or $c.total_count > ($c.check_runs | length) then
      "refuse more workflow runs or check runs than one page holds; refusing to decide on part of the list"
    else
      [$w.workflow_runs[] | select(.path == ".github/workflows/ci.yml" and .event == "push" and .head_branch == "main"
          and .head_sha == $sha and .repository.full_name == $repo and .head_repository.full_name == $repo)]
      | sort_by(.id) as $runs
      | if ($runs | length) == 0 then "refuse no push run of .github/workflows/ci.yml on main exists for \($sha)"
        elif ($runs[-1].check_suite_id | type) != "number" then "refuse the newest CI run on main has no check suite id"
        elif $runs[-1].status != "completed" then "refuse the newest CI run on main for \($sha) (run \($runs[-1].id)) is still \($runs[-1].status)"
        elif $runs[-1].conclusion != "success" then "refuse the newest CI run on main for \($sha) (run \($runs[-1].id)) concluded \($runs[-1].conclusion)"
        else
          $runs[-1] as $run
          | [$c.check_runs[] | select(.name == "ci-ok" and .app.slug == "github-actions" and .head_sha == $sha
              and .check_suite.id == $run.check_suite_id)] | sort_by(.id) as $ok
          | if ($ok | length) == 0 then "refuse the CI run \($run.id) on main has no ci-ok check run"
            elif $ok[-1].status != "completed" then "refuse ci-ok of the CI run \($run.id) on main is still \($ok[-1].status)"
            elif $ok[-1].conclusion != "success" then "refuse ci-ok of the CI run \($run.id) on main concluded \($ok[-1].conclusion)"
            else "pass ci-ok passed in the CI run \($run.id) on main for \($sha) (check run \($ok[-1].id))" end
        end
    end')"; then
  echo "the workflow runs or check runs of ${sha} could not be read; refusing to release" >&2
  exit 1
fi
case "${verdict}" in
  "pass "*) echo "${verdict#pass }" ;;
  "refuse "*)
    echo "${verdict#refuse }; refusing to release. Wait for the CI run on main to pass, then rerun this workflow." >&2
    exit 1 ;;
  *) echo "unexpected verdict '${verdict}'" >&2; exit 1 ;;
esac
