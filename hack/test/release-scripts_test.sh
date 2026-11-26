#!/usr/bin/env bash
# Offline tests of the release helpers, with stubs for gh, docker and cosign: check-release-tag.sh,
# floating-tags.sh, release-state.sh, publish-release.sh, upload-release-assets.sh, release-verify-notes.sh,
# resolve-image-digests.sh (with verify-image.sh) and promote-images.sh. Usage: hack/test/release-scripts_test.sh
# pass and fail always return 0, so `test && pass || fail` cannot run both.
# shellcheck disable=SC2015
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
hack="${root}/hack"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0
test_email="release-test@example.invalid"
git_q() { git -c user.name=test -c user.email="${test_email}" -c commit.gpgsign=false "$@"; }

pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }
# expect_exit <name> <want code> <command...>
expect_exit() {
  local name="$1" want="$2" got=0
  shift 2
  "$@" > "${work}/out" 2> "${work}/err" || got=$?
  if [ "${got}" = "${want}" ]; then pass "${name}"; else fail "${name}: exit ${got}, want ${want} ($(tail -n 1 "${work}/err"))"; fi
}

# A repository with tags, for the helpers that read `git tag -l`.
tagrepo() { # dir tags...
  local dir="$1" t
  shift
  rm -rf "${dir}"
  git_q init -q "${dir}"
  git_q -C "${dir}" commit -q --allow-empty -m base
  for t in "$@"; do git_q -C "${dir}" tag "${t}"; done
}
big=18446744073709551616 # 2^64: overflows shell arithmetic

# --- check-release-tag.sh (run inside a repository whose tags it reads)
m="${work}/manifests"
mkdir -p "${m}"
echo '{".":"0.1.0"}' > "${m}/010.json"
echo '{}' > "${m}/empty-object.json"
: > "${m}/empty-file.json"
printf '  \n' > "${m}/blank.json"
echo 'null' > "${m}/null.json"
echo '{".":false}' > "${m}/false.json"
echo '{".":"1.bad"}' > "${m}/bad-version.json"
echo '{".":"01.0.0"}' > "${m}/leading-zero.json"
echo '{".":"0.1.0","extra":"0.1.0"}' > "${m}/extra-key.json"
echo '[]' > "${m}/array.json"
echo '{} {}' > "${m}/two-values.json"
echo '{".":"0.1.0"' > "${m}/broken.json"
echo '{".":"3.8.4"}' > "${m}/384.json"
tagrepo "${work}/notags"
tagrepo "${work}/released" v3.8.4 v3.8.5 v3.9.0-rc.1
tagrepo "${work}/bigtag" v0.1.0 "v${big}.0.0"
crt() { # repo tag manifest
  (cd "$1" && "${hack}/check-release-tag.sh" "$2" "$3")
}
expect_exit "stable tag equal to the manifest" 0 crt "${work}/notags" v0.1.0 "${m}/010.json"
expect_exit "stable tag not in the manifest" 1 crt "${work}/notags" v0.2.0 "${m}/010.json"
expect_exit "stable tag with an empty manifest" 1 crt "${work}/notags" v0.1.0 "${m}/empty-object.json"
expect_exit "older stable tag" 1 crt "${work}/notags" v0.0.9 "${m}/010.json"
expect_exit "candidate for the next release" 0 crt "${work}/notags" v0.2.0-rc.1 "${m}/010.json"
expect_exit "candidate for the first release" 0 crt "${work}/notags" v0.1.0-rc.1 "${m}/empty-object.json"
expect_exit "candidate for a released version" 1 crt "${work}/notags" v0.1.0-rc.2 "${m}/010.json"
expect_exit "candidate for an older version" 1 crt "${work}/notags" v0.0.5-rc.1 "${m}/010.json"
expect_exit "other pre-release form" 1 crt "${work}/notags" v0.2.0-beta.1 "${m}/010.json"
expect_exit "rc.0" 1 crt "${work}/notags" v0.2.0-rc.0 "${m}/010.json"
expect_exit "leading zero" 1 crt "${work}/notags" v0.02.0 "${m}/010.json"
expect_exit "build metadata" 1 crt "${work}/notags" v0.1.0+x "${m}/010.json"
expect_exit "minor version compared numerically" 0 crt "${work}/notags" v0.10.0-rc.1 "${m}/010.json"
# Malformed manifests: only {} is the bootstrap case.
for f in empty-file blank null false bad-version leading-zero extra-key array two-values broken; do
  expect_exit "candidate refused with a ${f} manifest" 1 crt "${work}/notags" v3.8.6-rc.1 "${m}/${f}.json"
