#!/usr/bin/env bash
# Offline tests of hack/ci-changed.sh. Each case builds a throwaway Git repository with a base commit and one
# change on top, runs the detector the way the `changes` job does (EVENT_NAME=pull_request, BASE_SHA=<base>)
# and compares its four output lines. Usage: hack/test/ci-changed_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
detector="${root}/hack/ci-changed.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0 n=0

test_email="detector-test@example.invalid"
git_q() { git -c user.name=test -c user.email="${test_email}" -c commit.gpgsign=false "$@"; }

# new_repo: a repository whose base commit holds a few files of every group.
new_repo() {
  repo="${work}/repo$((n += 1))"
  mkdir -p "${repo}" && cd "${repo}"
  git_q init -q -b main .
  mkdir -p internal/x cmd/y docs/reference docs/how-to cmd/sablecourt-agent
  echo 'package x' > internal/x/a.go
  echo 'module m' > go.mod
  echo '# ref' > docs/reference/cli.md
  echo '# how' > docs/how-to/a.md
  echo 'FROM scratch' > cmd/sablecourt-agent/Dockerfile
  echo 'notes' > notes.txt
  git_q add -A && git_q commit -qm base
  base="$(git rev-parse HEAD)"
}

commit() { git_q add -A && git_q commit -qm change --allow-empty; }

# expect <name> <go> <images> <docs> <codeql> [base override]
expect() {
  local name="$1" want got
  want="$(printf 'go=%s\nimages=%s\ndocs=%s\ncodeql=%s' "$2" "$3" "$4" "$5")"
  got="$(EVENT_NAME=pull_request BASE_SHA="${6-${base}}" "${detector}" 2>/dev/null)" || { echo "FAIL ${name}: detector exited non-zero"; fails=$((fails + 1)); return; }
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got $(echo "${got}" | tr '\n' ' ')want $(echo "${want}" | tr '\n' ' ')"
    fails=$((fails + 1))
  fi
  cd "${root}"
}

new_repo; echo 'package x' > "café.go"; commit
expect "non-ASCII Go file name" true true false true

new_repo; mkdir -p internal; printf 'package x\n' > "internal/fixture	name.go"; commit
expect "tab in a path" true true true true

new_repo; mkdir -p bulk; for i in $(seq 1 3500); do echo "$i" > "bulk/f$i.txt"; done; echo 'package x' > 00.go; commit
expect "00.go plus 3,500 other files" true true false true

new_repo; echo 'package x' > "file with space.go"; commit
expect "spaces in a Go file name" true true false true

new_repo; echo 'package x' > ./-leading.go; commit
expect "leading dash" true true false true

new_repo; git_q mv internal/x/a.go notes-renamed.txt; commit
expect "Go file renamed to text" true true true true

new_repo; git_q rm -q internal/x/a.go; commit
expect "Go file deleted" true true true true

new_repo; echo 'go 1.27' >> go.mod; commit
expect "go.mod only" true true false true

new_repo; printf 'go 1.27\nuse ./absent\n' > go.work; commit
expect "go.work only" true true false false

new_repo; echo 'example.invalid/m v1.0.0 h1:x=' > go.work.sum; commit
expect "go.work.sum only" true true false false

new_repo; echo 'go 1.27' > go.work; commit; git_q rm -q go.work; commit; base="$(git rev-parse HEAD~1)"
expect "go.work deleted" true true false false

new_repo; mkdir -p tools; echo 'go 1.27' > tools/go.work.bak; commit
expect "a file that only looks like go.work" false false false false

new_repo; echo 'bin/' > .dockerignore; commit
expect ".dockerignore" true true false false

new_repo; echo 'FROM scratch AS x' >> cmd/sablecourt-agent/Dockerfile; commit
expect "nested Dockerfile (not built; Go group runs)" true true true false

new_repo; echo 'more' >> docs/how-to/a.md; commit
expect "docs page only (link check runs anyway)" false false false false

new_repo; echo 'more' >> docs/reference/cli.md; commit
expect "reference page" true false true false

new_repo; git_q mv internal/x/a.go internal/x/b.go; commit
expect "source rename that a docs link may point at" true true true true

new_repo; commit
expect "empty diff" false false false false

new_repo; echo x >> notes.txt; commit
expect "missing base object" true true true true 0000000000000000000000000000000000000000

new_repo; echo x >> notes.txt; commit
expect "unset base" true true true true ""

new_repo; mkdir -p .github/workflows; echo 'on: push' > .github/workflows/lint.yml; commit
expect "called workflow" true true true true

new_repo; mkdir -p .github/workflows; echo 'on: push' > .github/workflows/scorecard.yml; commit
expect "standalone workflow" false false false true

new_repo; echo x >> notes.txt; commit
got="$(cd "${repo}" && TMPDIR="${work}/no/such/dir" EVENT_NAME=pull_request BASE_SHA="${base}" "${detector}" 2>/dev/null)"
if [ "${got}" = "$(printf 'go=true\nimages=true\ndocs=true\ncodeql=true')" ]; then
  echo "ok   classification error selects everything"
else
  echo "FAIL classification error selects everything: got ${got}"
  fails=$((fails + 1))
fi

got="$(EVENT_NAME=push "${detector}" 2>/dev/null)"
if [ "${got}" = "$(printf 'go=true\nimages=true\ndocs=true\ncodeql=true')" ]; then
  echo "ok   push selects everything"
else
  echo "FAIL push selects everything"
  fails=$((fails + 1))
fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
