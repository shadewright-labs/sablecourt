#!/usr/bin/env python3
"""Builds the docs site with Zensical and fails on any warning (standard library only; run with python3 -I).

Usage: scripts/docs-build.py [ZENSICAL [KNOWN_FILE]]

This is `zensical build --clean --strict` with one addition: warnings listed in scripts/docs-known-warnings.txt are
tolerated. A warning is a broken link, a link to a heading that does not exist, or a page that does not exist.
Each line of the known list is `<page> | <message>` (page relative to docs/, message as Zensical prints it) and
covers one occurrence. A listed warning that no longer happens fails the build, so the list only shrinks.
"""
import re
import subprocess
import sys
from collections import Counter
from pathlib import Path

ANSI = re.compile(r"\x1b\[[0-9;]*m")
WARNING = re.compile(r"^(Warning|Error): (.*)$")
LOCATION = re.compile(r"╭─\[ (.+?):\d+:\d+ \]")


def read_known(path):
    known = Counter()
    if path.exists():
        for raw in path.read_text().splitlines():
            line = raw.strip()
            if line and not line.startswith("#"):
                page, _, message = (part.strip() for part in line.partition("|"))
                known[(page, message)] += 1
    return known


def main(argv):
    zensical = argv[1] if len(argv) > 1 else ".venv/bin/zensical"
    known_file = Path(argv[2] if len(argv) > 2 else "scripts/docs-known-warnings.txt")

    run = subprocess.run([zensical, "build", "--clean"], capture_output=True, text=True)
    output = ANSI.sub("", run.stdout + run.stderr)
    sys.stdout.write(output)
    if run.returncode != 0:
        print("docs build: zensical failed", file=sys.stderr)
        return 1

    found = Counter()
    pending = None
    for line in output.splitlines():
        match = WARNING.match(line)
        if match:
            if pending is not None:
                found[(pending[0], pending[1])] += 1
            pending = ["?", match.group(2).strip()]
            continue
        location = LOCATION.search(line)
        if location and pending is not None:
            pending[0] = location.group(1)
            found[(pending[0], pending[1])] += 1
            pending = None
    if pending is not None:
        found[(pending[0], pending[1])] += 1

    known = read_known(known_file)
    problems = []
    for key, count in sorted(found.items()):
        extra = count - known.get(key, 0)
        if extra > 0:
            problems.append(f"warning in {key[0]}: {key[1]} (x{extra})")
    for key, count in sorted(known.items()):
        if found.get(key, 0) < count:
            problems.append(f"known warning no longer happens, delete its line: {key[0]} | {key[1]}")
    for problem in problems:
        print(f"docs build: {problem}", file=sys.stderr)
    if problems:
        return 1
    tolerated = sum(found.values())
    print(f"docs build: ok ({tolerated} known warning{'s' if tolerated != 1 else ''} tolerated)")
    return 0


sys.exit(main(sys.argv))
