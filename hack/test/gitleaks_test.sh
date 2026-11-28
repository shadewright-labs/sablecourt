#!/usr/bin/env bash
# Positive and negative controls for the secret scan: runs hack/secret-scan.sh (the command of secret-scan.yml)
# with this repository's .gitleaks.toml on throwaway repositories.
#   - A synthetic token in an ordinary file fails the scan (exit 1).
#   - Every file named by an allowlist passes as it is in this tree, in a new commit, with no allowlist applying
#     (its fixtures are built from split literals, so stock gitleaks finds nothing there), and fails once a
#     private key block (contiguous, or assembled from Go string literals) or a token is added to it, before or
#     after its fixtures: a fixture header can no longer swallow a key that follows it.
#   - Files that only the history still holds fail with a new key block or token at their path.
#   - The allowlists are scoped: each has exactly one value, and all but the one for a file this repository
#     cannot change yet name the commits whose history holds that value, so the same value in a new commit is
#     reported.
#   - A token that only a merge commit introduces (added while resolving the merge) fails the scan.
#   - In a pull request range, a token that main removed and the branch keeps through a merge of main fails the
#     scan, and a pull request event with an empty base or head is refused.
# The secrets are assembled at run time, so this file holds none. Usage: hack/test/gitleaks_test.sh [gitleaks]
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
gitleaks="${1:-gitleaks}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0 n=0
test_email="scan-test@example.invalid"

alnum() { head -c "$1" < <(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom); }
git_q() { git -c user.name=test -c user.email="${test_email}" -c commit.gpgsign=false "$@"; }

new_repo() {
  repo="${work}/r$((n += 1))"
  mkdir -p "${repo}"
  git_q -C "${repo}" init -q -b main
  cp "${root}/.gitleaks.toml" "${repo}/.gitleaks.toml"
}