done
expect_exit "stable refused with a null manifest" 1 crt "${work}/notags" v0.1.0 "${m}/null.json"
expect_exit "missing manifest" 1 crt "${work}/notags" v0.1.0-rc.1 "${m}/absent.json"
# A candidate on an old commit, whose manifest predates a release that exists now.
expect_exit "candidate for a version released since (old manifest)" 1 crt "${work}/released" v3.8.5-rc.1 "${m}/384.json"
expect_exit "candidate older than the newest release" 1 crt "${work}/released" v3.8.4-rc.2 "${m}/empty-object.json"
expect_exit "candidate newer than every release" 0 crt "${work}/released" v3.8.6-rc.1 "${m}/384.json"
expect_exit "candidate tags do not count as releases" 0 crt "${work}/released" v3.9.0-rc.2 "${m}/384.json"
expect_exit "candidate below a release with a 20-digit major" 1 crt "${work}/bigtag" v3.9.0-rc.1 "${m}/010.json"
expect_exit "candidate above a release with a 20-digit major" 0 crt "${work}/bigtag" "v${big}.0.1-rc.1" "${m}/010.json"

# --- floating-tags.sh
repo="${work}/tags"
tagrepo "${repo}" v0.1.0 v0.1.1 v0.2.0 v0.3.0-rc.1
floating() { (cd "${repo}" && "${hack}/floating-tags.sh" "$1" | tr '\n' ' '); }
[ "$(floating v0.2.0)" = "latest=true minor=true " ] && pass "newest stable takes latest and X.Y" || fail "newest stable: $(floating v0.2.0)"
[ "$(floating v0.1.1)" = "latest=false minor=true " ] && pass "older line keeps its X.Y, not latest" || fail "older line: $(floating v0.1.1)"
[ "$(floating v0.1.0)" = "latest=false minor=false " ] && pass "older patch moves nothing" || fail "older patch: $(floating v0.1.0)"
[ "$(floating v0.3.0-rc.1)" = "latest=false minor=false " ] && pass "candidate moves nothing" || fail "candidate: $(floating v0.3.0-rc.1)"
[ "$(floating main)" = "latest=false minor=false " ] && pass "branch moves nothing" || fail "branch: $(floating main)"
git -C "${repo}" tag v0.10.0
[ "$(floating v0.2.0)" = "latest=false minor=true " ] && pass "versions compared numerically" || fail "numeric: $(floating v0.2.0)"
tagrepo "${repo}" v3.8.4
[ "$(floating v3.8.4)" = "latest=true minor=true " ] && pass "v3.8.4 alone takes both" || fail "v3.8.4 alone: $(floating v3.8.4)"
git -C "${repo}" tag v3.8.5
[ "$(floating v3.8.4)" = "latest=false minor=false " ] && pass "v3.8.4 decided after v3.8.5 exists takes neither" || fail "v3.8.4 after v3.8.5: $(floating v3.8.4)"
tagrepo "${repo}" v0.2.0 "v${big}.0.0"
[ "$(floating v0.2.0)" = "latest=false minor=true " ] && pass "a 20-digit major is newer" || fail "20-digit major: $(floating v0.2.0)"
tagrepo "${repo}" v0.2.0 "v0.${big}.0"
[ "$(floating v0.2.0)" = "latest=false minor=true " ] && pass "a 20-digit minor is newer" || fail "20-digit minor: $(floating v0.2.0)"
tagrepo "${repo}" v0.2.0 "v0.2.${big}"
[ "$(floating v0.2.0)" = "latest=false minor=false " ] && pass "a 20-digit patch is newer" || fail "20-digit patch: $(floating v0.2.0)"
tagrepo "${repo}" v0.2.0 "v0.2.9" "v0.2.10"
[ "$(floating v0.2.9)" = "latest=false minor=false " ] && pass "patch 10 is newer than 9" || fail "patch 10 vs 9: $(floating v0.2.9)"

