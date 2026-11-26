#!/usr/bin/env bash
# Offline tests of the release helpers: check-release-tag.sh, floating-tags.sh, release-state.sh,
# publish-release.sh (both with a gh stub) and release-verify-notes.sh. Usage: hack/test/release-scripts_test.sh
# pass and fail always return 0, so `test && pass || fail` cannot run both.
# shellcheck disable=SC2015
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
hack="${root}/hack"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0

pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }
# expect_exit <name> <want code> <command...>
expect_exit() {
  local name="$1" want="$2" got=0
  shift 2
  "$@" > "${work}/out" 2> "${work}/err" || got=$?
  if [ "${got}" = "${want}" ]; then pass "${name}"; else fail "${name}: exit ${got}, want ${want} ($(tail -n 1 "${work}/err"))"; fi
}

# --- check-release-tag.sh
echo '{".":"0.1.0"}' > "${work}/m-010.json"
echo '{}' > "${work}/m-empty.json"
expect_exit "stable tag equal to the manifest" 0 "${hack}/check-release-tag.sh" v0.1.0 "${work}/m-010.json"
expect_exit "stable tag not in the manifest" 1 "${hack}/check-release-tag.sh" v0.2.0 "${work}/m-010.json"
expect_exit "stable tag with an empty manifest" 1 "${hack}/check-release-tag.sh" v0.1.0 "${work}/m-empty.json"
expect_exit "older stable tag" 1 "${hack}/check-release-tag.sh" v0.0.9 "${work}/m-010.json"
expect_exit "candidate for the next release" 0 "${hack}/check-release-tag.sh" v0.2.0-rc.1 "${work}/m-010.json"
expect_exit "candidate for the first release" 0 "${hack}/check-release-tag.sh" v0.1.0-rc.1 "${work}/m-empty.json"
expect_exit "candidate for a released version" 1 "${hack}/check-release-tag.sh" v0.1.0-rc.2 "${work}/m-010.json"
expect_exit "candidate for an older version" 1 "${hack}/check-release-tag.sh" v0.0.5-rc.1 "${work}/m-010.json"
expect_exit "other pre-release form" 1 "${hack}/check-release-tag.sh" v0.2.0-beta.1 "${work}/m-010.json"
expect_exit "rc.0" 1 "${hack}/check-release-tag.sh" v0.2.0-rc.0 "${work}/m-010.json"
expect_exit "leading zero" 1 "${hack}/check-release-tag.sh" v0.02.0 "${work}/m-010.json"
expect_exit "build metadata" 1 "${hack}/check-release-tag.sh" v0.1.0+x "${work}/m-010.json"
expect_exit "minor version compared numerically" 0 "${hack}/check-release-tag.sh" v0.10.0-rc.1 "${work}/m-010.json"

# --- floating-tags.sh
repo="${work}/tags"
git init -q "${repo}"
git -C "${repo}" -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false commit -q --allow-empty -m base
for t in v0.1.0 v0.1.1 v0.2.0 v0.3.0-rc.1; do git -C "${repo}" tag "${t}"; done
floating() { (cd "${repo}" && "${hack}/floating-tags.sh" "$1" | tr '\n' ' '); }
[ "$(floating v0.2.0)" = "latest=true minor=true " ] && pass "newest stable takes latest and X.Y" || fail "newest stable: $(floating v0.2.0)"
[ "$(floating v0.1.1)" = "latest=false minor=true " ] && pass "older line keeps its X.Y, not latest" || fail "older line: $(floating v0.1.1)"
[ "$(floating v0.1.0)" = "latest=false minor=false " ] && pass "older patch moves nothing" || fail "older patch: $(floating v0.1.0)"
[ "$(floating v0.3.0-rc.1)" = "latest=false minor=false " ] && pass "candidate moves nothing" || fail "candidate: $(floating v0.3.0-rc.1)"
[ "$(floating main)" = "latest=false minor=false " ] && pass "branch moves nothing" || fail "branch: $(floating main)"
git -C "${repo}" tag v0.10.0
[ "$(floating v0.2.0)" = "latest=false minor=true " ] && pass "versions compared numerically" || fail "numeric: $(floating v0.2.0)"

# --- gh stub
stub="${work}/bin"
mkdir -p "${stub}"
cat > "${stub}/gh" <<'STUB'
#!/usr/bin/env bash
# gh stub: `release view` prints $STUB_VIEW_JSON (applying --jq), or fails like gh when $STUB_NOT_FOUND is set;
# `release edit` is logged to $STUB_LOG.
set -euo pipefail
if [ "$1 $2" = "release view" ]; then
  if [ -n "${STUB_NOT_FOUND:-}" ]; then echo "release not found" >&2; exit 1; fi
  jqexpr=""
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "--jq" ]; then jqexpr="$2"; fi
    shift
  done
  if [ -n "${jqexpr}" ]; then jq -r "${jqexpr}" <<< "${STUB_VIEW_JSON}"; else printf '%s\n' "${STUB_VIEW_JSON}"; fi
  exit 0
fi
if [ "$1 $2" = "release edit" ]; then echo "$*" >> "${STUB_LOG}"; exit 0; fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
chmod +x "${stub}/gh"
export STUB_LOG="${work}/gh.log"