# scan_repo <name> <want exit code>: scans ${repo} with the CI command.
scan_repo() {
  local got=0
  "${root}/hack/secret-scan.sh" "${gitleaks}" "${repo}" > "${work}/out" 2>&1 || got=$?
  if [ "${got}" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1: exit ${got}, want $2"; fails=$((fails + 1)); fi
}

# scan <name> <want exit code> <path> <content> [file to start from]: the content goes before the copied file.
# scan_after: the same, with the content after the copied file. gitleaks' private-key rule matches lazily from
# a header to the next "KEY-----" at least 64 characters on, so a fixture header earlier in a file can swallow
# the header of a key that follows it; an allowlist for that fixture span then hid the key. The fixtures no
# longer hold a contiguous header, and no allowlist covers these files, so the key after them is reported.
scan() {
  new_repo
  mkdir -p "${repo}/$(dirname "$3")"
  if [ -n "$4" ]; then printf '%s\n' "$4" > "${repo}/$3"; else : > "${repo}/$3"; fi
  if [ -n "${5:-}" ]; then cat "$5" >> "${repo}/$3"; fi
  git_q -C "${repo}" add -A
  git_q -C "${repo}" commit -qm fixture
  scan_repo "$1" "$2"
}
scan_after() {
  new_repo
  mkdir -p "${repo}/$(dirname "$3")"
  cp "$5" "${repo}/$3"
  printf '\n%s\n' "$4" >> "${repo}/$3"
  git_q -C "${repo}" add -A
  git_q -C "${repo}" commit -qm fixture
  scan_repo "$1" "$2"
}

begin="-----BEGIN"" PRIVATE KEY-----"
end="-----END"" PRIVATE KEY-----"
pem_block() { printf '%s\n%s\n%s\n%s\n%s' "var fixtureKey = \`${begin}" "$(alnum 64)" "$(alnum 64)" "$(alnum 64)" "${end}\`"; }
# A key built from Go string literals: the form the earlier quote-based allowlists let through.
pem_literals() { printf '%s' "var fixtureKey = \"${begin}\\n\" + \"$(alnum 64)$(alnum 64)\" + \"\\n${end}\""; }
token_line() { printf '%s' "var fixtureToken = \"ghp_$(alnum 36)\""; }

scan "synthetic token in an ordinary file" 1 notes/config.txt "token = ghp_$(alnum 36)"
scan "contiguous private key in an ordinary file" 1 notes/key.txt "$(pem_block)"

# Every path that has an allowlist in .gitleaks.toml.
paths="$(sed -n "s/^paths = \['''^\(.*\)\$'''\]$/\1/p" "${root}/.gitleaks.toml" | sed 's/\\\././g' | LC_ALL=C sort -u)"
[ -n "${paths}" ] || { echo "FAIL no allowlisted paths read from .gitleaks.toml"; fails=$((fails + 1)); }
while IFS= read -r path; do
  [ -n "${path}" ] || continue
  if [ -f "${root}/${path}" ]; then
    scan "${path}: fixtures as in this tree pass" 0 "${path}" "" "${root}/${path}"
    scan "${path}: another key block is found" 1 "${path}" "$(pem_block)" "${root}/${path}"
    scan "${path}: a key built from literals is found" 1 "${path}" "$(pem_literals)" "${root}/${path}"
    scan "${path}: another token is found" 1 "${path}" "$(token_line)" "${root}/${path}"
    scan_after "${path}: a key block after the fixtures is found" 1 "${path}" "$(pem_block)" "${root}/${path}"
    scan_after "${path}: a key from literals after the fixtures is found" 1 "${path}" "$(pem_literals)" "${root}/${path}"
    scan_after "${path}: a token after the fixtures is found" 1 "${path}" "$(token_line)" "${root}/${path}"
  else
    scan "${path} (history only): a key block is found" 1 "${path}" "$(pem_block)"
    scan "${path} (history only): a key built from literals is found" 1 "${path}" "$(pem_literals)"
    scan "${path} (history only): a token is found" 1 "${path}" "$(token_line)"
  fi
done <<< "${paths}"

# An allowed historic value is reported in any commit but the ones its entry names.
scan "a value allowed in old commits is found in a new one" 1 cmd/sablectl/council_test.go \
  "var fixtureToken = \"ghp_""SYNTHETIC1234567890abcdefghij\""

# Every allowlist has one path and one exact value, AND-ed, and names its commits; only the entry for a file
# this repository has not split yet goes without commits.
if python3 -I - "${root}/.gitleaks.toml" <<'PY'
import sys, tomllib
with open(sys.argv[1], "rb") as f:
    cfg = tomllib.load(f)
bad, unscoped = [], []
for a in cfg.get("allowlists", []):
    paths, regexes = a.get("paths", []), a.get("regexes", [])
    if set(a) - {"description", "condition", "commits", "paths", "regexes"} or a.get("condition") != "AND" \
            or len(paths) != 1 or len(regexes) != 1 or not regexes[0].startswith("^") or not regexes[0].endswith("$"):
        bad.append(paths)
    elif not a.get("commits"):
        unscoped.append(paths[0])
if cfg.get("allowlist") or bad or unscoped != [r"^internal/gate/mcp/upstream_test\.go$"]:
    print(f"bad entries: {bad}, entries without commits: {unscoped}")
    sys.exit(1)
PY
then
  echo "ok   every allowlist is one exact value in one file, scoped by commit"
else
  echo "FAIL allowlists not scoped"
  fails=$((fails + 1))
fi

# merge_only <name> <file> <content>: the secret exists only in the merge commit; neither parent has it.
merge_only() {
  new_repo
  echo base > "${repo}/README.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm base
  git_q -C "${repo}" checkout -q -b topic
  echo topic > "${repo}/topic.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm topic
  git_q -C "${repo}" checkout -q main
  echo main > "${repo}/main.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm main
  git_q -C "${repo}" merge -q --no-ff --no-commit topic > /dev/null
  mkdir -p "$(dirname "${repo}/$2")"
  printf '%s\n' "$3" >> "${repo}/$2"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm merge
  scan_repo "$1" 1
}
merge_only "token added only in a merge commit" notes/merged.txt "token = ghp_$(alnum 36)"
merge_only "key added only in a merge commit, allowlisted path" internal/attach/hub_test.go "$(pem_block)"
merge_only "token added only in a merge commit to an existing file" README.txt "token = ghp_$(alnum 36)"

# merge_restores <name>: main removed a token (an earlier commit, outside the range) and the branch, made before
# that, merges main keeping its copy: the first-parent diff of the merge is empty, the diff against main adds it.
# The scan runs in pull request mode (base = main after the removal, head = the merge).
merge_restores() {
  new_repo
  echo base > "${repo}/README.txt"
  printf 'token = ghp_%s\n' "$(alnum 36)" > "${repo}/old.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm "token and base"
  git_q -C "${repo}" checkout -q -b topic
  git_q -C "${repo}" checkout -q main
  git_q -C "${repo}" rm -q old.txt && git_q -C "${repo}" commit -qm "remove token"
  base_sha="$(git -C "${repo}" rev-parse HEAD)"
  git_q -C "${repo}" checkout -q topic
  echo topic > "${repo}/topic.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm topic
  git_q -C "${repo}" merge -q --no-ff -s ours main -m "merge main, keep token"
  head_sha="$(git -C "${repo}" rev-parse HEAD)"
  local got=0
  SECRET_SCAN_BASE="${base_sha}" SECRET_SCAN_HEAD="${head_sha}" \
    "${root}/hack/secret-scan.sh" "${gitleaks}" "${repo}" > "${work}/out" 2>&1 || got=$?
  if [ "${got}" = 1 ]; then echo "ok   $1"; else echo "FAIL $1: exit ${got}, want 1"; fails=$((fails + 1)); fi
}
merge_restores "a token restored by a merge of main is found in a pull request range"

# The scan fails closed when the history cannot be read. A stub git first in PATH breaks `git log`, either for
# every caller or only for gitleaks' own call (`log -p -U0` with no --format), while every other git command
# runs the real git. Without the check, gitleaks reads no commits, finds nothing and exits 0.
real_git="$(command -v git)"
stub="${work}/stub"
mkdir -p "${stub}"
cat > "${stub}/git" <<EOF
#!/bin/sh
case "\${GIT_STUB_MODE}" in
  fail-log|fail-gitleaks-log|empty-gitleaks-log|partial-gitleaks-log) ;;
  *) exec "${real_git}" "\$@" ;;
