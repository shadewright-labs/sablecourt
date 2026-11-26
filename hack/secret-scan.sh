#!/usr/bin/env bash
# Scans the whole Git history of a repository with gitleaks and the repository's .gitleaks.toml, the command of
# secret-scan.yml (and of hack/test/gitleaks_test.sh, so the controls test exactly what CI runs). Findings are
# printed redacted; exit 1 on any finding. The log options are gitleaks' defaults (every ref, full history) plus
# --diff-merges=first-parent: without it `git log -p` shows no diff for a merge commit, so a change made only
# while resolving a merge (a token added in the merge commit itself) would never be scanned. Usage:
#   hack/secret-scan.sh <gitleaks binary> [repository, default .]
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 <gitleaks binary> [repository]" >&2
  exit 2
fi
exec "$1" git "${2:-.}" --redact --no-banner --exit-code 1 \
  --log-opts='--all --full-history --diff-merges=first-parent'
