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
  mkdir -p internal/x cmd/y docs/reference docs/how-to internal/gate/councilproxy
  echo 'package x' > internal/x/a.go
  echo 'module m' > go.mod
  echo '# ref' > docs/reference/cli.md
  echo '# how' > docs/how-to/a.md
  echo 'FROM scratch' > internal/gate/councilproxy/Dockerfile
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

# Make reads GNUmakefile and makefile ahead of Makefile, so either can change what a bare `make` runs.
new_repo; printf 'lint:\n\ttrue\n' > GNUmakefile; commit
expect "GNUmakefile only" true false true false

new_repo; printf 'lint:\n\ttrue\n' > makefile; commit
expect "lowercase makefile only" true false true false

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

new_repo; echo 'FROM scratch AS x' >> internal/gate/councilproxy/Dockerfile; commit
expect "nested Dockerfile (not built; Go group runs)" true true true false

new_repo; echo 'more' >> docs/how-to/a.md; commit
expect "docs page only" false false true false

new_repo; echo 'site_name = "Sablecourt"' > zensical.toml; commit
expect "zensical.toml only" false false true false

new_repo; echo 'zensical==0.1.1' > requirements-docs.in; commit
expect "requirements-docs.in only" false false true false

new_repo; echo 'zensical==0.1.1' > requirements-docs.txt; commit
expect "requirements-docs.txt only" false false true false

new_repo; mkdir -p overrides; echo '<!-- x -->' > overrides/main.html; commit
expect "overrides file only" false false true false

new_repo; mkdir -p hack; echo '# checker' > hack/docs-site-check.py; commit
expect "built-site checker script" true false true false

new_repo; mkdir -p hack; echo '# pending' > hack/docs-pages-pending.txt; commit
expect "docs pending list" true false true false

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

# Every input of the pull request image build selects the images group: what the root Dockerfile and its build
# context read (Go code and embedded files, the module and workspace files, vendor/, the Dockerfile and both
# forms of its ignore file) and what images-build.yml runs before the build. The second list is read from those
# two files, so a new COPY source or a new script there needs a path in the images group.
images_only() { # name path
  local got
  new_repo
  mkdir -p "$(dirname "$2")"
  echo change >> "$2"
  commit
  got="$(EVENT_NAME=pull_request BASE_SHA="${base}" "${detector}" 2>/dev/null | grep '^images=')" || got=""
  if [ "${got}" = "images=true" ]; then
    echo "ok   image input ${2} selects the image build"
  else
    echo "FAIL image input ${2} selects the image build: got '${got}'"
    fails=$((fails + 1))
  fi
  cd "${root}"
}
for input in Dockerfile .dockerignore Dockerfile.dockerignore vendor/modules.txt vendor/example.invalid/m/m.go \
  go.mod go.sum go.work go.work.sum main.go cmd/sablecourtd/main.go internal/ui/templates/page.html \
  internal/store/sqlite/migrations/0001.sql examples/demo-agent/main.go hack/setup-buildx.sh LICENSE; do
  images_only "${input}" "${input}"
done
derived="$( {
  grep -oE 'hack/[A-Za-z0-9_./-]+' "${root}/.github/workflows/images-build.yml"
  awk 'toupper($1) == "COPY" && $0 !~ /--from=/ { for (i = 2; i < NF; i++) if ($i !~ /^--/ && $i != ".") print $i }' "${root}/Dockerfile"
} | LC_ALL=C sort -u)"
[ -n "${derived}" ] || { echo "FAIL no inputs read from images-build.yml and the Dockerfile"; fails=$((fails + 1)); }
while IFS= read -r input; do
  [ -n "${input}" ] && images_only "${input}" "${input}"
done <<< "${derived}"

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
