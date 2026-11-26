#!/usr/bin/env bash
# Offline fixture tests for hack/docs-site-check.py. Builds small throwaway HTML sites and checks
# that a valid site passes while a broken link, a broken anchor, a missing image, a missing
# stylesheet, a third-party asset and a broken redirect each fail. Usage: hack/test/docs-site-check_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
checker="${root}/hack/docs-site-check.py"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0

page() { # file body
  mkdir -p "$(dirname "$1")"
  printf '<!doctype html><html><head><link rel="stylesheet" href="%s"></head><body>%s</body></html>\n' "$2" "$3" > "$1"
}

build() { # dir: valid site with home page, subpage, local stylesheet, local SVG image and valid links
  rm -rf "$1"
  mkdir -p "$1/stylesheets" "$1/diagrams" "$1/guide"
  printf 'body { color: #bbb4c5; }\n' > "$1/stylesheets/brand.css"
  printf '<svg xmlns="http://www.w3.org/2000/svg"></svg>\n' > "$1/diagrams/ctx.svg"
  page "$1/index.html" "./stylesheets/brand.css" \
    '<h1 id="top">Home</h1><a href="guide/#step">Guide</a> <a href="https://shadewrightlabs.com/sablecourt/guide/">Abs</a> <a href="https://example.com/">Ext</a> <img src="diagrams/ctx.svg" alt="ctx">'
  page "$1/guide/index.html" "../stylesheets/brand.css" \
    '<h2 id="step">Step</h2><a href="../">Home</a> <a href="../diagrams/ctx.svg"><img src="../diagrams/ctx.svg" alt="ctx"></a>'
}

# expect <name> <want: pass|fail> <site dir> [pending file]
expect() {
  local name="$1" want="$2" site="$3" pending="${4:-}" got
  if [ -n "${pending}" ]; then
    if python3 -I "${checker}" check "${site}" "${pending}" > "${work}/out" 2>&1; then got=pass; else got=fail; fi
  else
    if python3 -I "${checker}" check "${site}" > "${work}/out" 2>&1; then got=pass; else got=fail; fi
  fi
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got ${got}, want ${want}: $(tail -n 3 "${work}/out")"
    fails=$((fails + 1))
  fi
}

mentions() { # name pattern
  if grep -q -- "$2" "${work}/out"; then
    echo "ok   ${1} message"
  else
    echo "FAIL ${1} message: expected '$2' in $(cat "${work}/out")"
    fails=$((fails + 1))
  fi
}

s="${work}/site"

build "${s}"
expect "valid built site passes" pass "${s}"

build "${s}"
page "${s}/guide/index.html" "../stylesheets/brand.css" '<h2 id="step">Step</h2><a href="../missing/">Missing</a>'
expect "broken internal link fails" fail "${s}"
mentions "broken internal link" "broken link ../missing/"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<a href="guide/#no-such-anchor">Bad</a>'
expect "broken anchor fails" fail "${s}"
mentions "broken anchor" "no #no-such-anchor"

build "${s}"
page "${s}/guide/index.html" "../stylesheets/brand.css" '<h2 id="step">Step</h2><img src="../diagrams/missing.svg" alt="missing">'
expect "missing image fails" fail "${s}"
mentions "missing image" "missing image ../diagrams/missing.svg"

build "${s}"
page "${s}/guide/index.html" "../stylesheets/absent.css" '<h2 id="step">Step</h2>'
expect "missing stylesheet fails" fail "${s}"
mentions "missing stylesheet" "missing stylesheet ../stylesheets/absent.css"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<script src="https://cdn.example.invalid/app.js"></script>'
expect "third-party script fails" fail "${s}"
mentions "third-party script" "third-party script https://cdn.example.invalid/app.js"

build "${s}"
page "${s}/index.html" "https://fonts.example.invalid/style.css" '<h1 id="top">Home</h1>'
expect "third-party stylesheet fails" fail "${s}"
mentions "third-party stylesheet" "third-party stylesheet https://fonts.example.invalid/style.css"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<img src="https://img.example.invalid/x.png" alt="x">'
expect "third-party image fails" fail "${s}"
mentions "third-party image" "third-party image https://img.example.invalid/x.png"

build "${s}"
: > "${s}/diagrams/a,b.svg"
page "${s}/index.html" "./stylesheets/brand.css" '<img src="diagrams/ctx.svg" srcset="diagrams/a,b.svg 2x" alt="ctx">'
expect "comma inside a srcset URL stays in the URL" pass "${s}"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<img src="diagrams/ctx.svg" srcset="diagrams/ctx.svg 1x, https://img.example.invalid/x.png 2x" alt="ctx">'
expect "third-party img srcset candidate fails" fail "${s}"
mentions "third-party img srcset candidate" "third-party image https://img.example.invalid/x.png"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<picture><source srcset="diagrams/ctx.svg 1x, https://img.example.invalid/dark.png 2x"><img src="diagrams/ctx.svg" alt="ctx"></picture>'
expect "third-party picture source srcset candidate fails" fail "${s}"
mentions "third-party picture source srcset candidate" "third-party image https://img.example.invalid/dark.png"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<img src="diagrams/ctx.svg" srcset="diagrams/ctx.svg 1x, diagrams/missing.svg 2x" alt="ctx">'
expect "missing local img srcset candidate fails" fail "${s}"
mentions "missing local img srcset candidate" "missing image diagrams/missing.svg"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<picture><source srcset="diagrams/ctx.svg 1x, ./diagrams/ctx.svg 2x"><img src="diagrams/ctx.svg" srcset="diagrams/ctx.svg 1x" alt="ctx"></picture>'
expect "local img and picture source srcset candidates pass" pass "${s}"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<a href="/guide/">Root outside base</a>'
expect "root-relative link outside base path fails" fail "${s}"
mentions "root-relative link outside base path" "/sablecourt/"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<a href="/sablecourt/">Root home</a> <a href="/sablecourt/guide/#step">Root guide</a>'
expect "root-relative link under base path passes" pass "${s}"

build "${s}"
mkdir -p "${s}/old"
printf '<!doctype html><html><head><meta http-equiv="refresh" content="0; url=../gone/"></head></html>\n' > "${s}/old/index.html"
expect "broken redirect fails" fail "${s}"
mentions "broken redirect" "broken redirect ../gone/"

build "${s}"
page "${s}/index.html" "./stylesheets/brand.css" '<a href="planned/">Planned</a>'
printf 'docs/index.md -> docs/planned.md\n' > "${work}/pending.txt"
expect "pending entry allows its listed link" pass "${s}" "${work}/pending.txt"

build "${s}"
page "${s}/guide/index.html" "../stylesheets/brand.css" '<h2 id="step">Step</h2><a href="../planned/">Planned</a>'
expect "pending entry does not cover another source page" fail "${s}" "${work}/pending.txt"

build "${s}"
mkdir -p "${work}/cmd/app"
printf 'package main\n' > "${work}/cmd/app/main.go"
page "${s}/guide/index.html" "../stylesheets/brand.css" '<h2 id="step">Step</h2><a href="../../cmd/app/main.go">Source</a>'
expect "link escaping site fails with repository URL hint" fail "${s}"
mentions "link escaping site" "repository URL"

build "${s}"
page "${s}/guide/index.html" "../stylesheets/brand.css" '<h2 id="step">Step</h2><img src="../../cmd/app/main.go" alt="bad">'
expect "image escaping site fails" fail "${s}"

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