# --- stubs
stub="${work}/bin"
mkdir -p "${stub}"
cat > "${stub}/gh" <<'STUB'
#!/usr/bin/env bash
# gh stub. `release view`: the Nth call prints ${STUB_DIR}/view.N (or view.last when there is no view.N), applying
# --jq; a file holding `ERR <message>` makes the call fail with that message on stderr. `release edit` and
# `release upload` are logged to ${STUB_DIR}/log; an upload fails while ${STUB_DIR}/upload-fail has lines left.
# `attestation verify` succeeds only when --source-ref and --source-digest equal STUB_PROV_REF and STUB_PROV_SHA.
set -euo pipefail
d="${STUB_DIR}"
case "$1 $2" in
  "release view")
    n=$(( $(cat "${d}/views" 2>/dev/null || echo 0) + 1 ))
    echo "${n}" > "${d}/views"
    f="${d}/view.${n}"
    [ -f "${f}" ] || f="${d}/view.last"
    body="$(cat "${f}")"
    if [ "${body#ERR }" != "${body}" ]; then echo "${body#ERR }" >&2; exit 1; fi
    jqexpr=""
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--jq" ]; then jqexpr="$2"; fi
      shift
    done
    if [ -n "${jqexpr}" ]; then jq -r "${jqexpr}" <<< "${body}"; else printf '%s\n' "${body}"; fi
    ;;
  "release edit") echo "edit $*" >> "${d}/log" ;;
  "release upload")
    echo "upload $*" >> "${d}/log"
    if [ -s "${d}/upload-fail" ]; then sed -i.bak '1d' "${d}/upload-fail"; echo "HTTP 502" >&2; exit 1; fi
    ;;
  "attestation verify")
    ref="" sha=""
    while [ "$#" -gt 0 ]; do
      case "$1" in --source-ref) ref="$2" ;; --source-digest) sha="$2" ;; esac
      shift
    done
    [ "${ref}" = "${STUB_PROV_REF:-}" ] && [ "${sha}" = "${STUB_PROV_SHA:-}" ] || { echo "no matching attestation" >&2; exit 1; }
    ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
STUB
cat > "${stub}/cosign" <<'STUB'
#!/usr/bin/env bash
# cosign stub: verify and verify-attestation succeed unless STUB_COSIGN_FAIL names the subcommand.
case "$1" in
  verify|verify-attestation) [ "${STUB_COSIGN_FAIL:-}" != "$1" ] || exit 1 ;;
  *) echo "unexpected cosign call: $*" >&2; exit 1 ;;
