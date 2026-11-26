#!/usr/bin/env python3
"""Checks the links in the tracked Markdown files.

Usage:
  python3 -I hack/docs-links.py check      internal links: fail on a link to a missing file or heading
  python3 -I hack/docs-links.py external   external links: report the ones that do not answer, never fail

"check" reads every tracked *.md file paragraph by paragraph (so a link may wrap over lines) and follows each
relative link outside fenced code blocks (``` or ~~~, three or more) and code spans: inline links (also the <angle bracket> form, with a
title in double quotes, single quotes or parentheses), full and collapsed reference links ([text][ref],
[text][]), reference definitions, and HTML href and src attributes (double-quoted, single-quoted or unquoted).
Headings are ATX (# Title) and Setext (a line underlined with === or ---). Shortcut reference links ([text]
alone) and links inside raw HTML blocks other than <a> and <img> are not followed. It fails when a target file does not exist, when a
link to a Markdown file names a #fragment that no heading of that file has (GitHub's anchor rules, inline code
in headings included), or when a reference link has no definition.

Links to pages or headings that are not written yet are listed in hack/docs-pages-pending.txt, one per line as
"<source file> -> <target>[#fragment]" (paths from the repository root). Each line allows one occurrence of
exactly that link from that source: two links to the same pending target in one file need two lines, also when
both use one reference definition, so a second, broken link cannot hide behind the first one's entry. A link from another file, or to another fragment, still
fails. An entry whose target now exists, or a line that no link uses any more, fails the check, so the list
cannot go stale.

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

# An opening fence is three or more backticks or tildes; the closing one has the same character, at least as
# many of them and nothing after them.
FENCE = re.compile(r"^\s*(`{3,}|~{3,})(.*)$")
# [text](<target with spaces>) or [text](target "title"), with the title in double or single quotes or in
# parentheses. Text, target and title may run over several lines of one paragraph.
INLINE = re.compile(
    r"!?\[(?:[^\[\]]|\[[^\]]*\])*\]\(\s*(?:<([^>\n]*)>|([^)\s]+))"
    r"(?:\s+(?:\"[^\"]*\"|'[^']*'|\([^)]*\)))?\s*\)"
)
# [text][ref] and [text][]
REFLINK = re.compile(r"\[((?:[^\[\]]|\[[^\]]*\])+)\]\[([^\]]*)\]")
REFDEF = re.compile(r"^\s{0,3}\[([^\]]+)\]:\s*(?:<([^>]*)>|(\S+))", re.MULTILINE)
# href and src in double quotes, single quotes or unquoted.
HTML_LINK = re.compile(r"<(?:a|img)\s[^>]*?(?:href|src)\s*=\s*(?:\"([^\"]*)\"|'([^']*)'|([^\s\"'=<>`]+))", re.IGNORECASE)
CODE_SPAN = re.compile(r"(`+)(?:(?!\1).)+?\1", re.DOTALL)
HEADING = re.compile(r"^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$")
SETEXT = re.compile(r"^\s{0,3}(=+|-+)\s*$")
EXPLICIT_ANCHOR = re.compile(r"<a\s+(?:name|id)\s*=\s*(?:\"([^\"]+)\"|'([^']+)')", re.IGNORECASE)


def tracked_markdown():
    out = subprocess.run(["git", "-C", ROOT, "ls-files", "-z", "*.md"], check=True, capture_output=True).stdout
    return sorted(p for p in out.decode().split("\0") if p)


def lines_outside_fences(path):
    """Yields (line number, raw text) for lines outside fenced code blocks."""
    in_fence = None
    with open(os.path.join(ROOT, path), encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            m = FENCE.match(line.rstrip("\n"))
            if m:
                fence = m.group(1)
                if in_fence is None:
                    in_fence = fence
                    continue
                if fence[0] == in_fence[0] and len(fence) >= len(in_fence) and not m.group(2).strip():
                    in_fence = None
                    continue
            if in_fence is None:
                yield n, line.rstrip("\n")


def paragraphs(path):
    """Yields (first line number, text) for each run of non-blank lines outside fences, joined with newlines, so
    a link that wraps over lines is seen whole. A fence or a blank line ends a paragraph."""
    start, buf, last = None, [], None
    for n, line in lines_outside_fences(path):
        if not line.strip() or (last is not None and n != last + 1):
            if buf:
                yield start, "\n".join(buf)
            start, buf = None, []
        if line.strip():
            if start is None:
                start = n
            buf.append(line)
        last = n
    if buf:
        yield start, "\n".join(buf)


def blank_code_spans(text):
    """Replaces each code span by spaces (newlines kept), so offsets and line numbers stay right."""
    return CODE_SPAN.sub(lambda m: "".join(c if c == "\n" else " " for c in m.group(0)), text)


def norm_label(label):
    return " ".join(label.lower().split())


def first(*groups):
    return next(g for g in groups if g is not None)


def scan(path):
    """Returns (links, reference uses, definitions): links as (line, target). A reference link counts as one link
    to its definition's target per use, so each use needs its own pending line; a definition no link uses counts
    once, at its own line."""
    links, uses, defs = [], [], {}
    for start, raw in paragraphs(path):
        text = blank_code_spans(raw)

        def line_of(offset):
            return start + text.count("\n", 0, offset)

        for m in REFDEF.finditer(text):
            defs.setdefault(norm_label(m.group(1)), (line_of(m.start()), first(m.group(2), m.group(3))))
        rest = REFDEF.sub(lambda m: " " * len(m.group(0)), text)
        for m in INLINE.finditer(rest):
            links.append((line_of(m.start()), first(m.group(1), m.group(2))))
        for m in HTML_LINK.finditer(rest):
            links.append((line_of(m.start()), first(m.group(1), m.group(2), m.group(3))))
        no_inline = INLINE.sub(lambda m: "".join(c if c == "\n" else " " for c in m.group(0)), rest)
        for m in REFLINK.finditer(no_inline):
            label = m.group(2) or m.group(1)
            uses.append((line_of(m.start()), norm_label(label)))
    used_labels = set()
    for n, label in uses:
        if label in defs:
            used_labels.add(label)
            links.append((n, defs[label][1]))
    for label, (n, target) in defs.items():
        if label not in used_labels:
            links.append((n, target))
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
        prev, prev_n = None, None  # the previous line, when it is ordinary paragraph text
        for n, raw in lines_outside_fences(path):
            for m in EXPLICIT_ANCHOR.finditer(raw):
                found.add(first(m.group(1), m.group(2)))
            m = HEADING.match(raw)
            text = None
            if m:
                text = m.group(1)
            elif prev is not None and prev_n == n - 1 and SETEXT.match(raw):
                # A Setext heading: a text line underlined with = or -. After a blank line, --- is a rule.
                text = prev.strip()
            if text is None:
                plain = raw.strip() and not raw.lstrip().startswith(("|", ">", "<", "-", "*", "+")) and not SETEXT.match(raw)
                prev, prev_n = (raw, n) if plain else (None, None)
                continue
            prev, prev_n = None, None
            base = slug(text)
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
            key = (source, target)
            entries[key] = (entries.get(key) or 0) + 1
    return entries


def target_exists(target):
    page, _, frag = target.partition("#")
    full = os.path.join(ROOT, page)
    if not os.path.exists(full):
        return False
    return not frag or (page.endswith(".md") and frag in anchors(page))


def check():
    pending = read_pending()
    used = {}
    errors = [msg for msg in pending.values() if isinstance(msg, str)]

    def exempt(key):
        """True when a pending entry is left for this occurrence of the link; each entry line covers one."""
        if isinstance(pending.get(key), int) and used.get(key, 0) < pending[key]:
            used[key] = used.get(key, 0) + 1
            return True
        return False

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
                if exempt(key):
                    continue
                errors.append(f"{path}:{n}: {target}: no such file {rel}")
                continue
            if fragment and rel.endswith(".md") and os.path.isfile(full) and fragment not in anchors(rel):
                if exempt(key):
                    continue
                errors.append(f"{path}:{n}: {target}: {rel} has no heading with the anchor #{fragment}")
    for key, count in sorted(pending.items()):
        if isinstance(count, str):
            continue
        source, target = key
        if target_exists(target):
            errors.append(f"hack/docs-pages-pending.txt: {source} -> {target} exists now; remove the entry")
        elif used.get(key, 0) < count:
            errors.append(
                f"hack/docs-pages-pending.txt: {source} -> {target} is listed {count} time(s) but "
                f"{used.get(key, 0)} link(s) use it; remove the unused line(s)"
            )
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
