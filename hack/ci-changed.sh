#!/usr/bin/env bash
# Decides which CI job groups have to run, from the files a pull request changes. Called once by the `changes`
# job of .github/workflows/ci.yml, which passes the result to the jobs it calls.
# Usage: hack/ci-changed.sh    (prints go=, images=, docs= and codeql= lines, each true or false; append to $GITHUB_OUTPUT)
# Environment:
#   EVENT_NAME  github.event_name; anything but "pull_request" (push to main, manual run) runs everything
#   BASE_SHA    base commit of the pull request; HEAD is compared with it (the checkout is the merge commit)
# Groups: go (lint, test, govulncheck, gosec), images (image build of the Dockerfile targets), docs (reference
# pages check and internal links), codeql (CodeQL analysis of Go code and workflows).
# A change to ci.yml, to a workflow it calls or to this script runs every group. Fails open: when the diff
# cannot be computed everything runs, so a problem here never lets a change skip its checks.
set -euo pipefail

# What the binaries and the images are built from; each group below that builds or runs them includes it.
build_paths='.*\.go|go\.mod|go\.sum|cmd/.*|internal/.*|examples/.*|Dockerfile|\.dockerignore'
# The reference pages under docs/reference are checked against the code by go test, so they are Go inputs too.
go_paths="${build_paths}|Makefile|hack/.*|\.golangci\.yml|docs/reference/.*"
# Go workflows that ci.yml does not call; a change to one runs the Go checks.
go_workflows='\.github/workflows/(fuzz|release|images)\.yml'
go_re="^(${go_paths}|${go_workflows})$"
images_re="^(${build_paths}|LICENSE|\.github/workflows/images\.yml)$"
# Every Markdown file is link checked; the reference pages are also checked by make docs-check.
docs_re='^(docs/.*|.*\.md|hack/docs-.*|Makefile)$'
# CodeQL analyses Go (built from the module) and the workflows and composite actions themselves.
codeql_re='^(.*\.go|go\.mod|go\.sum|\.github/workflows/.*|\.github/actions/.*|\.github/codeql/.*)$'
# ci.yml and the workflows it calls, and this script.
self_re='^(\.github/workflows/(ci|lint|test|vulncheck|gosec|workflow-lint|images-build|docs-check|codeql|secret-scan)\.yml|hack/ci-changed\.sh)$'

emit() { # go images docs codeql
  echo "go=$1"
  echo "images=$2"
  echo "docs=$3"
  echo "codeql=$4"
}

if [ "${EVENT_NAME:-}" != "pull_request" ]; then
  emit true true true true
  exit 0
fi

if [ -z "${BASE_SHA:-}" ] || ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  git fetch --no-tags --depth=1 origin "${BASE_SHA:-}" >&2 || true
fi
if ! files="$(git diff --name-only --no-renames "${BASE_SHA:-}" HEAD 2>/dev/null)"; then
  echo "cannot diff against ${BASE_SHA:-unset}: running everything" >&2
  emit true true true true
  exit 0
fi

echo "changed files:" >&2
printf '%s\n' "$files" | sed 's/^/  /' >&2

match() { printf '%s\n' "$files" | grep -Eq "$1" && echo true || echo false; }

if [ "$(match "$self_re")" = true ]; then
  emit true true true true
else
  emit "$(match "$go_re")" "$(match "$images_re")" "$(match "$docs_re")" "$(match "$codeql_re")"
fi