esac
STUB
cat > "${stub}/docker" <<'STUB'
#!/usr/bin/env bash
# docker stub for `buildx imagetools`: `inspect <ref:tag> --format ...` prints the digest stored for that tag in
# ${STUB_DIR}/tags/ (or fails), `inspect --raw <ref@digest>` prints an index with linux/amd64 and linux/arm64
# children, `create --tag <ref:tag> <ref@digest>` stores the digest (logged; fails for a tag in STUB_CREATE_FAIL,
# stores STUB_CREATE_WRONG instead when set).
set -euo pipefail
d="${STUB_DIR}"
[ "$1 $2" = "buildx imagetools" ] || { echo "unexpected docker call: $*" >&2; exit 1; }
shift 2
key() { printf '%s' "$1" | tr '/:@' '___'; }
case "$1" in
  inspect)
    if [ "$2" = --raw ]; then
      printf '{"manifests":[{"digest":"sha256:%s","platform":{"os":"linux","architecture":"amd64"}},{"digest":"sha256:%s","platform":{"os":"linux","architecture":"arm64"}},{"digest":"sha256:%s","platform":{"os":"unknown","architecture":"unknown"}}]}\n' \
        "$(printf '1%.0s' $(seq 64))" "$(printf '2%.0s' $(seq 64))" "$(printf '3%.0s' $(seq 64))"
      exit 0
    fi
    f="${d}/tags/$(key "$2")"
    [ -f "${f}" ] || { echo "not found" >&2; exit 1; }
    printf '"%s"\n' "$(cat "${f}")"
    ;;
  create)
    echo "create $3 $4" >> "${d}/log"
    if [ -n "${STUB_CREATE_FAIL:-}" ] && [ "$3" = "${STUB_CREATE_FAIL}" ]; then exit 1; fi
    mkdir -p "${d}/tags"
    printf '%s' "${STUB_CREATE_WRONG:-${4#*@}}" > "${d}/tags/$(key "$3")"
    ;;
  *) echo "unexpected imagetools call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${stub}/gh" "${stub}/cosign" "${stub}/docker"
export STUB_DIR="${work}/stub"
reset_stub() { rm -rf "${STUB_DIR}"; mkdir -p "${STUB_DIR}"; : > "${STUB_DIR}/log"; }
views() { # json-or-ERR... : the answers of successive `gh release view` calls; the last one repeats
  local i=0 v
  for v in "$@"; do i=$((i + 1)); printf '%s\n' "${v}" > "${STUB_DIR}/view.${i}"; done
  cp "${STUB_DIR}/view.${i}" "${STUB_DIR}/view.last"
}
writes() { grep -c -e '^edit' -e '^upload' -e '^create' "${STUB_DIR}/log" || true; }
run() { PATH="${stub}:${PATH}" "$@"; }

