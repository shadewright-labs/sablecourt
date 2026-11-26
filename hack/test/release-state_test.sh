#!/usr/bin/env bash
# Table tests of hack/release-state.sh with a stub gh that serves a release list: a failed or malformed lookup must
# stop the release (exit 1), never read as "no release".
# Usage: hack/test/release-state_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/release-state.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0

# The stub prints $STUB_PAGES (each page a JSON document) for `gh api --paginate .../releases...` and fails with
# an HTTP error when $STUB_FAIL is set.
mkdir -p "${work}/bin"
cat > "${work}/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1 $2" = "api --paginate" ] || { echo "unexpected gh call: $*" >&2; exit 1; }
case "$3" in repos/*/releases\?per_page=100) ;; *) echo "unexpected path: $3" >&2; exit 1 ;; esac
[ -z "${STUB_FAIL:-}" ] || { echo "HTTP 502: Bad Gateway" >&2; exit 1; }
printf '%s' "${STUB_PAGES}"
SH
chmod +x "${work}/bin/gh"
export PATH="${work}/bin:${PATH}" GH_REPO=example/repo

# <name> <pages> <wanted exit status> <wanted stdout> [env assignments...]
case_() {
  local name="$1" pages="$2" want_rc="$3" want_out="$4" out rc=0
  shift 4
  out="$(env STUB_PAGES="${pages}" "$@" "${script}" v1.0.0 2> /dev/null)" || rc=$?
  if [ "${rc}" = "${want_rc}" ] && [ "${out}" = "${want_out}" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got exit ${rc} and '${out}', want exit ${want_rc} and '${want_out}'"
    fails=$((fails + 1))
  fi
}

pub='{"id":1,"tag_name":"v1.0.0","draft":false}'
draft='{"id":2,"tag_name":"v1.0.0","draft":true}'
other='{"id":3,"tag_name":"v0.9.0","draft":false}'
case_ "no release at all" "[]" 0 ""
case_ "only releases of other tags" "[${other}]" 0 ""
case_ "a published release stops the run" "[${other},${pub}]" 3 ""
case_ "a draft is listed for deletion" "[${draft},${other}]" 0 "2"
case_ "two drafts are both listed" "[${draft},{\"id\":5,\"tag_name\":\"v1.0.0\",\"draft\":true}]" 0 "2
5"
case_ "a published release on a later page stops the run" "[${draft}][${pub}]" 3 ""
case_ "a draft and a published release stop the run" "[${draft},${pub}]" 3 ""
case_ "a failed lookup stops the run" "[]" 1 "" STUB_FAIL=1
case_ "an answer that is not a list stops the run" '{"message":"Not Found"}' 1 ""
case_ "a release without a draft field stops the run" '[{"id":1,"tag_name":"v1.0.0"}]' 1 ""
case_ "a draft field that is a string stops the run" '[{"id":1,"tag_name":"v1.0.0","draft":"false"}]' 1 ""
case_ "an answer that is not JSON stops the run" "<html>" 1 ""
case_ "an empty answer is no release" "" 0 ""

rc=0
"${script}" > /dev/null 2>&1 || rc=$?
if [ "${rc}" = 2 ]; then echo "ok   a missing argument is a usage error"; else echo "FAIL a missing argument is a usage error"; fails=$((fails + 1)); fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
