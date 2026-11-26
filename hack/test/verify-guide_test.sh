#!/usr/bin/env bash
# Tests that docs/how-to/verify-a-release.md and the release section of docs/ci.md say only what release.yml and
# the tests enforce:
#   - the code blocks of the guide pass bash -n and shellcheck, start with set -euo pipefail, and run each command
#     whose output is used on its own line, as an assignment;
#   - each line of those blocks is a fixed setup line or, with the guide's variables mapped to the workflow's, part
#     of a line of the verify job; each cosign and gh command carries the identity, issuer, tag, commit and runner
#     flags; each flag, file name, URL and image name on the page appears in the verify job or in release.yml;
#   - the SBOM comparison of the guide and of the verify job, run with jq on fixtures, passes for an equal file and
#     fails for a file with a second JSON value or another value;
#   - each prose line of both pages is in CLAIMS below, with a check of the workflow, the scripts or the guide's
#     code that makes it true, so an edited or added line fails until its check is written; no prose line uses
#     only, always, never, every, any, nothing, cannot or exactly;
#   - a mutation table edits copies of the pages and requires that a check fails on each.
# Usage: hack/test/verify-guide_test.sh [repository root]
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
python3 -I - "${root}" <<'PY'
import base64, json, os, re, subprocess, sys, tempfile

root = sys.argv[1]
fails = 0


def check(name, ok):
    global fails
    print(("ok   " if ok else "FAIL ") + name)
    if not ok:
        fails += 1


def read(path):
    with open(os.path.join(root, path), encoding="utf-8") as f:
        return f.read()


def jobs(text):
    """Maps each job name of a workflow to its text."""
    out, cur, in_jobs = {}, None, False
    for line in text.splitlines():
        if re.match(r"^jobs:\s*$", line):
            in_jobs = True
            continue
        if in_jobs and re.match(r"^\S", line):
            in_jobs = False
        if not in_jobs:
            continue
        m = re.match(r"^  ([A-Za-z0-9_-]+):\s*$", line)
        if m:
            cur = m.group(1)
            out[cur] = ""
        elif cur:
            out[cur] += line + "\n"
    return out


WF = read(".github/workflows/release.yml")
J = jobs(WF)
GUIDE = read("docs/how-to/verify-a-release.md")
CI = read("docs/ci.md")
TRIVYIGNORE = read(".trivyignore.yaml")
WORKFLOWS_TEST = read("hack/test/workflows_test.sh")


def release_section(ci):
    at = ci.find("\n## Releases\n")
    if at < 0:
        return ""
    rest = ci[at + 1:]
    nxt = re.search(r"^## ", rest[3:], re.M)
    return rest if not nxt else rest[:nxt.start() + 3]


def blocks(page):
    return re.findall(r"^```bash\n(.*?)^```$", page, re.M | re.S)


def prose_lines(page):
    """The non-empty lines outside fenced blocks, without table rows (workflows_test.sh checks the tables)."""
    out, fenced = [], False
    for line in page.splitlines():
        if line.startswith("```"):
            fenced = not fenced
            continue
        if not fenced and line.strip() and not line.startswith("|"):
            out.append(line)
    return out


def commands(code):
    """The commands of a code block with continuation lines joined."""
    return [c.strip() for c in re.sub(r"\\\n\s*", " ", code).splitlines() if c.strip()]


def norm(s):
    """Maps the guide's names to the verify job's, so a line of one can be found in the other."""
    s = s.replace("refs/tags/v$VERSION", "refs/tags/${TAG}").replace('"v$VERSION"', '"${TAG}"')
    s = s.replace("shadewright-labs/sablecourt/", "${REPOSITORY}/")
    s = re.sub(r"--repo shadewright-labs/sablecourt\b", "--repo ${REPOSITORY}", s)
    s = re.sub(r"^(\s*)([A-Z_]+)=", lambda m: m.group(1) + m.group(2).lower() + "=", s)
    s = re.sub(r"^(\s*)for ([A-Z_]+) in ", lambda m: m.group(1) + "for " + m.group(2).lower() + " in ", s)
    s = s.replace("$IMAGE@$DIGEST", "${ref}").replace('"$SHA"', '"${GITHUB_SHA}"')
    s = re.sub(r"\$\{?([A-Za-z_]+)", lambda m: "${" + m.group(1).lower(), s)
    s = re.sub(r"\$\{([a-z_]+)\}?", r"${\1}", s)
    s = s.replace('"', "").replace("${dist}/", "").replace("${runner_temp}/", "")
    return s.strip()


