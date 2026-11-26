#!/usr/bin/env bash
# Scans the whole Git history of a repository with gitleaks and the repository's .gitleaks.toml, the command of
# secret-scan.yml (and of hack/test/gitleaks_test.sh, so the controls test exactly what CI runs). Findings are
# printed redacted; exit 1 on any finding. The log options are gitleaks' defaults (every ref, full history) plus
# --diff-merges=first-parent: without it `git log -p` shows no diff for a merge commit, so a change made only
# while resolving a merge (a token added in the merge commit itself) would never be scanned.
#
# Fails closed when the history cannot be read. gitleaks runs `git log` itself and ignores its exit status: a
# failed or empty read scans nothing and passes. So this script first reads the same range itself, checks git's
# exit status and that the log holds exactly `git rev-list --count` commits, and counts the commits gitleaks must
# report as scanned: those with a text hunk in a file that is not deleted (gitleaks skips deleted and binary
# files, so an empty commit, a mode change, a pure rename, a binary change or a deletion is not counted). The
# scan fails when gitleaks reports fewer, or no count at all, or when the git process gitleaks started did not
# exit 0 (a read cut short inside a commit).
#
# Known limit: a git process that stops part way through a commit and still exits 0 is not detected, because
# gitleaks counts commits, not bytes. No ordinary failure ends that way. Usage:
#   hack/secret-scan.sh <gitleaks binary> [repository, default .]
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 <gitleaks binary> [repository]" >&2
  exit 2
fi
gitleaks="$1" repo="${2:-.}"
range=(--all --full-history)
log_opts=("${range[@]}" --diff-merges=first-parent)
work="$(mktemp -d "${TMPDIR:-/tmp}/secret-scan.XXXXXX")"
work="$(cd "${work}" && pwd)" # absolute, so the shim directory in PATH survives a change of directory
trap 'rm -rf "${work}"' EXIT

fail() {
  echo "secret scan: $*" >&2
  exit 1
}

total="$(git -C "${repo}" rev-list --count "${range[@]}")" || fail "git rev-list failed"
[[ "${total}" =~ ^[0-9]+$ ]] || fail "git rev-list returned '${total}'"
git -C "${repo}" log --format='commit %H' -p -U0 --no-ext-diff --no-textconv "${log_opts[@]}" > "${work}/log" \
  || fail "git log failed"
read -r listed want < <(awk '
  /^commit [0-9a-f]+$/ { c = $2; listed++; del = 0; next }
  /^diff --git / { del = 0; next }
  /^deleted file mode / { del = 1; next }
  /^@@ / { if (c != "" && !del && !(c in seen)) { seen[c] = 1; want++ } }
  END { print listed + 0, want + 0 }
' "${work}/log") || fail "cannot count the commits of the log"
[ "${listed}" -eq "${total}" ] || fail "git log listed ${listed} commits, git rev-list counts ${total}"

# gitleaks starts its own git and does not look at how it ended, so a read that stops part way through a commit
# still counts that commit. A shim named git, first in PATH for the gitleaks call only, runs the real git and
# writes the exit status of each `git log` to a file (gitleaks also runs other git commands that may fail
# harmlessly); any status but 0, or no `git log` at all, fails the scan.
real_git="$(command -v git)" || fail "git not found"
mkdir "${work}/bin"
# The shim takes the real git and the status file from its environment, so no path is written into its text. It
# records a call only when the git subcommand is log (the first word that is not an option or the value of -C or -c).
cat > "${work}/bin/git" <<'SHIM'
#!/bin/sh
sub="" skip=0
for a in "$@"; do
  if [ "$skip" = 1 ]; then skip=0; continue; fi
  case "$a" in
    -C|-c) skip=1 ;;
    -*) ;;
    *) sub="$a"; break ;;
  esac
done
"$SECRET_SCAN_REAL_GIT" "$@"
rc=$?
[ "$sub" = log ] && echo "$rc" >> "$SECRET_SCAN_GIT_STATUS"
exit "$rc"
SHIM
chmod +x "${work}/bin/git"
: > "${work}/git-status"

rc=0
SECRET_SCAN_REAL_GIT="${real_git}" SECRET_SCAN_GIT_STATUS="${work}/git-status" PATH="${work}/bin:${PATH}" \
  "${gitleaks}" git "${repo}" --redact --no-banner --exit-code 1 \
  --log-opts="${log_opts[*]}" > "${work}/out" 2>&1 || rc=$?
cat "${work}/out"
if [ "${rc}" -ne 0 ]; then
  exit "${rc}"
fi
[ -s "${work}/git-status" ] || fail "gitleaks did not read the history through git"
if grep -qv '^0$' "${work}/git-status"; then
  fail "the git process that fed gitleaks exited with $(grep -v '^0$' "${work}/git-status" | head -n 1)"
fi
scanned="$(sed 's/\x1b\[[0-9;]*m//g' "${work}/out" | sed -n 's/.*[^0-9]\([0-9][0-9]*\) commits scanned.*/\1/p' | tail -n 1)"
[ -n "${scanned}" ] || fail "gitleaks reported no count of scanned commits"
[ "${scanned}" -ge "${want}" ] || fail "gitleaks scanned ${scanned} commits, the history has ${want} with text changes (${total} in all)"
echo "secret scan: ${scanned} commits scanned, ${want} with text changes expected, ${total} in all"