# --- release-state.sh
state() { reset_stub; views "$@"; run "${hack}/release-state.sh" o/r v0.1.0; }
[ "$(state '{"isDraft":false}')" = "published=true" ] && pass "release-state: published" || fail "release-state: published"
[ "$(state '{"isDraft":true}')" = "published=false" ] && pass "release-state: draft" || fail "release-state: draft"
[ "$(state 'ERR release not found')" = "published=false" ] && pass "release-state: not found" || fail "release-state: not found"
expect_exit "release-state: no boolean draft state" 1 state '{}'
expect_exit "release-state: missing gh configuration fails" 1 state 'ERR configuration file not found'
expect_exit "release-state: server error fails" 1 state 'ERR HTTP 503: Service Unavailable'
expect_exit "release-state: a repository not found fails" 1 state 'ERR GraphQL: Could not resolve to a Repository (repository not found)'
expect_exit "release-state: not found with more text fails" 1 state 'ERR release not found; token expired'

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
  jq -cn --arg tag "$1" --arg body "${notes}" --argjson assets "${assets}" --argjson d "$2" --argjson p "$3" \
    '{tagName: $tag, body: $body, assets: $assets} + (if $d == null then {} else {isDraft: $d} end) + {isPrerelease: $p}'
}
extra_asset='.assets += [{name: "extra.zip", state: "uploaded", size: 5, digest: "sha256:abcd"}]'
publish() { # tag [verify-only] -- views...
  local tag="$1" mode=""
  shift
  if [ "$1" = verify-only ]; then mode=verify-only; shift; fi
  shift # --
  reset_stub
  views "$@"
  RELEASE_WAIT_SECONDS=0 RELEASE_POLL_SECONDS=0 run "${hack}/publish-release.sh" o/r "${tag}" "${dA}" "${dB}" "${dist}" ${mode:+"${mode}"} > "${work}/out" 2> "${work}/err"
}
make_dist 0.1.0
complete_draft="$(release_json v0.1.0 true false)"
complete_public="$(release_json v0.1.0 false false)"
if publish v0.1.0 -- "${complete_draft}" && grep -q -- '--draft=false' "${STUB_DIR}/log"; then pass "publish: complete draft is published"; else fail "publish: complete draft ($(cat "${work}/err"))"; fi
if publish v0.1.0 -- "${complete_public}" && [ "$(writes)" = 0 ]; then pass "publish: published and complete is a no-op"; else fail "publish: no-op"; fi
if ! publish v0.1.0 -- "$(release_json v0.1.0 null false)" && [ "$(writes)" = 0 ]; then pass "publish: missing draft state refused"; else fail "publish: missing draft state accepted"; fi
make_dist 0.2.0-rc.1
if ! publish v0.2.0-rc.1 -- "$(release_json v0.2.0-rc.1 true false)" && [ "$(writes)" = 0 ]; then pass "publish: candidate without pre-release flag refused"; else fail "publish: rc flag"; fi
if publish v0.2.0-rc.1 -- "$(release_json v0.2.0-rc.1 true true)"; then pass "publish: candidate with pre-release flag"; else fail "publish: rc with flag ($(cat "${work}/err"))"; fi
make_dist 0.1.0
if ! publish v0.1.0 -- "$(release_json v0.1.0 true true)"; then pass "publish: stable with pre-release flag refused"; else fail "publish: stable flagged as pre-release accepted"; fi
if ! publish v0.1.0 -- "$(release_json v0.1.0 true false tamper)" && [ "$(writes)" = 0 ]; then pass "publish: asset digest mismatch refused"; else fail "publish: digest mismatch accepted"; fi
if ! publish v0.1.0 -- "$(jq -c '.assets |= .[1:]' <<< "${complete_draft}")" && [ "$(writes)" = 0 ]; then pass "publish: missing asset refused"; else fail "publish: missing asset accepted"; fi
if ! publish v0.1.0 -- "$(jq -c '.body = "notes"' <<< "${complete_draft}")"; then pass "publish: missing verification section refused"; else fail "publish: missing section accepted"; fi
# An asset nobody verified.
if ! publish v0.1.0 -- "$(jq -c "${extra_asset}" <<< "${complete_draft}")" && [ "$(writes)" = 0 ]; then pass "publish: draft with an extra asset refused"; else fail "publish: extra asset accepted"; fi
if ! publish v0.1.0 -- "$(jq -c '.assets += [.assets[0]]' <<< "${complete_draft}")" && [ "$(writes)" = 0 ]; then pass "publish: duplicate asset refused"; else fail "publish: duplicate asset accepted"; fi
if ! publish v0.1.0 -- "$(jq -c '.assets[0].name = "sablecourt_0.1.0_linux_amd64.tar.gz.bak"' <<< "${complete_draft}")" && [ "$(writes)" = 0 ]; then pass "publish: renamed asset refused"; else fail "publish: renamed asset accepted"; fi
# A public release is never written and must be complete.
if ! publish v0.1.0 -- "$(jq -c '.assets = []' <<< "${complete_public}")" && [ "$(writes)" = 0 ]; then pass "publish: public release with zero assets fails"; else fail "publish: public release with zero assets passed"; fi
if ! publish v0.1.0 -- "$(jq -c "${extra_asset}" <<< "${complete_public}")" && [ "$(writes)" = 0 ]; then pass "publish: public release with an extra asset fails"; else fail "publish: public extra asset passed"; fi
if ! publish v0.1.0 -- "$(jq -c '.body = "notes"' <<< "${complete_public}")" && [ "$(writes)" = 0 ]; then pass "publish: public release without the section fails"; else fail "publish: public without section passed"; fi
# The draft is read again just before publishing.
if ! publish v0.1.0 -- "${complete_draft}" "${complete_public}" && [ "$(writes)" = 0 ]; then pass "publish: draft published by someone else meanwhile is not edited"; else fail "publish: reread published"; fi
if ! publish v0.1.0 -- "${complete_draft}" "$(jq -c "${extra_asset}" <<< "${complete_draft}")" && [ "$(writes)" = 0 ]; then pass "publish: asset added before the edit stops it"; else fail "publish: reread extra asset"; fi
if ! publish v0.1.0 -- "${complete_draft}" "ERR HTTP 503" && [ "$(writes)" = 0 ]; then pass "publish: failed reread stops it"; else fail "publish: reread error"; fi
# verify-only never writes.
if publish v0.1.0 verify-only -- "${complete_public}" && [ "$(writes)" = 0 ]; then pass "verify-only: complete public release passes"; else fail "verify-only: complete public ($(cat "${work}/err"))"; fi
if ! publish v0.1.0 verify-only -- "${complete_draft}" && [ "$(writes)" = 0 ]; then pass "verify-only: a draft fails without a write"; else fail "verify-only: draft"; fi
if ! publish v0.1.0 verify-only -- "$(jq -c '.assets = []' <<< "${complete_public}")"; then pass "verify-only: public release with zero assets fails"; else fail "verify-only: zero assets passed"; fi