VERIFY = J.get("verify", "")
VERIFY_LINES = [norm(l) for l in VERIFY.splitlines()]
VERIFY_NORM = "\n".join(VERIFY_LINES)
# The lines of the guide's code that are setup and do not occur in the verify job, after norm().
SETUP_LINES = {
    "version=0.1.0",
    "sha=" + "0" * 40,
    "mkdir sablecourt-${version}",
    "cd sablecourt-${version}",
    "gh release download ${tag} --repo ${repository}",
    "test ${lines} -eq 9",
    "image=ghcr.io/shadewright-labs/sablecourt",
    "digest=sha256:" + "0" * 64,
}
GH_FLAGS = ['--repo shadewright-labs/sablecourt', '--cert-identity "$IDENTITY"', '--cert-oidc-issuer "$ISSUER"',
            '--source-ref "refs/tags/v$VERSION"', '--source-digest "$SHA"', "--deny-self-hosted-runners"]
COSIGN_FLAGS = ['--certificate-identity "$IDENTITY"', '--certificate-oidc-issuer "$ISSUER"']
ABSOLUTE = re.compile(r"\b(only|always|never|every|any|nothing|cannot|exactly)\b", re.I)


def wf_step(job, step):
    m = re.search(r"^      - name: " + re.escape(step) + r"\n(.*?)(?=^      - name: |\Z)", J.get(job, ""), re.M | re.S)
    return m.group(1) if m else ""


def guide_code(page):
    return "\n".join(blocks(page))


def guide_cmds(page):
    return commands(guide_code(page))


def gh_cmds(page):
    return [c for c in guide_cmds(page) if c.startswith("gh attestation verify ")]


def cosign_cmds(page):
    return [c for c in guide_cmds(page) if c.startswith("cosign verify")]


def block_after(page, line):
    """The code block right before a prose line of the guide."""
    at = page.find("```\n\n" + line + "\n")
    found = blocks(page[:at + 4]) if at >= 0 else []
    return found[-1] if found else ""


def sbom_filter(text, var):
    m = re.search(r"""jq -se --slurpfile want "\$\{?""" + var + r"""\}?" '([^']*)'""", text)
    return m.group(1) if m else ""


def runs(filt, sbom_text):
    """Runs an SBOM comparison on an attestation with predicate {"a": 1} and the given SBOM file."""
    with tempfile.TemporaryDirectory() as d:
        payload = base64.b64encode(json.dumps({"predicate": {"a": 1, "b": [1, 2]}}).encode()).decode()
        with open(os.path.join(d, "att.json"), "w") as f:
            f.write(json.dumps({"payload": payload}) + "\n")
        with open(os.path.join(d, "sbom.json"), "w") as f:
            f.write(sbom_text)
        r = subprocess.run(["jq", "-se", "--slurpfile", "want", os.path.join(d, "sbom.json"), filt,
                            os.path.join(d, "att.json")], capture_output=True)
        return r.returncode == 0


def sbom_ok(filt):
    return (bool(filt) and runs(filt, '{"a": 1, "b": [1, 2]}\n') and runs(filt, '{"b":[1,2],"a":1}')
            and not runs(filt, '{"a": 1, "b": [1, 2]}\n{"c": 3}\n') and not runs(filt, '{"a": 1, "b": [1, 2]}\n[]\n')
            and not runs(filt, '{"a": 2, "b": [1, 2]}\n') and not runs(filt, ''))


def notes_name(*parts):
    notes = wf_step("publish", "Create the draft release")
    return all(p in notes for p in parts)


def scan_cmd():
    return wf_step("scan-images", "Scan every platform image by digest")


def needs(job):
    m = re.search(r"^    needs: (.+)$", J.get(job, ""), re.M)
    return {n.strip() for n in m.group(1).strip("[]").split(",")} if m else set()


def cmds_ok(page):
    """Each cosign command has the identity and issuer, each gh command the six flags, none a pattern."""
    code = guide_code(page)
    return (len(cosign_cmds(page)) == 3 and all(all(f in c for f in COSIGN_FLAGS) for c in cosign_cmds(page))
            and len(gh_cmds(page)) == 4 and all(all(f in c for f in GH_FLAGS) for c in gh_cmds(page))
            and not re.search(r"regexp|regex|--signer-|--insecure|--skip|--allow", code))


