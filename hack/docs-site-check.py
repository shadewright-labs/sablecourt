#!/usr/bin/env python3
"""Checks the built docs site (Zensical output in site/): internal links, heading anchors,
images, stylesheets, scripts, redirects, and absence of third-party assets.

Usage (run with -I, standard library only):
  python3 -I hack/docs-site-check.py check SITE_DIR [PENDING_FILE [REPO_DIR]]

PENDING_FILE uses the same "<source> -> <target>[#fragment]" format as hack/docs-pages-pending.txt
(paths relative to the repository root under docs/). Each entry allows one occurrence of that link
on its source page in the built site. Relative <a href> links that step one level above site/ into
tracked source files in REPO_DIR (such as ADR links to cmd/ or internal/) are checked against
REPO_DIR when REPO_DIR is provided; images, stylesheets and scripts must exist inside SITE_DIR.
Any other broken internal link, broken anchor, missing image, missing stylesheet/script, broken
meta-refresh redirect, or third-party script/stylesheet/image URL exits 1.

Tests: hack/test/docs-site-check_test.sh.
"""

import os
import re
import sys
import urllib.parse
from html.parser import HTMLParser

SITE_URL = "https://shadewrightlabs.com/sablecourt"
SITE_BASE_PATH = "/sablecourt"


