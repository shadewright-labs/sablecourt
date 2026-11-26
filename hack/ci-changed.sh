#!/usr/bin/env bash
# Decides which CI job groups have to run, from the files a pull request changes. Called once by the `changes`
# job of .github/workflows/ci.yml, which passes the result to the jobs it calls.
# Usage: hack/ci-changed.sh    (prints go=, images=, docs= and codeql= lines, each true or false; append to $GITHUB_OUTPUT)
# Environment:
#   EVENT_NAME  github.event_name; anything but "pull_request" (push to main, manual run) runs everything
#   BASE_SHA    base commit of the pull request; HEAD is compared with it (the checkout is the merge commit)
# Groups: go (lint, test, govulncheck, gosec), images (build of the daemon and agent targets of the root
# Dockerfile), docs (the reference pages check), codeql (CodeQL analysis of Go code and workflows). The internal
# link check, workflow lint, the script tests and the secret scan run on every pull request.
# A change to ci.yml, to a workflow it calls or to this script runs every group. Fails open: when the diff
# cannot be computed, or anything in the classification fails, every group runs, so a problem here never lets
# a change skip its checks. Paths are read NUL-delimited (no quoting of unusual names) and matched in the shell,
# with no pipeline that a match could close early. Tests: hack/test/ci-changed_test.sh.
set -Eeuo pipefail
# Byte-wise matching: a path that is not valid UTF-8 still matches `.*`.
export LC_ALL=C

all() {
  printf 'go=true\nimages=true\ndocs=true\ncodeql=true\n'
}
# Any failing command from here on prints every group as selected and exits 0. Nothing else is printed before
# the final four lines, so the output is always exactly one decision.
fail_open() {
  echo "classification failed (line ${1:-?}): running everything" >&2
  all
  exit 0
}
trap 'fail_open "$LINENO"' ERR

# What the binaries and the images are built from; each group below that builds or runs them includes it. The
# workspace files go.work and go.work.sum count too: CI and the root Dockerfile build with GOWORK=off, but a
# change to them still runs the Go checks and the image build, so nothing that can change a build is skipped.
build_paths='.*\.go|go\.mod|go\.sum|go\.work|go\.work\.sum|cmd/.*|internal/.*|examples/.*|Dockerfile|\.dockerignore'
# The reference pages under docs/reference are checked against the code by go test, so they are Go inputs too.
go_paths="${build_paths}|Makefile|hack/.*|\.golangci\.yml|docs/reference/.*"
# Go workflows that ci.yml does not call; a change to one runs the Go checks.
go_workflows='\.github/workflows/(fuzz|release)\.yml'
go_re="^(${go_paths}|${go_workflows})$"
images_re="^(${build_paths}|LICENSE)$"
# The reference pages and what renders or checks them.
docs_re='^(docs/reference/.*|cmd/.*|internal/.*|Makefile)$'
# CodeQL analyses Go (built from the module) and the workflows and composite actions themselves.
codeql_re='^(.*\.go|go\.mod|go\.sum|\.github/workflows/.*|\.github/actions/.*|\.github/codeql/.*)$'
# ci.yml and the workflows it calls, and this script.
self_re='^(\.github/workflows/(ci|lint|test|vulncheck|gosec|workflow-lint|images-build|docs-check|codeql|secret-scan|repo-checks)\.yml|hack/ci-changed\.sh|hack/ci-ok\.sh)$'

if [ "${EVENT_NAME:-}" != "pull_request" ]; then
  all
  exit 0
fi

if [ -z "${BASE_SHA:-}" ] || ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  git fetch --no-tags --depth=1 origin "${BASE_SHA:-}" >&2 || true
fi
# The ERR trap is inherited by the command substitution (set -E). Without clearing it there, a failing mktemp
# would run fail_open inside the subshell and its four lines would become the file name.
list="$(trap - ERR; mktemp "${TMPDIR:-/tmp}/ci-changed.XXXXXX")"
trap 'rm -f "${list}"' EXIT
if ! git diff -z --name-only --no-renames "${BASE_SHA:-}" HEAD -- > "${list}" 2>/dev/null; then
  echo "cannot diff against ${BASE_SHA:-unset}: running everything" >&2
  all
  exit 0
fi

go=false images=false docs=false codeql=false self=false
count=0
while IFS= read -r -d '' f; do
  count=$((count + 1))
  if [[ "${f}" =~ ${self_re} ]]; then self=true; fi
  if [[ "${f}" =~ ${go_re} ]]; then go=true; fi
  if [[ "${f}" =~ ${images_re} ]]; then images=true; fi
  if [[ "${f}" =~ ${docs_re} ]]; then docs=true; fi
  if [[ "${f}" =~ ${codeql_re} ]]; then codeql=true; fi
done < "${list}"
echo "changed files: ${count}" >&2

if [ "${self}" = true ]; then
  all
  exit 0
fi
printf 'go=%s\nimages=%s\ndocs=%s\ncodeql=%s\n' "${go}" "${images}" "${docs}" "${codeql}"
