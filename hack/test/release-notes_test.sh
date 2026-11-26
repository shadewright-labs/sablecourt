#!/usr/bin/env bash
# Table tests of hack/release-notes.sh: extracts one version's section from CHANGELOG.md for release.yml,
# handling unlinked headings (first release), linked minor headings (## [X.Y.Z](...)), linked patch headings
# (### [X.Y.Z](...)) with ### subsections inside, and failing when no non-empty section exists.
# Usage: hack/test/release-notes_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/release-notes.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0

cat > "${work}/CHANGELOG.md" <<'MD'
# Changelog

## [0.2.0](https://github.com/shadewright-labs/sablecourt/compare/v0.1.1...v0.2.0) (2026-12-01)

### Features

* add council summaries

### Bug fixes

* handle empty handoff

### [0.1.1](https://github.com/shadewright-labs/sablecourt/compare/v0.1.0...v0.1.1) (2026-11-28)

### Bug fixes

* fix race in scheduler

## 0.1.0 (2026-11-26)

### Features

* initial release

## [0.0.9](https://github.com/shadewright-labs/sablecourt/compare/v0.0.8...v0.0.9) (2026-11-20)

## [0.1.00](https://github.com/shadewright-labs/sablecourt/compare/v0.0.9...v0.1.00) (2026-11-21)

### Features

* should not match 0.1.0
MD

# <name> <version> <file> <want_rc> <want_out>
case_() {
  local name="$1" ver="$2" file="$3" want_rc="$4" want_out="$5" out rc=0
  out="$(bash "${script}" "${ver}" "${file}" 2> /dev/null)" || rc=$?
  if [ "${rc}" = "${want_rc}" ] && [ "${out}" = "${want_out}" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got exit ${rc} out='${out}', want exit ${want_rc} out='${want_out}'"
    fails=$((fails + 1))
  fi
}

case_ "linked minor heading extracts all subsections up to patch heading" 0.2.0 "${work}/CHANGELOG.md" 0 $'### Features\n\n* add council summaries\n\n### Bug fixes\n\n* handle empty handoff'
case_ "linked patch heading keeps its ### subsection and stops at next version" 0.1.1 "${work}/CHANGELOG.md" 0 $'### Bug fixes\n\n* fix race in scheduler'
case_ "unlinked first-release heading extracts section" 0.1.0 "${work}/CHANGELOG.md" 0 $'### Features\n\n* initial release'
case_ "empty section fails" 0.0.9 "${work}/CHANGELOG.md" 1 ""
case_ "missing version fails" 0.3.0 "${work}/CHANGELOG.md" 1 ""
case_ "missing file fails" 0.1.0 "${work}/missing.md" 1 ""
case_ "pre-release version is rejected as usage error" 0.1.0-rc.1 "${work}/CHANGELOG.md" 2 ""
case_ "v-prefixed version is rejected as usage error" v0.1.0 "${work}/CHANGELOG.md" 2 ""

rc=0
bash "${script}" > /dev/null 2>&1 || rc=$?
if [ "${rc}" = 2 ]; then
  echo "ok   missing arguments are a usage error"
else
  echo "FAIL missing arguments are a usage error: got exit ${rc}, want 2"
  fails=$((fails + 1))
fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