# Each prose line of the guide and of the release section of docs/ci.md, and what makes it true. A changed or new
# line fails until it is added here with its check.
GUIDE_CLAIMS = {
    "# Verify a release": lambda p: True,
    "You need Bash, GNU `sha256sum`, jq, cosign 3, the GitHub CLI (`gh`) logged in, and for the images Docker with Buildx.":
        lambda p: all(t in guide_code(p) for t in ("sha256sum ", "jq ", "cosign ", "gh ", "docker buildx "))
        and "cosign-release: v3." in WF,
    "A pass shows that the file or image is the one this repository's release workflow signed at the tag `v$VERSION`, and that its provenance names the source commit `$SHA`; it does not show who created the tag.":
        lambda p: cmds_ok(p),
    "## Commands": lambda p: True,
    "Put the blocks in this order in one file and run it with `bash`. Take `SHA` and the image digests from the release notes.":
        lambda p: notes_name("from ${GITHUB_SHA}", "${DAEMON_IMAGE}@${DAEMON_DIGEST}", "${AGENT_IMAGE}@${AGENT_DIGEST}")
        and blocks(p)[0].startswith("set -euo pipefail\n"),
    "A pass sets the certificate identity that the cosign and gh checks below require, and the commit `$SHA` that the gh checks require.":
        lambda p: cmds_ok(p) and 'IDENTITY="https://github.com/shadewright-labs/sablecourt/.github/workflows/release.yml@refs/tags/v$VERSION"'
        in block_after(p, "A pass sets the certificate identity that the cosign and gh checks below require, and the commit `$SHA` that the gh checks require."),
    "A pass puts the release's files in the folder `sablecourt-$VERSION`.":
        lambda p: 'gh release download "v$VERSION"' in block_after(p, "A pass puts the release's files in the folder `sablecourt-$VERSION`."),
    "A pass shows that `checksums.txt` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`.":
        lambda p: "cosign verify-blob " in block_after(p, "A pass shows that `checksums.txt` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`.")
        and "checksums.txt.sigstore.json" in block_after(p, "A pass shows that `checksums.txt` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`."),
    "A pass shows that `checksums.txt` lists nine files and that each has the SHA-256 recorded there.":
        lambda p: block_after(p, "A pass shows that `checksums.txt` lists nine files and that each has the SHA-256 recorded there.")
        == 'sha256sum --check --strict checksums.txt\nLINES=$(wc -l < checksums.txt)\ntest "$LINES" -eq 9\n',
    "A pass shows that each archive, SBOM and `checksums.txt` has build provenance for `release.yml` at the tag `v$VERSION` and the commit `$SHA`, from GitHub and from `sablecourt_${VERSION}_provenance.intoto.jsonl`.":
        lambda p: "for FILE in sablecourt_*.tar.gz sablecourt_*.spdx.json checksums.txt; do" in p
        and '--bundle "sablecourt_${VERSION}_provenance.intoto.jsonl"' in p,
    "For the agent image, set `IMAGE=ghcr.io/shadewright-labs/sablecourt-agent` and its digest in the next block.":
        lambda p: "AGENT_IMAGE: ghcr.io/shadewright-labs/sablecourt-agent\n" in WF,
    "A pass shows that the image index `$IMAGE@$DIGEST` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`.":
        lambda p: "cosign verify \\" in block_after(p, "A pass shows that the image index `$IMAGE@$DIGEST` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`."),
    "A pass shows that the linux/amd64 and linux/arm64 images of the index each carry a signed SPDX attestation equal to the release's SBOM file for that image, and that the file holds one JSON value.":
        lambda p: "for ARCH in amd64 arm64; do" in p and "cosign verify-attestation --type spdxjson" in p
        and sbom_ok(sbom_filter(guide_code(p), "SBOM")),
    "A pass shows that the image index has build provenance for `release.yml` at the tag `v$VERSION` and the commit `$SHA`, from GitHub and from `sablecourt_${VERSION}_image_${IMAGE##*/}.intoto.jsonl`.":
        lambda p: guide_code(p).count('gh attestation verify "oci://$IMAGE@$DIGEST"') == 2
        and '--bundle "sablecourt_${VERSION}_image_${IMAGE##*/}.intoto.jsonl"' in p,
    "## What this does not prove": lambda p: True,
    "- Who created the tag `v$VERSION`: the repository's tag ruleset decides that.":
        lambda p: "git tag" not in WF and "git verify-tag" not in guide_code(p),
    "- Which checks the tagged commit's `release.yml` ran: the certificate names the workflow file and the tag, not the file's content.":
        lambda p: True,
    "- That the commit `$SHA` is on `main`.": lambda p: "main" not in guide_code(p),
    "- That an image tag such as `$VERSION` points at `$DIGEST`: deploy by digest.":
        lambda p: "$IMAGE:" not in guide_code(p),
    "- That the images have no known vulnerabilities.": lambda p: "trivy" not in guide_code(p).lower(),
}
CI_CLAIMS = {
    "## Releases": lambda c: True,
    "`release.yml` runs on a pushed `v*` tag. [Verify a release](how-to/verify-a-release.md) lists the commands that check a release.":
        lambda c: re.search(r"^on:\n  push:\n    tags: \[\"v\*\"\]\n\n", WF, re.M) is not None
        and os.path.exists(os.path.join(root, "docs/how-to/verify-a-release.md")),
    "No job or step of `release.yml` has an `if:` or `continue-on-error`, so a job starts when the jobs in its `needs` have succeeded. The `needs` of each job:":
        lambda c: not re.search(r"^\s+(?:- )?(?:if|continue-on-error):", WF, re.M),
    "What can exist outside the run when a job other than `publish` fails, run artifacts left out. `hack/test/workflows_test.sh` derives both tables from `release.yml` and fails when one differs.":
        lambda c: "the failure table of docs/ci.md is the one the needs graph gives" in WORKFLOWS_TEST
        and "the needs table of docs/ci.md is the one of the workflow" in WORKFLOWS_TEST,
    "### Scan policy": lambda c: True,
    "- A HIGH or CRITICAL finding fails `scan-images`, with or without a fix, unless `.trivyignore.yaml` in the tagged commit accepts it, and the signing jobs need `scan-images`.":
        lambda c: "--severity HIGH,CRITICAL --exit-code 1 --ignorefile .trivyignore.yaml" in scan_cmd()
        and "--ignore-unfixed" not in WF and "ref:" not in J.get("scan-images", "")
        and all("scan-images" in needs(j) for j in ("sign-images", "sign-binaries")),
    "- Record an accepted finding as an entry in `.trivyignore.yaml`, in a reviewed pull request.":
        lambda c: re.search(r"^vulnerabilities:", TRIVYIGNORE, re.M) is not None,
    "- The vulnerability database is pinned by digest in `TRIVY_DB` in `release.yml`; change the pin in a pull request.":
        lambda c: re.search(r"^      TRIVY_DB: ghcr\.io/aquasecurity/trivy-db@sha256:[0-9a-f]{64}$", WF, re.M) is not None,
    "- When a tag fails, fix the cause on `main`, cut the next release candidate from the new commit, and do not reuse the tag.":
        lambda c: "git merge-base --is-ancestor" in J.get("check", "") and "hack/release-state.sh" in J.get("check", ""),
    "### Repository settings": lambda c: True,
    "The pipeline relies on these settings, which are kept outside the repository's files:":
        lambda c: not os.path.exists(os.path.join(root, ".github/rulesets")),
    "- Creating `v*` tags must be limited to repository admins by a tag ruleset.": lambda c: True,
    "- Immutable releases must be on.": lambda c: "gh release upload" not in WF and "--clobber" not in WF,
    "- Required reviewers on the `release` environment are optional, and none are set by default.":
        lambda c: re.search(r"^    environment: release$", J.get("publish", ""), re.M) is not None,
    "### Known limits": lambda c: True,
    "- Not pinned by a checksum in this repository: the Go toolchain that `actions/setup-go` installs from `go.mod`, and the runner's `gh`, `jq`, Docker and preinstalled Buildx.":
        lambda c: "go-version-file: go.mod" in J.get("binaries", "")
        and not any("hack/setup-buildx.sh" in J.get(j, "") for j in ("scan-images", "sign-images", "verify"))
        and not re.search(r"install[^\n]*\b(gh|jq)\b", WF),
    "- The pipeline has not run on a tag yet, so its tests check the workflow text and the scripts, not a run.":
        lambda c: True,
    "- Open from issue 122: stable `vX.Y.Z` tags are to be created by a release GitHub App, and `publish` writes image tags before it publishes the draft release, so a failure in between can leave tags next to a draft.":
        lambda c: "create-github-app-token" not in WF
        and WF.find("- name: Tag the verified image digests") < WF.find("- name: Publish the release") > 0,
}


