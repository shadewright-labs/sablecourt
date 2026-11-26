#!/usr/bin/env bash
# Parser and policy fixtures for hack/docs-links.py check. Each case writes a small Git repository, runs the
# checker on it (DOCS_ROOT) and compares the exit status. Usage: hack/test/docs-links_test.sh
# The fixtures hold Markdown backticks in single quotes on purpose.
# shellcheck disable=SC2016
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0 n=0

# case_ <name> <want exit> <pending file content> then pairs of <path> <content>
case_() {
  local name="$1" want="$2" pending="$3" repo="${work}/r$((n += 1))" got=0
  shift 3
  mkdir -p "${repo}/hack" "${repo}/docs"
  printf '# Title\n\n## Has `code` in it\n\n## Plain heading\n' > "${repo}/docs/target.md"
  printf '%s' "${pending}" > "${repo}/hack/docs-pages-pending.txt"
  while [ "$#" -gt 0 ]; do
    mkdir -p "$(dirname "${repo}/$1")"
    printf '%s\n' "$2" > "${repo}/$1"
    shift 2
  done
  git -C "${repo}" init -q
  git -C "${repo}" add -A
  DOCS_ROOT="${repo}" python3 -I "${root}/hack/docs-links.py" check > "${work}/out" 2>&1 || got=$?
  if [ "${got}" = "${want}" ]; then echo "ok   ${name}"; else echo "FAIL ${name}: exit ${got}, want ${want}: $(tail -n 2 "${work}/out")"; fails=$((fails + 1)); fi
}

case_ "valid link and anchor" 0 "" docs/a.md '[t](target.md#plain-heading)'
case_ "heading with inline code" 0 "" docs/a.md '[t](target.md#has-code-in-it)'
case_ "missing file" 1 "" docs/a.md '[t](absent.md)'
case_ "missing anchor" 1 "" docs/a.md '[t](target.md#nope)'
case_ "angle bracket link to a missing file" 1 "" docs/a.md '[t](<missing page.md>)'
case_ "angle bracket link to an existing file" 0 "" docs/a.md '[t](<target.md>)'
case_ "undefined reference link" 1 "" docs/a.md 'see [text][nowhere]'
case_ "collapsed reference link without definition" 1 "" docs/a.md 'see [nowhere][]'
case_ "defined reference link" 0 "" docs/a.md 'see [text][ok]

[ok]: target.md'
case_ "reference definition to a missing file" 1 "" docs/a.md '[ok]: absent.md'
case_ "code span and fence ignored" 0 "" docs/a.md 'x `[t](absent.md)` y
```
[t](absent.md)
```'
case_ "pending link from its source" 0 "docs/a.md -> docs/absent.md
" docs/a.md '[t](absent.md)'
case_ "pending target from another source" 1 "docs/a.md -> docs/absent.md
" docs/a.md '[t](absent.md)' docs/b.md '[t](absent.md)'
case_ "pending page does not cover a fragment" 1 "docs/a.md -> docs/absent.md
" docs/a.md '[t](absent.md#never-planned)'
case_ "pending heading" 0 "docs/a.md -> docs/target.md#later
" docs/a.md '[t](target.md#later)'
case_ "pending heading does not cover another heading" 1 "docs/a.md -> docs/target.md#later
" docs/a.md '[t](target.md#other)'
case_ "stale entry: target exists" 1 "docs/a.md -> docs/target.md
" docs/a.md '[t](target.md)'
case_ "unused entry" 1 "docs/a.md -> docs/absent.md
" docs/a.md 'no links'
case_ "malformed entry" 1 "docs/absent.md
" docs/a.md 'no links'

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