# --- upload-release-assets.sh
upload() { # views...
  reset_stub
  views "$@"
  ATTEMPTS=3 POLL_SECONDS=0 run "${hack}/upload-release-assets.sh" o/r v0.1.0 "${dist}" > "${work}/out" 2> "${work}/err"
}
uploads() { grep -c '^upload' "${STUB_DIR}/log" || true; }
if upload '{"isDraft":true}' && [ "$(uploads)" = 1 ] && grep -q -- '--clobber' "${STUB_DIR}/log"; then pass "upload: draft gets the assets"; else fail "upload: draft"; fi
if ! upload '{"isDraft":false}' && [ "$(uploads)" = 0 ]; then pass "upload: published release refused"; else fail "upload: published accepted"; fi
if upload 'ERR release not found' '{"isDraft":true}' && [ "$(uploads)" = 1 ]; then pass "upload: waits for the draft"; else fail "upload: wait for draft"; fi
if ! upload 'ERR configuration file not found' && [ "$(uploads)" = 0 ]; then pass "upload: other gh error refused"; else fail "upload: gh error"; fi
reset_stub; views '{"isDraft":true}' '{"isDraft":false}'; echo 1 > "${STUB_DIR}/upload-fail"
if ! ATTEMPTS=3 POLL_SECONDS=0 run "${hack}/upload-release-assets.sh" o/r v0.1.0 "${dist}" > /dev/null 2>&1 && [ "$(uploads)" = 1 ]; then pass "upload: no clobber retry once the release is published"; else fail "upload: retried after publication"; fi
reset_stub; views '{"isDraft":true}'; printf '1\n1\n1\n' > "${STUB_DIR}/upload-fail"
if ! ATTEMPTS=3 POLL_SECONDS=0 run "${hack}/upload-release-assets.sh" o/r v0.1.0 "${dist}" > /dev/null 2>&1 && [ "$(uploads)" = 3 ] && [ "$(cat "${STUB_DIR}/views")" = 3 ]; then pass "upload: draft state read before every attempt"; else fail "upload: attempts $(uploads), views $(cat "${STUB_DIR}/views")"; fi
rm -f "${dist}/checksums.txt"
if ! upload '{"isDraft":true}' && [ "$(uploads)" = 0 ]; then pass "upload: missing local file refused"; else fail "upload: missing local file"; fi
make_dist 0.1.0