def page_checks(guide, ci):
    code = guide_code(guide)
    rel = release_section(ci)
    out = []
    with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as f:
        f.write(code)
        script = f.name
    try:
        out.append(("guide: the code passes bash -n", subprocess.run(["bash", "-n", script], capture_output=True).returncode == 0))
        sc = subprocess.run(["shellcheck", "-s", "bash", script], capture_output=True, text=True)
        out.append(("guide: the code passes shellcheck", sc.returncode == 0))
        if sc.returncode:
            print(sc.stdout, file=sys.stderr)
    finally:
        os.unlink(script)
    lines = [l.strip() for l in code.splitlines() if l.strip()]
    out += [
        ("guide: every fence is a bash block", guide.count("```") == 2 * len(blocks(guide)) and len(blocks(guide)) == 8),
        ("guide: the first block starts with set -euo pipefail", blocks(guide)[:1] != [] and blocks(guide)[0].startswith("set -euo pipefail\n")),
        ("guide: each command whose output is used runs on its own line, as an assignment",
         all(re.match(r'^[A-Z_]+=\$\(', l) for l in lines if "$(" in l or "`" in l)),
        ("guide: no && or || or if, so set -e sees each failure",
         not re.search(r"&&|\|\||^\s*if |^\s*!", code, re.M)),
        ("guide: each code line is a setup line or part of a line of the verify job",
         all(norm(l) in SETUP_LINES or any(norm(l) in v for v in VERIFY_LINES) for l in lines)),
        ("guide: the identity is the verify job's", re.search(
            r'^IDENTITY="https://github\.com/shadewright-labs/sablecourt/\.github/workflows/release\.yml@refs/tags/v\$VERSION"$',
            code, re.M) is not None
         and 'identity="https://github.com/${REPOSITORY}/.github/workflows/release.yml@refs/tags/${TAG}"' in VERIFY),
        ("guide: each cosign and gh command has the identity, issuer, tag, commit and runner flags", cmds_ok(guide)),
        ("guide: the same cosign and gh commands as the verify job",
         sorted(re.findall(r"cosign (verify(?:-blob|-attestation)?) ", code)) == sorted(re.findall(r"cosign (verify(?:-blob|-attestation)?) ", VERIFY))
         and sum("--bundle" in c for c in gh_cmds(guide)) == 2 == len(re.findall(r'gh attestation verify \S+ \\\n\s+--bundle', VERIFY))
         and len(gh_cmds(guide)) == VERIFY.count("gh attestation verify ")),
        ("guide: each flag on the page is in the verify job",
         all(f in VERIFY for f in re.findall(r"(?<![\w-])--[a-z][a-z-]*", guide))),
        ("guide: each file name on the page is in the verify job", all(
            norm(n) in VERIFY_NORM
            for n in re.findall(r"[\w${}#*/.-]*\.(?:tar\.gz|jsonl|json|txt)\b", guide))),
        ("guide: each URL on the page is in the verify job", all(
            norm(u) in VERIFY_NORM for u in re.findall(r"https://[^\s\"`)]+", guide))),
        ("guide: each image name on the page is one release.yml pushes", set(re.findall(r"ghcr\.io/shadewright-labs/[a-z-]+", guide))
         == {"ghcr.io/shadewright-labs/sablecourt", "ghcr.io/shadewright-labs/sablecourt-agent"}
         and "DAEMON_IMAGE: ghcr.io/shadewright-labs/sablecourt\n" in WF and "AGENT_IMAGE: ghcr.io/shadewright-labs/sablecourt-agent\n" in WF),
        ("guide: the SBOM comparison fails on a second JSON value or another value", sbom_ok(sbom_filter(code, "SBOM"))),
        ("release.yml: the SBOM comparison fails on a second JSON value or another value", sbom_ok(sbom_filter(VERIFY, "sbom"))),
        ("guide: the intro is two lines", re.match(r"# Verify a release\n\n[^\n]+\n[^\n]+\n\n## ", guide) is not None),
        ("guide: each block is followed by one line that says what a pass shows",
         len(re.findall(r"^```\n\nA pass [^\n]+\n\n(?:[^\n]+\n\n)?(?:```bash|## )", guide, re.M)) == len(blocks(guide))),
        ("guide: at most five bullets on what it does not prove",
         len(re.findall(r"^- ", guide.split("## What this does not prove", 1)[-1], re.M)) <= 5
         and "## What this does not prove" in guide),
    ]
    for page, claims, name in ((guide, GUIDE_CLAIMS, "guide"), (rel, CI_CLAIMS, "ci.md, Releases")):
        prose = prose_lines(page)
        bare = [re.sub(r"`[^`]*`", "", l) for l in prose]
        out.append((f"{name}: no absolute word in the prose", not any(ABSOLUTE.search(b) for b in bare)))
        out.append((f"{name}: every prose line has a check", prose != [] and all(l in claims for l in prose)))
        out.append((f"{name}: every check has its line", all(k in prose for k in claims)))
        out.append((f"{name}: every check of a line passes", all(claims[l](page) for l in prose if l in claims)))
    return out