esac
is_log=0 has_format=0
for a in "\$@"; do
  case "\${a}" in
    log) is_log=1 ;;
    --format=*) has_format=1 ;;
  esac
done
if [ "\${is_log}" = 1 ]; then
  if [ "\${GIT_STUB_MODE}" = fail-log ]; then echo "stub: log failed" >&2; exit 128; fi
  if [ "\${has_format}" = 0 ]; then
    if [ "\${GIT_STUB_MODE}" = fail-gitleaks-log ]; then echo "stub: log failed" >&2; exit 128; fi
    # The read stops inside the last commit: every commit header arrives, the end of the last hunk does not.
    if [ "\${GIT_STUB_MODE}" = partial-gitleaks-log ]; then
      "${real_git}" "\$@" > "${work}/full.log"
      n=\$(wc -c < "${work}/full.log")
      head -c "\$((n - 8))" "${work}/full.log"
      exit 74
    fi
    exit 0
  fi
fi
exec "${real_git}" "\$@"
EOF
chmod +x "${stub}/git"

# history_repo: a clean repository whose history has every kind of commit gitleaks does not count (empty, mode
# change, pure rename, binary, deletion, a merge with no first-parent change) next to ordinary ones.
history_repo() {
  new_repo
  printf 'a\nb\n' > "${repo}/a.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm text
  git_q -C "${repo}" commit -q --allow-empty -m empty
  chmod +x "${repo}/a.txt" && git_q -C "${repo}" commit -qam mode
  git_q -C "${repo}" mv a.txt b.txt && git_q -C "${repo}" commit -qm rename
  printf '\000\001\002' > "${repo}/bin.dat" && git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm binary
  git_q -C "${repo}" rm -q bin.dat && git_q -C "${repo}" commit -qm delete
  git_q -C "${repo}" checkout -q -b side
  echo side > "${repo}/side.txt" && git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm side
  git_q -C "${repo}" checkout -q main
  git_q -C "${repo}" merge -q --no-ff -s ours side -m merge
}

history_repo
scan_repo "clean history with uncounted commits passes" 0
for mode in fail-log fail-gitleaks-log empty-gitleaks-log partial-gitleaks-log; do
  history_repo
  GIT_STUB_MODE="${mode}" PATH="${stub}:${PATH}" scan_repo "history read broken (${mode}) fails" 1
done

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
