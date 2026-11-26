#!/usr/bin/env bash
# Table tests of hack/release-image-tag.sh with a stub docker: a version tag that is absent or already on the same
# digest is fine; one on another digest, or a lookup that fails for any reason but "not found", stops the release.
# Usage: hack/test/release-image-tag_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/release-image-tag.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0
image=ghcr.io/shadewright-labs/sablecourt
mine="sha256:$(printf 'a%.0s' {1..64})"
theirs="sha256:$(printf 'b%.0s' {1..64})"

# The stub answers `docker buildx imagetools inspect <ref> --format ...` with $STUB_OUT on stdout, $STUB_ERR on
# stderr and the exit status $STUB_RC.
mkdir -p "${work}/bin"
cat > "${work}/bin/docker" <<'SH'
#!/usr/bin/env bash
[ "$1 $2 $3" = "buildx imagetools inspect" ] || { echo "unexpected docker call: $*" >&2; exit 1; }
printf '%s' "${STUB_OUT:-}"
[ -z "${STUB_ERR:-}" ] || printf '%s\n' "${STUB_ERR}" >&2
exit "${STUB_RC:-0}"
SH
chmod +x "${work}/bin/docker"
export PATH="${work}/bin:${PATH}"

# <name> <wanted exit status> <wanted stdout> [env assignments...]
case_() {
  local name="$1" want_rc="$2" want_out="$3" out rc=0
  shift 3
  out="$(env "$@" "${script}" "${image}" 1.0.0 "${mine}" 2> /dev/null)" || rc=$?
  if [ "${rc}" = "${want_rc}" ] && [ "${out}" = "${want_out}" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got exit ${rc} and '${out}', want exit ${want_rc} and '${want_out}'"
    fails=$((fails + 1))
  fi
}

case_ "an absent tag may be written" 0 absent STUB_RC=1 STUB_ERR="ERROR: ${image}:1.0.0: not found"
case_ "a tag on the same digest is a rerun of the same release" 0 same STUB_OUT="{\"digest\":\"${mine}\"}"
case_ "a tag on another digest stops the release" 1 "" STUB_OUT="{\"digest\":\"${theirs}\"}"
case_ "a denied lookup stops the release" 1 "" STUB_RC=1 STUB_ERR="ERROR: unexpected status from HEAD request: 403 Forbidden"
case_ "a registry error stops the release" 1 "" STUB_RC=1 STUB_ERR="ERROR: failed to do request: 502 Bad Gateway"
case_ "not found for another reference stops the release" 1 "" STUB_RC=1 STUB_ERR="ERROR: ${image}:latest: not found"
case_ "a failure with no message stops the release" 1 "" STUB_RC=1
case_ "not found next to another error stops the release" 1 "" STUB_RC=1 STUB_ERR="ERROR: ${image}:1.0.0: not found
ERROR: failed to authorize: 401 Unauthorized"
case_ "not found after a warning stops the release" 1 "" STUB_RC=1 STUB_ERR="WARNING: rate limited, retrying
ERROR: ${image}:1.0.0: not found"
case_ "not found with an exit status other than 1 stops the release" 1 "" STUB_RC=2 STUB_ERR="ERROR: ${image}:1.0.0: not found"
case_ "not found with output on stdout stops the release" 1 "" STUB_RC=1 STUB_OUT='{"digest":"x"}' STUB_ERR="ERROR: ${image}:1.0.0: not found"
case_ "an answer without a digest stops the release" 1 "" STUB_OUT='{"mediaType":"x"}'
case_ "an answer that is not JSON stops the release" 1 "" STUB_OUT="<html>"

rc=0
"${script}" "${image}" 1.0.0 latest > /dev/null 2>&1 || rc=$?
if [ "${rc}" = 2 ]; then echo "ok   a digest that is not sha256 is refused"; else echo "FAIL a digest that is not sha256 is refused"; fails=$((fails + 1)); fi
rc=0
"${script}" "${image}" 1.0.0 > /dev/null 2>&1 || rc=$?
if [ "${rc}" = 2 ]; then echo "ok   a missing argument is a usage error"; else echo "FAIL a missing argument is a usage error"; fails=$((fails + 1)); fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
