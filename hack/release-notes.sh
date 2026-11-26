#!/usr/bin/env bash
# Extracts the body of one plain X.Y.Z version section from CHANGELOG.md for release.yml.
# Recognizes release-please headings with or without a compare link (## 0.1.0 (...),
# ## [0.2.0](...) (...), ### [0.1.1](...) (...)) and preserves inner ### subsections
# such as "### Features" and "### Bug fixes" until the next version heading.
# Usage: hack/release-notes.sh <version> <changelog file>
#   exit 0 and prints the trimmed section body when a non-empty section exists;
#   exit 1 when the file is missing or has no non-empty section for <version>;
#   exit 2 on a bad argument.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <version> <changelog file>" >&2
  exit 2
fi
version="$1" changelog="$2"
if ! [[ "${version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "version '${version}' is not plain X.Y.Z" >&2
  exit 2
fi
if [ ! -f "${changelog}" ]; then
  echo "changelog '${changelog}' not found" >&2
  exit 1
fi

awk -v ver="${version}" '
function heading_rest(line,    s) {
  if (line !~ /^##+ /) return ""
  s = line
  sub(/^##+ /, "", s)
  return s
}
function is_any_release_heading(line,    s) {
  if (line ~ /^## /) return 1
  s = heading_rest(line)
  if (s == "") return 0
  if (substr(s, 1, 1) == "[") s = substr(s, 2)
  return (s ~ /^[0-9]+\.[0-9]+\.[0-9]+($|[ (\-\]])/)
}
function is_target_heading(line, target,    s, n, ch) {
  s = heading_rest(line)
  if (s == "") return 0
  n = length(target)
  if (substr(s, 1, 1) == "[") {
    return (substr(s, 2, n) == target && substr(s, 2 + n, 2) == "](")
  }
  if (substr(s, 1, n) == target) {
    ch = substr(s, 1 + n, 1)
    return (ch == "" || ch == " " || ch == "(")
  }
  return 0
}
{
  if (!in_sec) {
    if (is_target_heading($0, ver)) {
      in_sec = 1
    }
    next
  }
  if (is_any_release_heading($0)) {
    exit
  }
  lines[++count] = $0
}
END {
  first = 0
  last = 0
  for (i = 1; i <= count; i++) {
    if (lines[i] ~ /[^[:space:]]/) {
      if (first == 0) first = i
      last = i
    }
  }
  if (first == 0) exit 1
  for (i = first; i <= last; i++) {
    print lines[i]
  }
}
' "${changelog}"
