#!/usr/bin/env python3
"""Checks the built docs site (Zensical output in site/): internal links, heading anchors,
images, stylesheets, scripts, redirects, and absence of third-party assets.

Usage (run with -I, standard library only):
  python3 -I hack/docs-site-check.py check SITE_DIR [PENDING_FILE]

PENDING_FILE uses the same "<source> -> <target>[#fragment]" format as hack/docs-pages-pending.txt
(paths relative to the repository root under docs/). Each entry allows one occurrence of that link
on its source page in the built site. Images, stylesheets, scripts and internal links must resolve
inside SITE_DIR; links to repository source files outside docs/ must use the repository URL.
Any broken internal link, broken anchor, missing image, missing stylesheet/script, broken
meta-refresh redirect, root-relative path outside the site base path, or third-party
script/stylesheet/image URL exits 1.

Tests: hack/test/docs-site-check_test.sh.
"""

import os
import posixpath
import re
import sys
import urllib.parse
from html.parser import HTMLParser

try:
    import tomllib
except ImportError:
    tomllib = None

DEFAULT_ZENSICAL_TOML = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "zensical.toml"
)


def read_site_url(config_path=DEFAULT_ZENSICAL_TOML):
    """Reads project.site_url from zensical.toml."""
    with open(config_path, "rb") as f:
        if tomllib is not None:
            url = tomllib.load(f).get("project", {}).get("site_url", "")
        else:
            m = re.search(r'^\s*site_url\s*=\s*"([^"]+)"', f.read().decode("utf-8"), re.M)
            url = m.group(1) if m else ""
    if not url:
        raise ValueError(f"{config_path}: missing project.site_url")
    return url.rstrip("/")


def parse_srcset(value):
    """Extracts candidate URLs from an HTML srcset attribute, as the HTML parsing rules split it.

    A URL is a run of characters with no white space, so a comma inside it belongs to it. Commas at its
    end close the candidate; otherwise the descriptors run to the next comma outside parentheses, which do not nest.
    """
    urls, i, n = [], 0, len(value)
    while True:
        while i < n and (value[i].isspace() or value[i] == ","):
            i += 1
        if i >= n:
            return urls
        start = i
        while i < n and not value[i].isspace():
            i += 1
        url = value[start:i]
        if url.endswith(","):
            urls.append(url.rstrip(","))
            continue
        urls.append(url)
        in_parens = False
        while i < n:
            c = value[i]
            i += 1
            if c == "(":
                in_parens = True
            elif c == ")":
                in_parens = False
            elif c == "," and not in_parens:
                break


