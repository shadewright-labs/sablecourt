#!/usr/bin/env python3
"""Checks the docs navigation against the files in docs/ (standard library only; run with python3 -I).

Usage: scripts/docs-nav-check.py [CONFIG [DOCS_DIR [PENDING_FILE]]]

Every page named in `nav` of zensical.toml must exist, and every Markdown page under docs/ must be in `nav`.
Zensical does not report either case itself. While the pages of the docs plan are written on other branches,
the differences that are known and expected are listed in scripts/docs-pending.txt, one per line:

  add <path>      a page in `nav` that is not written yet (path relative to docs/)
  remove <path>   a page on disk that the plan replaces and that is not in `nav`

The list may only shrink: a listed page that now exists (add), that is in `nav` or gone (remove), or a difference
that is not listed, fails the check. When the list is empty the check is fully strict.
"""
import sys
import tomllib
from pathlib import Path


def nav_pages(items):
    for item in items:
        for value in item.values():
            if isinstance(value, str):
                yield value
            else:
                yield from nav_pages(value)


def read_pending(path):
    add, remove = set(), set()
    if not path.exists():
        return add, remove
    for number, raw in enumerate(path.read_text().splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        kind, _, page = line.partition(" ")
        if kind == "add" and page:
            add.add(page.strip())
        elif kind == "remove" and page:
            remove.add(page.strip())
        else:
            sys.exit(f"{path}:{number}: expected 'add <path>' or 'remove <path>'")
    return add, remove


def main(argv):
    config = Path(argv[1] if len(argv) > 1 else "zensical.toml")
    docs = Path(argv[2] if len(argv) > 2 else "docs")
    pending = Path(argv[3] if len(argv) > 3 else "scripts/docs-pending.txt")

    nav = list(nav_pages(tomllib.loads(config.read_text())["project"]["nav"]))
    duplicates = sorted({p for p in nav if nav.count(p) > 1})
    nav = set(nav)
    disk = {str(p.relative_to(docs)) for p in docs.rglob("*.md")}
    add, remove = read_pending(pending)

    problems = [f"page listed twice in nav: {p}" for p in duplicates]
    for p in sorted(nav - disk - add):
        problems.append(f"in nav but missing on disk (write it, or list 'add {p}'): {p}")
    for p in sorted(disk - nav - remove):
        problems.append(f"on disk but not in nav (add it to nav, or list 'remove {p}'): {p}")
    for p in sorted(add & disk):
        problems.append(f"listed 'add' but now exists, delete the line: {p}")
    for p in sorted(add - nav):
        problems.append(f"listed 'add' but not in nav, delete the line: {p}")
    for p in sorted(remove & nav):
        problems.append(f"listed 'remove' but in nav, delete the line: {p}")
    for p in sorted(remove - disk):
        problems.append(f"listed 'remove' but gone from disk, delete the line: {p}")

    for p in problems:
        print(f"docs nav: {p}", file=sys.stderr)
    if problems:
        return 1
    print(f"docs nav: ok ({len(nav)} pages in nav, {len(add)} not written yet, {len(remove)} to be replaced)")
    return 0


sys.exit(main(sys.argv))