class Page(HTMLParser):
    """Collects anchors, links, images, assets and meta refresh targets from one HTML file."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.ids = set()
        self.links = []
        self.images = []
        self.assets = []
        self.refresh = None

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if a.get("id"):
            self.ids.add(a["id"])
        if tag == "a":
            if a.get("name"):
                self.ids.add(a["name"])
            if a.get("href") is not None:
                self.links.append(a["href"])
        elif tag == "img":
            if a.get("src") is not None:
                self.images.append(a["src"])
        elif tag == "script":
            if a.get("src") is not None:
                self.assets.append(("script", a["src"]))
        elif tag == "link":
            rel = {r.lower() for r in (a.get("rel") or "").split()}
            if "stylesheet" in rel and a.get("href") is not None:
                self.assets.append(("stylesheet", a["href"]))
        elif tag == "meta" and (a.get("http-equiv") or "").lower() == "refresh":
            m = re.search(r"url\s*=\s*['\"]?([^'\";]+)", a.get("content") or "", re.I)
            if m:
                self.refresh = m.group(1).strip()


def pages(site):
    """Maps the URL path of every built HTML page to its file path."""
    found = {}
    for root, _, files in os.walk(site):
        for name in files:
            if not name.endswith(".html"):
                continue
            rel = os.path.relpath(os.path.join(root, name), site).replace(os.sep, "/")
            url = "/" + (rel[: -len("index.html")] if name == "index.html" else rel)
            found[url] = os.path.join(root, name)
    return found


def parse(path):
    p = Page()
    with open(path, encoding="utf-8") as f:
        p.feed(f.read())
    return p


def is_external_http(ref):
    u = urllib.parse.urlsplit(ref)
    if u.scheme in ("http", "https"):
        return ref != SITE_URL and not ref.startswith(SITE_URL + "/")
    return ref.startswith("//")


def resolve(ref, base):
    """Returns (site-root-relative path, fragment, escapes_by_one) for an internal reference, or None if external.

    escapes_by_one is the repo-relative path (e.g. 'cmd/sablecourtd/main.go') when a relative reference
    steps exactly one directory above the site root (matching a Markdown link from docs/ to repo root).
    """
    if not ref:
        return None
    if ref == SITE_URL or ref.startswith(SITE_URL + "/"):
        ref = ref[len(SITE_URL):] or "/"
    u = urllib.parse.urlsplit(ref)
    if u.scheme or u.netloc:
        return None
    raw_path = u.path
    if raw_path == SITE_BASE_PATH or raw_path.startswith(SITE_BASE_PATH + "/"):
        raw_path = raw_path[len(SITE_BASE_PATH):] or "/"
    escapes_by_one = None
    if raw_path and not raw_path.startswith("/"):
        base_dir = base if base.endswith("/") else os.path.dirname(base)
        rel_norm = os.path.normpath(os.path.join(base_dir.lstrip("/"), urllib.parse.unquote(raw_path))).replace(os.sep, "/")
        if rel_norm.startswith("../") and not rel_norm.startswith("../../"):
            escapes_by_one = rel_norm[len("../"):]
    if raw_path:
        path = urllib.parse.unquote(urllib.parse.urljoin(base, raw_path))
    else:
        path = base
    return path, urllib.parse.unquote(u.fragment), escapes_by_one


def target_path(site, path):
    """Returns the normalized filesystem path inside site, or None if path escapes site."""
    root = os.path.normpath(os.path.abspath(site))
    target = os.path.normpath(os.path.join(root, path.lstrip("/")))
    if target != root and not target.startswith(root + os.sep):
        return None
    return target


def file_exists(site, path):
    target = target_path(site, path)
    return target is not None and os.path.isfile(target)


def page_or_file_exists(site, path):
    target = target_path(site, path)
    if target is None:
        return False
    return os.path.isfile(target) or os.path.isfile(os.path.join(target, "index.html"))


def md_to_url(md_path):
    """Maps a repo-relative docs/ Markdown path to its built URL path."""
    if not md_path.startswith("docs/"):
        return None
    rel = md_path[len("docs/"):]
    if rel in ("index.md", "README.md"):
        return "/"
    for suffix in ("/index.md", "/README.md"):
        if rel.endswith(suffix):
            return "/" + rel[: -len(suffix)] + "/"
    if rel.endswith(".md"):
        return "/" + rel[:-3] + "/"
    return "/" + rel


def read_pending(pending_file):
    """Reads pending entries keyed by (source_url, target_url, fragment) -> allowed count."""
    entries = {}
    if not pending_file:
        return entries
    with open(pending_file, encoding="utf-8") as f:
        for line in f:
            s = line.strip()
            if not s or s.startswith("#"):
                continue
            source, sep, target = (p.strip() for p in s.partition("->"))
            if not sep or not source or not target:
                continue
            src_url = md_to_url(source)
            t_page, _, frag = target.partition("#")
            dst_url = md_to_url(t_page)
            if src_url is None or dst_url is None:
                continue
            key = (src_url, dst_url, frag)
            entries[key] = entries.get(key, 0) + 1
    return entries


def repo_target_exists(repo_dir, rel):
    """True when rel is an existing non-docs file or directory inside repo_dir."""
    if not repo_dir or not rel or rel == "docs" or rel.startswith("docs/"):
        return False
    root = os.path.normpath(os.path.abspath(repo_dir))
    target = os.path.normpath(os.path.join(root, rel))
    if target == root or not target.startswith(root + os.sep):
        return False
    return os.path.exists(target)


def check(site, pending_file=None, repo_dir=None):
    problems = []
    built = pages(site)
    if not built:
        return [f"{site}: no HTML pages found"]
    parsed = {url: parse(f) for url, f in built.items()}
    pending = read_pending(pending_file)
    used = {}

    def exempt(url, path, frag):
        # Match either the directory URL (/a/b/) or an unrewritten .md path (/a/b.md).
        candidates = [(url, path, frag)]
        if path.endswith(".md"):
            candidates.append((url, path[:-3] + "/", frag))
        elif not path.endswith("/"):
            candidates.append((url, path + "/", frag))
        for key in candidates:
            if pending.get(key, 0) > used.get(key, 0):
                used[key] = used.get(key, 0) + 1
                return True
        return False

    for url, p in sorted(parsed.items()):
        for src in p.images:
            if is_external_http(src):
                problems.append(f"{url}: third-party image {src}")
                continue
            r = resolve(src, url)
            if r is None:
                continue
            path, _, escaped = r
            if escaped is not None or not file_exists(site, path):
                problems.append(f"{url}: missing image {src} (no file {path})")

        for kind, ref in p.assets:
            if is_external_http(ref):
                problems.append(f"{url}: third-party {kind} {ref}")
                continue
            r = resolve(ref, url)
            if r is None:
                continue
            path, _, escaped = r
            if escaped is not None or not file_exists(site, path):
                problems.append(f"{url}: missing {kind} {ref} (no file {path})")

        for href in p.links:
            r = resolve(href, url)
            if r is None:
                continue
            path, frag, escaped = r
            if escaped is not None:
                if repo_target_exists(repo_dir, escaped):
                    continue
                problems.append(f"{url}: broken link {href} (escapes site to {escaped})")
                continue
            if not page_or_file_exists(site, path):
                if exempt(url, path, frag):
                    continue
                problems.append(f"{url}: broken link {href} (no page or file {path})")
                continue
            if frag:
                folder = path[: -len("index.html")] if path.endswith("/index.html") else path
                target_page = parsed.get(folder) or parsed.get(path) or parsed.get(path + "/")
                if target_page is not None and frag not in target_page.ids:
                    if exempt(url, path, frag):
                        continue
                    problems.append(f"{url}: broken anchor {href} (no #{frag} on {path})")

        if p.refresh:
            r = resolve(p.refresh, url)
            if r is not None and (r[2] is not None or not page_or_file_exists(site, r[0])):
                problems.append(f"{url}: broken redirect {p.refresh} (no page {r[0]})")

    return problems


def main(argv):
    if len(argv) not in (3, 4, 5) or argv[1] != "check":
        print(__doc__, file=sys.stderr)
        return 2
    pending = argv[3] if len(argv) >= 4 and argv[3] else None
    repo_dir = argv[4] if len(argv) == 5 else None
    problems = check(argv[2], pending, repo_dir)
    for problem in problems:
        print(f"ERROR {problem}", file=sys.stderr)
    if problems:
        print(f"{len(problems)} problem(s) in the built docs site", file=sys.stderr)
        return 1
    print("built docs site: links, anchors, images and assets ok")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