state() { PATH="${stub}:${PATH}" STUB_VIEW_JSON="$1" STUB_NOT_FOUND="${2:-}" "${hack}/release-state.sh" o/r v0.1.0; }
[ "$(state '{"isDraft":false}')" = "published=true" ] && pass "release-state: published" || fail "release-state: published"
[ "$(state '{"isDraft":true}')" = "published=false" ] && pass "release-state: draft" || fail "release-state: draft"
[ "$(state '{}' yes)" = "published=false" ] && pass "release-state: not found" || fail "release-state: not found"
expect_exit "release-state: no boolean draft state" 1 env PATH="${stub}:${PATH}" STUB_VIEW_JSON='{}' "${hack}/release-state.sh" o/r v0.1.0

# --- publish-release.sh
dA="sha256:$(printf 'a%.0s' $(seq 64))" dB="sha256:$(printf 'b%.0s' $(seq 64))"
dist="${work}/dist"
mkdir -p "${dist}"
make_dist() { # version
  rm -f "${dist}"/*
  for os in linux darwin; do for arch in amd64 arm64; do echo "${os}${arch}" > "${dist}/sablecourt_$1_${os}_${arch}.tar.gz"; done; done
  echo sums > "${dist}/checksums.txt"
  echo bundle > "${dist}/checksums.txt.sigstore.json"
}
release_json() { # tag isDraft isPrerelease [tamper]
  local notes assets f
  notes="$("${hack}/release-verify-notes.sh" o/r "$1" "${dA}" "${dB}" < /dev/null)"
  assets="[]"
  for f in "${dist}"/*; do
    sum="$(sha256sum "${f}")"
    assets="$(jq --arg n "$(basename "${f}")" --arg d "sha256:${sum%% *}" '. + [{name: $n, state: "uploaded", size: 10, digest: $d}]' <<< "${assets}")"
  done
  if [ "${4:-}" = tamper ]; then assets="$(jq '.[0].digest = "sha256:0000"' <<< "${assets}")"; fi
  jq -n --arg tag "$1" --arg body "${notes}" --argjson assets "${assets}" --argjson d "$2" --argjson p "$3" \
    '{tagName: $tag, body: $body, assets: $assets} + (if $d == null then {} else {isDraft: $d} end) + {isPrerelease: $p}'
}
publish() { # tag json
  : > "${STUB_LOG}"
  PATH="${stub}:${PATH}" STUB_VIEW_JSON="$2" RELEASE_WAIT_SECONDS=0 RELEASE_POLL_SECONDS=0 \
    "${hack}/publish-release.sh" o/r "$1" "${dA}" "${dB}" "${dist}" > "${work}/out" 2> "${work}/err"
}
make_dist 0.1.0
if publish v0.1.0 "$(release_json v0.1.0 true false)" && grep -q -- '--draft=false' "${STUB_LOG}"; then pass "publish: complete draft is published"; else fail "publish: complete draft ($(cat "${work}/err"))"; fi
if publish v0.1.0 "$(release_json v0.1.0 false false)" && [ ! -s "${STUB_LOG}" ]; then pass "publish: published and complete is a no-op"; else fail "publish: no-op"; fi
if ! publish v0.1.0 "$(release_json v0.1.0 null false)" && [ ! -s "${STUB_LOG}" ]; then pass "publish: missing draft state refused"; else fail "publish: missing draft state accepted"; fi
make_dist 0.2.0-rc.1
if ! publish v0.2.0-rc.1 "$(release_json v0.2.0-rc.1 true false)" && [ ! -s "${STUB_LOG}" ]; then pass "publish: candidate without pre-release flag refused"; else fail "publish: rc flag"; fi
if publish v0.2.0-rc.1 "$(release_json v0.2.0-rc.1 true true)"; then pass "publish: candidate with pre-release flag"; else fail "publish: rc with flag ($(cat "${work}/err"))"; fi
make_dist 0.1.0
if ! publish v0.1.0 "$(release_json v0.1.0 true true)"; then pass "publish: stable with pre-release flag refused"; else fail "publish: stable flagged as pre-release accepted"; fi
if ! publish v0.1.0 "$(release_json v0.1.0 true false tamper)" && [ ! -s "${STUB_LOG}" ]; then pass "publish: asset digest mismatch refused"; else fail "publish: digest mismatch accepted"; fi
json="$(release_json v0.1.0 true false | jq '.assets |= .[1:]')"
if ! publish v0.1.0 "${json}" && [ ! -s "${STUB_LOG}" ]; then pass "publish: missing asset refused"; else fail "publish: missing asset accepted"; fi
json="$(release_json v0.1.0 true false | jq '.body = "notes"')"
if ! publish v0.1.0 "${json}"; then pass "publish: missing verification section refused"; else fail "publish: missing section accepted"; fi

# --- release-verify-notes.sh
notes() { "${hack}/release-verify-notes.sh" o/r v0.1.0 "${dA}" "${dB}"; }
first="$(printf 'intro\n' | notes)"
again="$(printf '%s\n' "${first}" | notes)"
[ "${first}" = "${again}" ] && pass "notes: rerun is idempotent" || fail "notes: not idempotent"
[ "$(grep -c 'sablecourt:verify:start' <<< "${again}")" = 1 ] && pass "notes: one section" || fail "notes: section count"
out="$(printf 'intro\n<!-- sablecourt:verify:start -->\nkept line\n' | notes)"
grep -q '^kept line$' <<< "${out}" && pass "notes: unmatched opening marker keeps the rest" || fail "notes: unmatched marker dropped text"
expect_exit "notes: bad digest refused" 1 "${hack}/release-verify-notes.sh" o/r v0.1.0 bad "${dB}"

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