# --- release-verify-notes.sh
notes() { "${hack}/release-verify-notes.sh" o/r v0.1.0 "${dA}" "${dB}"; }
start='<!-- sablecourt:verify:start -->' end='<!-- sablecourt:verify:end -->'
first="$(printf 'intro\n' | notes)"
again="$(printf '%s\n' "${first}" | notes)"
[ "${first}" = "${again}" ] && pass "notes: rerun is idempotent" || fail "notes: not idempotent"
[ "$(grep -c 'sablecourt:verify:start' <<< "${again}")" = 1 ] && pass "notes: one section" || fail "notes: section count"
grep -q '^intro$' <<< "${again}" && pass "notes: prose kept on rerun" || fail "notes: prose lost on rerun"
crlf="$(printf '%s\n' "${first}" | sed 's/$/\r/' | notes)"
[ "$(grep -c 'sablecourt:verify:start' <<< "${crlf}")" = 1 ] && pass "notes: CRLF body is replaced, not doubled" || fail "notes: CRLF body"
# The sequence from the review: an unmatched opening marker followed by prose, then a second run.
body="$(printf 'intro\n%s\nkeep this line\n' "${start}")"
if ! printf '%s\n' "${body}" | notes > "${work}/n1" 2>/dev/null && [ ! -s "${work}/n1" ]; then pass "notes: unmatched opening marker refused, nothing written"; else fail "notes: unmatched marker accepted"; fi
# What the earlier helper wrote for that body (the prose, then a full section) is refused too, not paired.
second="$(printf '%s\n\n%s\n' "${body}" "${first#intro$'\n\n'}")"
if ! printf '%s\n' "${second}" | notes > "${work}/n2" 2>/dev/null; then pass "notes: second run on a stray marker refused (prose kept)"; else fail "notes: second run deleted: $(grep -c 'keep this line' "${work}/n2") copies left"; fi
for case in "end marker only:intro\n${end}\nprose" "end before start:${end}\nprose\n${start}" "two sections:${start}\na\n${end}\n${start}\nb\n${end}" "marker inside prose:see ${start} here\nprose" "nested start:${start}\n${start}\n${end}"; do
  name="${case%%:*}" text="${case#*:}"
  # shellcheck disable=SC2059
  if ! printf "${text}\n" | notes > /dev/null 2>&1; then pass "notes: ${name} refused"; else fail "notes: ${name} accepted"; fi
done
expect_exit "notes: bad digest refused" 1 "${hack}/release-verify-notes.sh" o/r v0.1.0 bad "${dB}"

# --- resolve-image-digests.sh and verify-image.sh
shaA="$(printf 'c%.0s' $(seq 40))" shaB="$(printf 'd%.0s' $(seq 40))"
digest="sha256:$(printf 'e%.0s' $(seq 64))"
resolve() { # expected commit, then env assignments
  local commit="$1"
  shift
  reset_stub
  mkdir -p "${STUB_DIR}/tags"
  printf '%s' "${digest}" > "${STUB_DIR}/tags/ghcr.io_owner_sablecourt_0.1.0"
  printf '%s' "${digest}" > "${STUB_DIR}/tags/ghcr.io_owner_sablecourt-agent_0.1.0"
  env PATH="${stub}:${PATH}" POLL_SECONDS=0 WAIT_SECONDS=0 STUB_PROV_REF=refs/tags/v0.1.0 "$@" \
    "${hack}/resolve-image-digests.sh" v0.1.0 owner/repo Owner "${commit}" > "${work}/out" 2> "${work}/err"
}
if resolve "${shaA}" STUB_PROV_SHA="${shaA}" && grep -qx "daemon=${digest}" "${work}/out"; then pass "resolve: provenance from the expected commit"; else fail "resolve: expected commit ($(tail -n 1 "${work}/err"))"; fi
if ! resolve "${shaA}" STUB_PROV_SHA="${shaB}"; then pass "resolve: same tag, another commit refused"; else fail "resolve: provenance from another commit accepted"; fi
if ! resolve "${shaA}" STUB_PROV_SHA=; then pass "resolve: provenance without a commit refused"; else fail "resolve: provenance without commit accepted"; fi
if ! resolve "${shaA}" STUB_PROV_SHA="${shaA}" STUB_COSIGN_FAIL=verify; then pass "resolve: missing signature refused"; else fail "resolve: signature"; fi
if ! resolve "${shaA}" STUB_PROV_SHA="${shaA}" STUB_COSIGN_FAIL=verify-attestation; then pass "resolve: missing SBOM refused"; else fail "resolve: SBOM"; fi
expect_exit "resolve: commit is required" 2 env PATH="${stub}:${PATH}" WAIT_SECONDS=0 POLL_SECONDS=0 "${hack}/resolve-image-digests.sh" v0.1.0 owner/repo owner
expect_exit "resolve: short commit refused" 2 env PATH="${stub}:${PATH}" WAIT_SECONDS=0 POLL_SECONDS=0 "${hack}/resolve-image-digests.sh" v0.1.0 owner/repo owner abc123

# --- promote-images.sh
promote() { # ref type, ref name, then extra env assignments; runs in the tags repository
  local type="$1" name="$2"
  shift 2
  reset_stub
  (cd "${repo}" && env PATH="${stub}:${PATH}" "$@" "${hack}/promote-images.sh" Owner "${type}" "${name}" "${shaA}" "${dA}" "${dB}") > "${work}/out" 2> "${work}/err"
}
created() { sed -n 's/^create \([^ ]*\) .*/\1/p' "${STUB_DIR}/log" | sed 's|ghcr.io/owner/||' | tr '\n' ' '; }
tagrepo "${repo}" v0.1.0 v0.1.1 v0.2.0
if promote tag v0.2.0 && [ "$(created)" = "sablecourt:0.2.0 sablecourt-agent:0.2.0 sablecourt:0.2 sablecourt-agent:0.2 sablecourt:latest sablecourt-agent:latest " ]; then pass "promote: newest release, version tags on both before floating tags"; else fail "promote: newest release: $(created)"; fi
grep -q "^create ghcr.io/owner/sablecourt:0.2.0 ghcr.io/owner/sablecourt@${dA}$" "${STUB_DIR}/log" && grep -q "^create ghcr.io/owner/sablecourt-agent:latest ghcr.io/owner/sablecourt-agent@${dB}$" "${STUB_DIR}/log" \
  && pass "promote: each image from its own digest" || fail "promote: digests"
if promote tag v0.1.0 && [ "$(created)" = "sablecourt:0.1.0 sablecourt-agent:0.1.0 " ]; then pass "promote: older release takes no floating tag"; else fail "promote: older release: $(created)"; fi
if promote tag v0.3.0-rc.1 && [ "$(created)" = "sablecourt:0.3.0-rc.1 sablecourt-agent:0.3.0-rc.1 " ]; then pass "promote: candidate takes only its version"; else fail "promote: candidate: $(created)"; fi
if promote branch main && [ "$(created)" = "sablecourt:main sablecourt-agent:main sablecourt:sha-ccccccc sablecourt-agent:sha-ccccccc " ]; then pass "promote: main"; else fail "promote: main: $(created)"; fi
if ! promote tag v0.2.0 STUB_CREATE_FAIL=ghcr.io/owner/sablecourt-agent:0.2.0 && ! grep -q -e ':latest' -e ':0.2 ' "${STUB_DIR}/log"; then pass "promote: a failed version tag stops before any floating tag"; else fail "promote: floating tags after a failure"; fi
if ! promote tag v0.2.0 STUB_CREATE_WRONG="${dB}" && [ "$(writes)" = 1 ]; then pass "promote: read-back mismatch stops it"; else fail "promote: read-back"; fi
if ! promote branch feature; then pass "promote: other branch refused"; else fail "promote: other branch"; fi
expect_exit "promote: bad digest refused" 2 env PATH="${stub}:${PATH}" "${hack}/promote-images.sh" owner tag v0.2.0 "${shaA}" sha256:short "${dB}"

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
