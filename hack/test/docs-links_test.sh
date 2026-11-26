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

# Syntax the earlier line-by-line parser missed (each broken one passed).
case_ "single-quoted title, missing target" 1 "" docs/a.md "[t](absent.md 'Title')"
case_ "single-quoted title, existing target" 0 "" docs/a.md "[t](target.md#plain-heading 'Title')"
case_ "parenthesised title, missing target" 1 "" docs/a.md '[t](absent.md (Title))'
case_ "single-quoted HTML href, missing target" 1 "" docs/a.md "<a href='absent.md'>t</a>"
case_ "single-quoted HTML src, existing target" 0 "" docs/a.md "<img src='target.md' alt='x'>"
case_ "unquoted HTML href, missing target" 1 "" docs/a.md '<a href=absent.md>t</a>'
case_ "link text over two lines, missing target" 1 "" docs/a.md 'see [the
page](absent.md) here'
case_ "target on the next line, missing target" 1 "" docs/a.md 'see [the page](
absent.md "Title") here'
case_ "link over two lines, existing anchor" 0 "" docs/a.md 'see [the
page](target.md#plain-heading)'
case_ "link after a wrapped code span is still checked" 1 "" docs/a.md 'x `code
span` and [t](absent.md)'
case_ "link inside a wrapped code span is ignored" 0 "" docs/a.md 'x `code [t](absent.md)
span` y'
case_ "Setext heading level 1" 0 "" docs/s.md 'Setext Title
============' docs/a.md '[t](s.md#setext-title)'
case_ "Setext heading level 2" 0 "" docs/s.md 'Intro

Second Part
-----------' docs/a.md '[t](s.md#second-part)'
case_ "a rule after a blank line is not a heading" 1 "" docs/s.md 'Intro

---' docs/a.md '[t](s.md#intro)'
case_ "a table separator is not a heading" 1 "" docs/s.md '| Col |
| --- |' docs/a.md '[t](s.md#col)'

# A pending line covers one occurrence.
case_ "two links to one pending target, one line" 1 "docs/a.md -> docs/absent.md
" docs/a.md '[t](absent.md) and [u](absent.md)'
case_ "two links to one pending target, two lines" 0 "docs/a.md -> docs/absent.md
docs/a.md -> docs/absent.md
" docs/a.md '[t](absent.md) and [u](absent.md)'
case_ "two lines, one link" 1 "docs/a.md -> docs/absent.md
docs/a.md -> docs/absent.md
" docs/a.md '[t](absent.md)'
case_ "pending fragment used twice, one line" 1 "docs/a.md -> docs/target.md#later
" docs/a.md '[t](target.md#later)

[u](target.md#later)'
case_ "two uses of one reference to a pending target, one line" 1 "docs/a.md -> docs/absent.md
" docs/a.md 'see [t][p] and [u][p]

[p]: absent.md'
case_ "two uses of one reference to a pending target, two lines" 0 "docs/a.md -> docs/absent.md
docs/a.md -> docs/absent.md
" docs/a.md 'see [t][p] and [u][p]

[p]: absent.md'
case_ "unused definition to a pending target, one line" 0 "docs/a.md -> docs/absent.md
" docs/a.md '[p]: absent.md'

# Fences: a closing fence has the opening character and at least its length.
case_ "four-backtick fence holding three backticks" 1 "" docs/a.md '````
```
````

[t](absent.md)'
case_ "four-backtick fence hides its content" 0 "" docs/a.md '````
```
[t](absent.md)
````'
case_ "tilde fence is not closed by backticks" 0 "" docs/a.md '~~~
```
[t](absent.md)
~~~'

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
