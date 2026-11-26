#!/usr/bin/env bash
# The predicate of the `ci-ok` job of .github/workflows/ci.yml. Reads the `needs` context as JSON from $NEEDS and
# the event name from $EVENT_NAME, prints each job's result, and exits 0 only when:
#   - the `changes` job succeeded and each of its outputs go, images, docs and codeql is exactly "true" or "false";
#   - every job listed below is present in $NEEDS (a job missing from the result fails the check);
#   - every job the decision or the event selects, and every job that always runs, succeeded;
#   - a job counts as deselected only when the decision said "false" for its group or the event excludes it
#     (images and codeql run on pull requests only); a deselected job may be skipped or successful.
# Failure, cancellation or a skip of a selected job fails the check. A called workflow reports one result for all
# its jobs: `test` covers the race tests and the coverage upload (test.yml), so a failed upload job fails ci-ok
# and a missing token (the upload job passes without uploading) does not. Tests: hack/test/ci-ok_test.sh.
# A new job in ci.yml goes into `needs` of ci-ok and into the table below.
set -euo pipefail

: "${NEEDS:?NEEDS must hold the needs context as JSON}"
event="${EVENT_NAME:-}"

printf '%s\n' "${NEEDS}" | jq -r 'to_entries[] | "\(.key): \(.value.result)"' || { echo "NEEDS is not JSON" >&2; exit 1; }

printf '%s\n' "${NEEDS}" | jq -e --arg event "${event}" '
  def out(k): .changes.outputs[k];
  def bool(k): out(k) == "true" or out(k) == "false";
  . as $root
  | (["go","images","docs","codeql"] | all(. as $k | $root | bool($k))) as $valid
  | ($root.changes.result == "success") as $changes_ok
  # job name -> whether the decision and the event select it
  | {
      lint:            (out("go") == "true"),
      test:            (out("go") == "true"),
      govulncheck:     (out("go") == "true"),
      gosec:           (out("go") == "true"),
      images:          ($event == "pull_request" and out("images") == "true"),
      "docs-check":    (out("docs") == "true"),
      codeql:          ($event == "pull_request" and out("codeql") == "true"),
      "workflow-lint": true,
      "secret-scan":   true,
      "repo-checks":   true
    } as $selected
  | $changes_ok and $valid
    and ($selected | to_entries | all(
          .key as $job | .value as $sel | ($root[$job].result // "missing") as $r
          | if $sel then $r == "success" else ($r == "success" or $r == "skipped") end))
' > /dev/null 2>&1 && exit 0

echo "ci-ok: a job failed, was cancelled, is missing, or was skipped although selected; or the changes job or its outputs are invalid" >&2
exit 1
