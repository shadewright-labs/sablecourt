#!/usr/bin/env python3
"""Checks the links in the tracked Markdown files.

Usage:
  python3 -I hack/docs-links.py check      internal links: fail on a link to a missing file or heading
  python3 -I hack/docs-links.py external   external links: report the ones that do not answer, never fail

"check" reads every tracked *.md file and follows each relative link outside fenced code blocks and code
spans: inline links (also the <angle bracket> form), full and collapsed reference links ([text][ref], [text][]),
reference definitions, and HTML href and src attributes. It fails when a target file does not exist, when a
link to a Markdown file names a #fragment that no heading of that file has (GitHub's anchor rules, inline code
in headings included), or when a reference link has no definition.

Links to pages or headings that are not written yet are listed in hack/docs-pages-pending.txt, one per line as
"<source file> -> <target>[#fragment]" (paths from the repository root). An entry allows exactly that link from
that source; a link from another file, or to another fragment, still fails. An entry whose target now exists, or
that no link uses any more, fails the check, so the list cannot go stale.

"external" sends one request per distinct http(s) URL and prints a warning for each one that fails. It is run
weekly by docs-links.yml, never on pull requests. DOCS_ROOT overrides the repository root (tests).
Tests: hack/test/docs-links_test.sh.
"""

import os
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

ROOT = os.environ.get("DOCS_ROOT") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PENDING = os.path.join(ROOT, "hack", "docs-pages-pending.txt")

FENCE = re.compile(r"^\s*(```|~~~)")
# [text](<target with spaces>) or [text](target "title")
INLINE = re.compile(r"!?\[(?:[^\[\]]|\[[^\]]*\])*\]\(\s*(?:<([^>\n]*)>|([^)\s]+))(?:\s+\"[^\"]*\")?\s*\)")
# [text][ref] and [text][]
REFLINK = re.compile(r"\[((?:[^\[\]]|\[[^\]]*\])+)\]\[([^\]]*)\]")
REFDEF = re.compile(r"^\s{0,3}\[([^\]]+)\]:\s*(?:<([^>]*)>|(\S+))")
HTML_LINK = re.compile(r"<(?:a|img)\s[^>]*?(?:href|src)=\"([^\"]+)\"", re.IGNORECASE)
CODE_SPAN = re.compile(r"(`+)(?:(?!\1).)+?\1")
HEADING = re.compile(r"^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$")
EXPLICIT_ANCHOR = re.compile(r"<a\s+(?:name|id)=\"([^\"]+)\"", re.IGNORECASE)


def tracked_markdown():
    out = subprocess.run(["git", "-C", ROOT, "ls-files", "-z", "*.md"], check=True, capture_output=True).stdout
    return sorted(p for p in out.decode().split("\0") if p)


def lines_outside_fences(path):
    """Yields (line number, raw text) for lines outside fenced code blocks."""
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
                yield n, line.rstrip("\n")


def norm_label(label):
    return " ".join(label.lower().split())


def scan(path):
    """Returns (links, reference uses, definitions): links as (line, target)."""
    links, uses, defs = [], [], {}
    for n, raw in lines_outside_fences(path):
        line = CODE_SPAN.sub("", raw)
        m = REFDEF.match(line)
        if m:
            defs[norm_label(m.group(1))] = True
            links.append((n, m.group(2) if m.group(2) is not None else m.group(3)))
            continue
        for m in INLINE.finditer(line):
            links.append((n, m.group(1) if m.group(1) is not None else m.group(2)))
        for m in HTML_LINK.finditer(line):
            links.append((n, m.group(1)))
        for m in REFLINK.finditer(INLINE.sub("", line)):
            label = m.group(2) or m.group(1)
            uses.append((n, norm_label(label)))
    return links, uses, defs


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
        for _, raw in lines_outside_fences(path):
            for m in EXPLICIT_ANCHOR.finditer(raw):
                found.add(m.group(1))
            m = HEADING.match(raw)
            if not m:
                continue
            base = slug(m.group(1))
            count = seen.get(base, 0)
            seen[base] = count + 1
            found.add(base if count == 0 else f"{base}-{count}")
        _anchors[path] = found
    return _anchors[path]


def read_pending():
    entries = {}
    if not os.path.exists(PENDING):
        return entries
    with open(PENDING, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            s = line.strip()
            if not s or s.startswith("#"):
                continue
            source, sep, target = (p.strip() for p in s.partition("->"))
            if not sep or not source or not target:
                entries[("", f"line {n}")] = f"malformed entry on line {n}: {s!r}"
                continue
            entries[(source, target)] = None
    return entries


def target_exists(target):
    page, _, frag = target.partition("#")
    full = os.path.join(ROOT, page)
    if not os.path.exists(full):
        return False
    return not frag or (page.endswith(".md") and frag in anchors(page))


def check():
    pending = read_pending()
    used = set()
    errors = [msg for msg in pending.values() if msg]
    for path in tracked_markdown():
        base_dir = os.path.dirname(path)
        links, uses, defs = scan(path)
        for n, label in uses:
            if label not in defs:
                errors.append(f"{path}:{n}: reference link [{label}] has no definition")
        for n, target in links:
            parsed = urllib.parse.urlsplit(target)
            if parsed.scheme or target.startswith("//"):
                continue
            file_part = urllib.parse.unquote(parsed.path)
            fragment = urllib.parse.unquote(parsed.fragment)
            if file_part.startswith("/"):
                rel = os.path.normpath(file_part.lstrip("/"))
            elif file_part:
                rel = os.path.normpath(os.path.join(base_dir, file_part))
            else:
                rel = path
            if rel.startswith(".."):
                errors.append(f"{path}:{n}: {target}: points outside the repository")
                continue
            key = (path, rel + (f"#{fragment}" if fragment else ""))
            full = os.path.join(ROOT, rel)
            if not os.path.exists(full):
                if key in pending:
                    used.add(key)
                    continue
                errors.append(f"{path}:{n}: {target}: no such file {rel}")
                continue
            if fragment and rel.endswith(".md") and os.path.isfile(full) and fragment not in anchors(rel):
                if key in pending:
                    used.add(key)
                    continue
                errors.append(f"{path}:{n}: {target}: {rel} has no heading with the anchor #{fragment}")
    for key, msg in sorted(pending.items()):
        if msg:
            continue
        source, target = key
        if target_exists(target):
            errors.append(f"hack/docs-pages-pending.txt: {source} -> {target} exists now; remove the entry")
        elif key not in used:
            errors.append(f"hack/docs-pages-pending.txt: no link from {source} to {target}; remove the entry")
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
        for n, target in scan(path)[0]:
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
