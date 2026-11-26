#!/usr/bin/env bash
# Tests of hack/release-assets.sh with a stub gh that serves a release from a fixture directory: the release
# passes only when it lists and serves exactly the verified files with the same bytes.
# Usage: hack/test/release-assets_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/release-assets.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0

check() {
  if [ "$2" = ok ]; then echo "ok   $1"; else echo "FAIL $1"; fails=$((fails + 1)); fi
}

# The stub lists the names in $STUB_LISTED (default: the files of $STUB_RELEASE) for `release view`, copies the
# files of $STUB_RELEASE for `release download`, and fails both when $STUB_FAIL names the subcommand.
mkdir -p "${work}/bin"
cat > "${work}/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  "release view")
    [ "${STUB_FAIL:-}" != view ] || { echo "HTTP 502" >&2; exit 1; }
    if [ -n "${STUB_LISTED+x}" ]; then printf '%s' "${STUB_LISTED}"; else ls -A "${STUB_RELEASE}"; fi ;;
  "release download")
    [ "${STUB_FAIL:-}" != download ] || { echo "HTTP 502" >&2; exit 1; }
    while [ "$#" -gt 0 ] && [ "$1" != --dir ]; do shift; done
    if [ -z "$(ls -A "${STUB_RELEASE}")" ]; then echo "no assets to download" >&2; exit 1; fi
    cp "${STUB_RELEASE}"/* "$2"/ ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
SH
chmod +x "${work}/bin/gh"
export PATH="${work}/bin:${PATH}"

fixture() { # makes a new verified directory and a release copy; prints the case directory
  local d
  d="$(mktemp -d "${work}/case.XXXXXX")"
  mkdir "${d}/dist" "${d}/release"
  printf 'archive one\n' > "${d}/dist/sablecourt_1.0.0_linux_amd64.tar.gz"
  printf 'sums\n' > "${d}/dist/checksums.txt"
  printf 'bundle\n' > "${d}/dist/checksums.txt.sigstore.json"
  cp "${d}/dist/"* "${d}/release/"
  echo "${d}"
}
run() { # <case dir> [env assignments...]: exit status of the script
  local d="$1" got=0
  shift
  env STUB_RELEASE="${d}/release" "$@" "${script}" v1.0.0 "${d}/dist" > /dev/null 2>&1 || got=$?
  echo "${got}"
}

d="$(fixture)"
check "the same files and bytes pass" "$([ "$(run "${d}")" = 0 ] && echo ok)"

d="$(fixture)"
printf 'extra\n' > "${d}/release/notes.zip"
check "an extra asset fails" "$([ "$(run "${d}")" = 1 ] && echo ok)"

d="$(fixture)"
rm "${d}/release/checksums.txt.sigstore.json"
check "a missing asset fails" "$([ "$(run "${d}")" = 1 ] && echo ok)"

d="$(fixture)"
printf 'archive two\n' > "${d}/release/sablecourt_1.0.0_linux_amd64.tar.gz"
check "an asset with other bytes fails" "$([ "$(run "${d}")" = 1 ] && echo ok)"

d="$(fixture)"
check "a listed asset that is not served fails" \
  "$([ "$(run "${d}" STUB_LISTED="$(ls -A "${d}/release"; echo late.tar.gz)")" = 1 ] && echo ok)"

d="$(fixture)"
check "a served asset that is not listed fails" \
  "$([ "$(run "${d}" STUB_LISTED="$(printf '%s\n' checksums.txt checksums.txt.sigstore.json)")" = 1 ] && echo ok)"

d="$(fixture)"
rm "${d}/release/checksums.txt"
check "a listed asset that is not downloaded fails" \
  "$([ "$(run "${d}" STUB_LISTED="$(ls -A "${d}/dist")")" = 1 ] && echo ok)"

d="$(fixture)"
rm "${d}/release/"*
check "a release with no asset fails" "$([ "$(run "${d}")" = 1 ] && echo ok)"

d="$(fixture)"
check "a failed listing fails" "$([ "$(run "${d}" STUB_FAIL=view)" = 1 ] && echo ok)"

d="$(fixture)"
check "a failed download fails" "$([ "$(run "${d}" STUB_FAIL=download)" = 1 ] && echo ok)"

d="$(fixture)"
rm "${d}/dist/"*
check "an empty verified directory fails" "$([ "$(run "${d}")" = 1 ] && echo ok)"

got=0
"${script}" v1.0.0 > /dev/null 2>&1 || got=$?
check "a missing argument is a usage error" "$([ "${got}" = 2 ] && echo ok)"

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
