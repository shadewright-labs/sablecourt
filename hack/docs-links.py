#!/usr/bin/env python3
"""Checks the links in the tracked Markdown files.

Usage:
  python3 -I hack/docs-links.py check      internal links: fail on a link to a missing file or heading
  python3 -I hack/docs-links.py external   external links: report the ones that do not answer, never fail

"check" reads every tracked *.md file, follows each relative link (inline and reference style, outside code
blocks and code spans) and fails when the target file does not exist or, for a link to a Markdown file with a
#fragment, when that file has no heading with that anchor (GitHub's anchor rules). Pages that are linked but
not written yet are listed in hack/docs-pages-pending.txt (a path, or a path#anchor for a heading not written
yet); a link to one of them is allowed, and an entry that now exists or that nothing links to fails the check,
so the list cannot go stale.

"external" sends one request per distinct http(s) URL and prints a warning for each one that fails. It is run
weekly by docs-links.yml, never on pull requests, so a pull request does not depend on third-party sites.
"""

import os
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PENDING = os.path.join(ROOT, "hack", "docs-pages-pending.txt")

FENCE = re.compile(r"^\s*(```|~~~)")
INLINE = re.compile(r"!?\[(?:[^\[\]]|\[[^\]]*\])*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)")
REFDEF = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+.*)?$")
HTML_LINK = re.compile(r"<(?:a|img)\s[^>]*?(?:href|src)=\"([^\"]+)\"", re.IGNORECASE)
CODE_SPAN = re.compile(r"(`+)(?:(?!\1).)+?\1")
HEADING = re.compile(r"^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$")
EXPLICIT_ANCHOR = re.compile(r"<a\s+(?:name|id)=\"([^\"]+)\"", re.IGNORECASE)


def tracked_markdown():
    out = subprocess.run(["git", "-C", ROOT, "ls-files", "-z", "*.md"], check=True, capture_output=True).stdout
    return sorted(p for p in out.decode().split("\0") if p)


def lines_outside_code(path):
    """Yields (line number, text) for lines outside fenced code blocks, with code spans removed."""
    in_fence = None
    with open(os.path.join(ROOT, path), encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            m = FENCE.match(line)
            if m:
                if in_fence is None:
                    in_fence = m.group(1)
                elif m.group(1) == in_fence:
                    in_fence = None
                continue
            if in_fence is None:
                yield n, CODE_SPAN.sub("", line.rstrip("\n"))


def links(path):
    for n, line in lines_outside_code(path):
        for rx in (INLINE, HTML_LINK):
            for m in rx.finditer(line):
                yield n, m.group(1)
        m = REFDEF.match(line)
        if m:
            yield n, m.group(1)


def slug(text):
    """GitHub's heading anchor: lower case, markup and punctuation dropped, spaces to hyphens."""
    text = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", text)
    text = re.sub(r"<[^>]+>", "", text)
    text = text.replace("`", "").lower()
    text = re.sub(r"[^\w\- ]", "", text)
    return text.replace(" ", "-")


_anchors = {}


def anchors(path):
    if path not in _anchors:
        found, seen = set(), {}
        for _, line in lines_outside_code(path):
            for m in EXPLICIT_ANCHOR.finditer(line):
                found.add(m.group(1))
            m = HEADING.match(line)
            if not m:
                continue
            base = slug(m.group(1))
            count = seen.get(base, 0)
            seen[base] = count + 1
            found.add(base if count == 0 else f"{base}-{count}")
        _anchors[path] = found
    return _anchors[path]


def read_pending():
    if not os.path.exists(PENDING):
        return set()
    with open(PENDING, encoding="utf-8") as f:
        return {s for s in (line.strip() for line in f) if s and not s.startswith("#")}


def check():
    pending = read_pending()
    used_pending = set()
    errors = []
    for path in tracked_markdown():
        base_dir = os.path.dirname(path)
        for n, target in links(path):
            parsed = urllib.parse.urlsplit(target)
            if parsed.scheme or target.startswith("//"):
                continue
            file_part = urllib.parse.unquote(parsed.path)
            fragment = urllib.parse.unquote(parsed.fragment)
            if file_part:
                rel = os.path.normpath(os.path.join(base_dir, file_part)) if not file_part.startswith("/") \
                    else os.path.normpath(file_part.lstrip("/"))
            else:
                rel = path
            if rel.startswith(".."):
                errors.append(f"{path}:{n}: {target}: points outside the repository")
                continue
            full = os.path.join(ROOT, rel)
            if not os.path.exists(full):
                if rel in pending:
                    used_pending.add(rel)
                    continue
                errors.append(f"{path}:{n}: {target}: no such file {rel}")
                continue
            if fragment and rel.endswith(".md") and os.path.isfile(full):
                if fragment not in anchors(rel):
                    if f"{rel}#{fragment}" in pending:
                        used_pending.add(f"{rel}#{fragment}")
                        continue
                    errors.append(f"{path}:{n}: {target}: {rel} has no heading with the anchor #{fragment}")
    for rel in sorted(pending):
        page, _, frag = rel.partition("#")
        if frag and os.path.isfile(os.path.join(ROOT, page)) and frag in anchors(page):
            errors.append(f"hack/docs-pages-pending.txt: {rel} exists now; remove it from the list")
        elif not frag and os.path.exists(os.path.join(ROOT, rel)):
            errors.append(f"hack/docs-pages-pending.txt: {rel} exists now; remove it from the list")
        elif rel not in used_pending:
            errors.append(f"hack/docs-pages-pending.txt: nothing links to {rel}; remove it from the list")
    for e in errors:
        print(e, file=sys.stderr)
    if errors:
        print(f"{len(errors)} broken internal link(s)", file=sys.stderr)
        return 1
    print("internal links: ok")
    return 0


def external():
    urls = {}
    for path in tracked_markdown():
        for n, target in links(path):
            if urllib.parse.urlsplit(target).scheme in ("http", "https"):
                urls.setdefault(target, f"{path}:{n}")
    bad = 0
    for url, where in sorted(urls.items()):
        req = urllib.request.Request(url, method="GET", headers={"User-Agent": "docs-link-check"})
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:  # noqa: S310 (http and https only, above)
                if resp.status >= 400:
                    raise urllib.error.HTTPError(url, resp.status, "", resp.headers, None)
        except Exception as exc:  # report every failure, never stop
            bad += 1
            print(f"::warning::{where}: {url}: {exc}")
    print(f"checked {len(urls)} external link(s), {bad} did not answer")
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2 or sys.argv[1] not in ("check", "external"):
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    sys.exit(check() if sys.argv[1] == "check" else external())