class Page(HTMLParser):
    """Collects anchors, links, images, assets and meta refresh targets from one HTML file."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.ids = set()
        self.links = []
        self.images = []
        self.assets = []
        self.refresh = None
        self._picture_depth = 0

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
            if a.get("srcset"):
                self.images.extend(parse_srcset(a["srcset"]))
        elif tag == "picture":
            self._picture_depth += 1
        elif tag == "source" and self._picture_depth > 0:
            if a.get("srcset"):
                self.images.extend(parse_srcset(a["srcset"]))
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

    def handle_endtag(self, tag):
        if tag == "picture" and self._picture_depth > 0:
            self._picture_depth -= 1


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


def is_site_url(ref, site_parts, base_path):
    """True when an http(s) ref targets the site itself (same origin and under base_path)."""
    u = urllib.parse.urlsplit(ref)
    if u.scheme != site_parts.scheme or u.netloc != site_parts.netloc:
        return False
    norm = posixpath.normpath(urllib.parse.unquote(u.path or "/"))
    if norm.startswith("//"):
        norm = "/" + norm.lstrip("/")
    return not base_path or norm == base_path or norm.startswith(base_path + "/")


def is_external_http(ref, site_parts, base_path):
    u = urllib.parse.urlsplit(ref)
    if u.scheme in ("http", "https"):
        return not is_site_url(ref, site_parts, base_path)
    return ref.startswith("//")


def resolve(ref, base, site_parts, base_path):
    """Returns (site-root-relative path, fragment, error_reason) for an internal reference, or None if external."""
    if not ref:
        return None
    u = urllib.parse.urlsplit(ref)
    if u.scheme or u.netloc:
        if u.scheme in ("http", "https") and is_site_url(ref, site_parts, base_path):
            raw_path = u.path or "/"
        else:
            return None
    else:
        raw_path = u.path
    frag = urllib.parse.unquote(u.fragment)
    if not raw_path:
        return base, frag, None
    if raw_path.startswith("/"):
        unquoted = urllib.parse.unquote(raw_path)
        norm_root = posixpath.normpath(unquoted)
        if norm_root.startswith("//"):
            norm_root = "/" + norm_root.lstrip("/")
        if base_path:
            if norm_root != base_path and not norm_root.startswith(base_path + "/"):
                return norm_root, frag, f"outside site base path {base_path}/"
            stripped = norm_root[len(base_path):] or "/"
        else:
            stripped = norm_root
        if unquoted.endswith("/") and stripped != "/":
            stripped += "/"
        return stripped, frag, None
    base_dir = base if base.endswith("/") else posixpath.dirname(base)
    rel_norm = posixpath.normpath(posixpath.join(base_dir.lstrip("/"), urllib.parse.unquote(raw_path)))
    if rel_norm == ".." or rel_norm.startswith("../"):
        target = rel_norm[len("../"):] if rel_norm.startswith("../") and not rel_norm.startswith("../../") else rel_norm
        return None, frag, f"escapes site to {target}; use the repository URL"
    path = urllib.parse.unquote(urllib.parse.urljoin(base, raw_path))
    return path, frag, None


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


def check(site, pending_file=None, site_url=None):
    if site_url is None:
        site_url = read_site_url()
    site_url = site_url.rstrip("/")
    site_parts = urllib.parse.urlsplit(site_url)
    base_path = site_parts.path.rstrip("/")

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

    def well_formed(url, ref):
        """Reports a reference urlsplit cannot parse as a problem of its page; the check goes on."""
        try:
            urllib.parse.urlsplit(ref)
        except ValueError:
            problems.append(f"{url}: malformed URL {ref}")
            return False
        return True

    for url, p in sorted(parsed.items()):
        p.images = [r for r in p.images if well_formed(url, r)]
        p.assets = [(k, r) for k, r in p.assets if well_formed(url, r)]
        p.links = [r for r in p.links if well_formed(url, r)]
        if p.refresh and not well_formed(url, p.refresh):
            p.refresh = None
        for src in p.images:
            if is_external_http(src, site_parts, base_path):
                problems.append(f"{url}: third-party image {src}")
                continue
            r = resolve(src, url, site_parts, base_path)
            if r is None:
                continue
            path, _, err = r
            if err is not None:
                problems.append(f"{url}: missing image {src} ({err})")
            elif not file_exists(site, path):
                problems.append(f"{url}: missing image {src} (no file {path})")

        for kind, ref in p.assets:
            if is_external_http(ref, site_parts, base_path):
                problems.append(f"{url}: third-party {kind} {ref}")
                continue
            r = resolve(ref, url, site_parts, base_path)
            if r is None:
                continue
            path, _, err = r
            if err is not None:
                problems.append(f"{url}: missing {kind} {ref} ({err})")
            elif not file_exists(site, path):
                problems.append(f"{url}: missing {kind} {ref} (no file {path})")

        for href in p.links:
            r = resolve(href, url, site_parts, base_path)
            if r is None:
                continue
            path, frag, err = r
            if err is not None:
                problems.append(f"{url}: broken link {href} ({err})")
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
            r = resolve(p.refresh, url, site_parts, base_path)
            if r is not None:
                path, _, err = r
                if err is not None:
                    problems.append(f"{url}: broken redirect {p.refresh} ({err})")
                elif not page_or_file_exists(site, path):
                    problems.append(f"{url}: broken redirect {p.refresh} (no page {path})")

    return problems


def main(argv):
    if len(argv) not in (3, 4) or argv[1] != "check":
        print(__doc__, file=sys.stderr)
        return 2
    pending = argv[3] if len(argv) == 4 and argv[3] else None
    problems = check(argv[2], pending)
    for problem in problems:
        print(f"ERROR {problem}", file=sys.stderr)
    if problems:
        print(f"{len(problems)} problem(s) in the built docs site", file=sys.stderr)
        return 1
    print("built docs site: links, anchors, images and assets ok")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