for name, ok in page_checks(GUIDE, CI):
    check(name, ok)


def sub(old, new, where="guide"):
    def mutate(guide, ci):
        if where == "guide":
            return guide.replace(old, new, 1), ci
        return guide, ci.replace(old, new, 1)
    mutate.changes = (old, where)
    return mutate


# Edits that make a page false; each must fail a check.
MUTATIONS = [
    sub('--source-ref "refs/tags/v$VERSION"', '--source-ref "refs/heads/main"'),
    sub('--bundle "sablecourt_${VERSION}_provenance.intoto.jsonl"', '--bundle "sablecourt_${VERSION}_provenance.json"'),
    sub("/.github/workflows/release.yml@refs/tags/v$VERSION", "/.github/workflows/release.yml@refs/heads/main"),
    sub("    --deny-self-hosted-runners > /dev/null\n", "    > /dev/null\n"),
    sub("any([.] == $want)'", "any(. == $want[0])'"),
    sub("test \"$LINES\" -eq 9", "test \"$LINES\" -eq 10"),
    sub("--certificate-identity \"$IDENTITY\"", "--certificate-identity-regexp \"$IDENTITY\""),
    sub('LINES=$(wc -l < checksums.txt)\ntest "$LINES" -eq 9', 'test "$(wc -l < checksums.txt)" -eq 9'),
    sub("from GitHub and from `sablecourt_${VERSION}_provenance.intoto.jsonl`.", "from GitHub."),
    sub("- That the images have no known vulnerabilities.\n", "- That the images have no known vulnerabilities.\n- That the release is reproducible.\n"),
    sub("A HIGH or CRITICAL finding", "A CRITICAL finding", "ci"),
    sub("| `verify` | `check`, `images`, `sign-binaries`, `sign-images` |", "| `verify` | `check`, `sign-binaries`, `sign-images` |", "ci"),
    sub("Immutable releases must be on.", "Immutable releases are always on.", "ci"),
    sub("by a tag ruleset.", "by a tag ruleset, which this repository enforces.", "ci"),
]
results = []
for m in MUTATIONS:
    old, where = m.changes
    guide, ci = m(GUIDE, CI)
    if (guide, ci) == (GUIDE, CI):
        check(f"mutation applies: {old[:60]!r}", False)
        continue
    failed = [n for n, ok in page_checks(guide, ci) if not ok]
    ci_failed = False
    if where == "ci" and old.startswith("| "):
        # The tables are checked by workflows_test.sh; run it on a copy with the edited page.
        with tempfile.TemporaryDirectory() as d:
            subprocess.run(["cp", "-R", os.path.join(root, ".github"), os.path.join(root, "hack"), d], check=True)
            os.makedirs(os.path.join(d, "docs/how-to"))
            with open(os.path.join(d, "docs/ci.md"), "w") as f:
                f.write(ci)
            with open(os.path.join(d, "docs/how-to/verify-a-release.md"), "w") as f:
                f.write(guide)
            ci_failed = subprocess.run([os.path.join(root, "hack/test/workflows_test.sh"), d], capture_output=True).returncode != 0
    check(f"mutated {where}: {old[:60]!r} fails {failed[:1] or ['workflows_test.sh'] if ci_failed else failed[:1]}", bool(failed) or ci_failed)
    results.append(m)
print(f"{len(results)} page mutation cases")

if fails:
    print(f"{fails} case(s) failed", file=sys.stderr)
    sys.exit(1)
print("all cases passed")
PY
