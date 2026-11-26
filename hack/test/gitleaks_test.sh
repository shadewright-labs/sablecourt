#!/usr/bin/env bash
# Positive and negative controls for the secret scan: runs the same gitleaks command as secret-scan.yml, with
# this repository's .gitleaks.toml, on throwaway repositories. A synthetic secret in an ordinary file, a new
# value in a file that has an allowlist, and a contiguous private key in such a file must each fail the scan
# (exit 1); the allowlisted fixture value must pass. The secrets are assembled at run time, so this file holds
# none. Usage: hack/test/gitleaks_test.sh [path to gitleaks]
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
gitleaks="${1:-gitleaks}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0 n=0

alnum() { head -c "$1" < <(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom); }

# scan <name> <want exit code> <path> <content>
scan() {
  local repo="${work}/r$((n += 1))" got=0
  mkdir -p "${repo}/$(dirname "$3")"
  cp "${root}/.gitleaks.toml" "${repo}/.gitleaks.toml"
  printf '%s\n' "$4" > "${repo}/$3"
  git -C "${repo}" init -q
  git -C "${repo}" -c user.name=test -c user.email=test@example.invalid -c commit.gpgsign=false add -A
  git -C "${repo}" -c user.name=test -c user.email=test@example.invalid -c commit.gpgsign=false commit -qm fixture
  (cd "${repo}" && "${gitleaks}" git . --redact --no-banner --exit-code 1 > /dev/null 2>&1) || got=$?
  if [ "${got}" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1: exit ${got}, want $2"; fails=$((fails + 1)); fi
}

pat="ghp_$(alnum 36)"
key_body="$(alnum 64)$(alnum 64)$(alnum 64)"
begin="-----BEGIN"" PRIVATE KEY-----"
end="-----END"" PRIVATE KEY-----"

scan "synthetic token in an ordinary file" 1 notes/config.txt "token = ${pat}"
scan "new token in a file with an allowlist" 1 internal/gate/mcp/upstream_test.go "var t = \"${pat}\""
scan "contiguous private key in a file with a key allowlist" 1 internal/attach/hub_test.go "${begin}
${key_body}
${end}"
scan "allowlisted fixture value" 0 internal/gate/mcp/upstream_test.go "var t = \"ghp_""1234567890abcdefghijklmnop1234567890\""

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
