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
#   - the checksum check of the guide and of the verify job, run with bash on fixtures, passes for the nine release
#     files listed once each and fails for nine copies of one line, a repeated, missing or extra file;
#   - links in the prose go to the GitHub documentation pages listed in DOC_URLS;
#   - each prose line of both pages is in CLAIMS below, with a check of the workflow, the scripts or the guide's
#     code that makes it true, so an edited or added line fails until its check is written; no prose line uses
#     only, always, never, every, any, nothing, cannot or exactly;
#   - a mutation table edits copies of the pages and requires that a check fails on each.
# Usage: hack/test/verify-guide_test.sh [repository root]
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
python3 -I - "${root}" <<'PY'
import base64, functools, json, os, re, subprocess, sys, tempfile

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
RP_WF = read(".github/workflows/release-please.yml")
RP_J = jobs(RP_WF)
RP_CFG = json.loads(read("release-please-config.json"))
RP_PKG = RP_CFG.get("packages", {}).get(".", {})
RP_MANIFEST = json.loads(read(".release-please-manifest.json"))
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
    "test ${names} = ${want}",
    "found=$(find . -mindepth 1 -maxdepth 1 -exec basename {} \\; | LC_ALL=C sort)",
    "test ${found} = ${expected}",
    "image=ghcr.io/shadewright-labs/sablecourt",
    "digest=sha256:" + "0" * 64,
}
GH_FLAGS = ['--repo shadewright-labs/sablecourt', '--cert-identity "$IDENTITY"', '--cert-oidc-issuer "$ISSUER"',
            '--source-ref "refs/tags/v$VERSION"', '--source-digest "$SHA"', "--deny-self-hosted-runners"]
COSIGN_FLAGS = ['--certificate-identity "$IDENTITY"', '--certificate-oidc-issuer "$ISSUER"']
# The pages of GitHub's documentation that the prose cites for GitHub's own behaviour, each read for the sentence
# that cites it: the registry login for oci:// images, jobs skipped after a failed need, pending runs of a
# concurrency group, and what an immutable release locks.
GH_DOCS = "https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax"
DOC_URLS = {
    "https://cli.github.com/manual/gh_attestation_verify",
    GH_DOCS + "#jobsjob_idneeds", GH_DOCS + "#concurrency",
    "https://docs.github.com/en/code-security/supply-chain-security/understanding-your-software-supply-chain/immutable-releases",
}
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


@functools.lru_cache(maxsize=None)
def sbom_ok(filt):
    return (bool(filt) and runs(filt, '{"a": 1, "b": [1, 2]}\n') and runs(filt, '{"b":[1,2],"a":1}')
            and not runs(filt, '{"a": 1, "b": [1, 2]}\n{"c": 3}\n') and not runs(filt, '{"a": 1, "b": [1, 2]}\n[]\n')
            and not runs(filt, '{"a": 2, "b": [1, 2]}\n') and not runs(filt, ''))


RELEASE_FILES = (["sablecourt_0.1.0_sbom.spdx.json"]
                 + [f"sablecourt_0.1.0_{o}_{a}.tar.gz" for o in ("darwin", "linux") for a in ("amd64", "arm64")]
                 + [f"sablecourt_0.1.0_image_{i}_linux_{a}.spdx.json"
                    for i in ("sablecourt", "sablecourt-agent") for a in ("amd64", "arm64")])


def sums_pass(script, listed, extra=()):
    """Runs a checksum check (bash, in a folder with the nine release files of 0.1.0 and the extra ones) on a
    checksums.txt that lists the given names, each with its real SHA-256."""
    with tempfile.TemporaryDirectory() as d:
        for n in RELEASE_FILES + list(extra):
            with open(os.path.join(d, n), "w") as f:
                f.write(n + "\n")
        out = subprocess.run(["sha256sum", "--"] + list(listed), cwd=d, capture_output=True, text=True, check=True)
        with open(os.path.join(d, "checksums.txt"), "w") as f:
            f.write(out.stdout)
        env = {"PATH": os.environ["PATH"], "VERSION": "0.1.0", "DIST": d}
        r = subprocess.run(["bash", "-c", "set -euo pipefail\n" + script], cwd=d, env=env, capture_output=True)
        return r.returncode == 0


@functools.lru_cache(maxsize=None)
def sums_ok(script):
    """Passes for the nine files, each once; fails for nine copies of one line, a repeated file in place of
    another, a tenth file and a missing file."""
    one = RELEASE_FILES[0]
    return (bool(script) and sums_pass(script, RELEASE_FILES)
            and not sums_pass(script, [one] * 9)
            and not sums_pass(script, RELEASE_FILES[:-1] + [one])
            and not sums_pass(script, RELEASE_FILES + ["sablecourt_0.1.0_extra.tar.gz"], ["sablecourt_0.1.0_extra.tar.gz"])
            and not sums_pass(script, RELEASE_FILES[:-1]))


FOURTEEN = (RELEASE_FILES + ["checksums.txt", "checksums.txt.sigstore.json", "sablecourt_0.1.0_provenance.intoto.jsonl"]
            + [f"sablecourt_0.1.0_image_{i}.intoto.jsonl" for i in ("sablecourt", "sablecourt-agent")])


def folder_pass(script, names):
    """Runs the guide's download block, without mkdir, cd and the download, in a folder with the given files."""
    lines = [l for l in script.splitlines(keepends=True) if not re.match(r"^(mkdir|cd|gh release download) ", l)]
    with tempfile.TemporaryDirectory() as d:
        for n in names:
            open(os.path.join(d, n), "w").close()
        env = {"PATH": os.environ["PATH"], "VERSION": "0.1.0"}
        return subprocess.run(["bash", "-c", "set -euo pipefail\n" + "".join(lines)], cwd=d, env=env,
                              capture_output=True).returncode == 0


@functools.lru_cache(maxsize=None)
def folder_ok(script):
    """Passes for the fourteen release files; fails for an extra file, a hidden extra file and a missing file."""
    return (bool(script) and folder_pass(script, FOURTEEN) and not folder_pass(script, FOURTEEN + ["other.tar.gz"])
            and not folder_pass(script, FOURTEEN + [".x"]) and not folder_pass(script, FOURTEEN[1:]))


def guide_sums(page):
    """The guide's block that runs sha256sum --check."""
    return next((b for b in blocks(page) if "sha256sum --check" in b), "")


def verify_sums():
    """The part of the verify job's archive step from sha256sum --check to the gh loop."""
    step = wf_step("verify", "Verify the archives")
    at, end = step.find("(cd \"${DIST}\" && sha256sum --check"), step.find("for file in ")
    return step[at:end] if 0 <= at < end else ""


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


def blk(p, prefix):
    """The code block right before the first prose line of the guide that starts with prefix."""
    m = re.search(r"^```\n\n" + re.escape(prefix), p, re.M)
    found = blocks(p[:m.start() + 4]) if m else []
    return found[-1] if found else ""


def gh_ok(code, n):
    """code has n gh attestation verify commands, each with the six flags and no other predicate type."""
    cmds = [c for c in commands(code) if c.startswith("gh attestation verify ")]
    return len(cmds) == n and all(all(f in c for f in GH_FLAGS) for c in cmds) and "--predicate-type" not in code


def actor_unused():
    """github.actor appears only as the registry login name, and no step reads who pushed the tag."""
    return (all(l.strip() == "username: ${{ github.actor }}" for l in WF.splitlines() if "actor" in l)
            and "GITHUB_ACTOR" not in WF)


def release_scripts_tested():
    hack = os.path.join(root, "hack")
    scripts = [f[:-3] for f in os.listdir(hack) if f.startswith("release-") and f.endswith(".sh")]
    tests = os.path.join(hack, "test")
    return (scripts != [] and all(os.path.exists(os.path.join(tests, s + "_test.sh")) for s in scripts)
            and not any(re.search(r"\bact\b|gh workflow run|gh run rerun", read("hack/test/" + t))
                        for t in os.listdir(tests) if t.startswith("release-")))


# Each prose line of the guide and of the release section of docs/ci.md, and what makes it true. A changed or new
# line fails until it is added here with its check.
GUIDE_CLAIMS = {
    "# Verify a release": lambda p: True,
    "You need Bash, `cut`, `sort`, a `sha256sum` with `--check` and `--strict` (GNU coreutils has both), jq, cosign 3 and the GitHub CLI (`gh`) logged in to GitHub. For the images you also need Docker with Buildx and a login to `ghcr.io`, which `gh attestation verify` requires for an `oci://` image ([gh manual](https://cli.github.com/manual/gh_attestation_verify)).":
        lambda p: all(t in guide_code(p) for t in ("sha256sum --check --strict ", "cut ", "sort", "jq ", "cosign ",
                                                    "gh attestation verify ", "gh release download ", "docker buildx ",
                                                    'gh attestation verify "oci://'))
        and "cosign-release: v3." in WF,
    "A pass shows that `checksums.txt` and the image index you check carry signatures from a certificate for `release.yml` at the tag `v$VERSION`, that the archives and SBOMs have the SHA-256 values listed in `checksums.txt`, and that the build provenance of these files, of `checksums.txt` and of the index was signed with a certificate whose source repository digest is `$SHA`. It does not show who created the tag.":
        lambda p: cmds_ok(p) and sums_ok(guide_sums(p)) and "cosign verify-blob " in guide_code(p)
        and 'cosign verify \\\n' in guide_code(p) and "git " not in guide_code(p),
    "## Commands": lambda p: True,
    "Put the blocks in this order in one file and run it with `bash`. Take `SHA` and the image digests from the release notes, where the release workflow writes the commit it built and both image digests.":
        lambda p: notes_name("from ${GITHUB_SHA}", "${DAEMON_IMAGE}@${DAEMON_DIGEST}", "${AGENT_IMAGE}@${AGENT_DIGEST}")
        and blocks(p)[0].startswith("set -euo pipefail\n"),
    "A pass sets the certificate identity and issuer that the cosign and gh commands below require, and the source repository digest `$SHA` that the gh commands require.":
        lambda p: cmds_ok(p) and blk(p, "A pass sets ").splitlines()[3:] == [
            'IDENTITY="https://github.com/shadewright-labs/sablecourt/.github/workflows/release.yml@refs/tags/v$VERSION"',
            'ISSUER="https://token.actions.githubusercontent.com"'] and blk(p, "A pass sets ").startswith(
            "set -euo pipefail\nVERSION=0.1.0\nSHA="),
    "A pass puts the release's files in the folder `sablecourt-$VERSION` and shows that the folder holds the fourteen files the release workflow attaches and no others.":
        lambda p: blk(p, "A pass puts ").startswith('mkdir "sablecourt-$VERSION"\ncd "sablecourt-$VERSION"\ngh release download "v$VERSION" --repo shadewright-labs/sablecourt\n')
        and folder_ok(blk(p, "A pass puts ")) and '"${DIST}"/*' in wf_step("publish", "Create the draft release")
        and "'%s\\n' checksums.txt checksums.txt.sigstore.json" in wf_step("verify", "Check the set of files"),
    "A pass shows that `checksums.txt` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`.":
        lambda p: "cosign verify-blob " in blk(p, "A pass shows that `checksums.txt` carries")
        and "--bundle checksums.txt.sigstore.json" in blk(p, "A pass shows that `checksums.txt` carries")
        and all(f in blk(p, "A pass shows that `checksums.txt` carries") for f in COSIGN_FLAGS),
    "A pass shows that each file listed in `checksums.txt` is in the folder with the SHA-256 listed for it, and that the list names the four archives and the five SBOMs of `$VERSION`, each once.":
        lambda p: blk(p, "A pass shows that each file listed").startswith("sha256sum --check --strict checksums.txt\n")
        and sums_ok(blk(p, "A pass shows that each file listed")),
    "A pass shows that each archive and SBOM in the folder and `checksums.txt` has an SLSA build provenance attestation signed with a certificate for `release.yml` at the tag `v$VERSION`, with the source ref `refs/tags/v$VERSION`, the source repository digest `$SHA` and no self-hosted runner, once as fetched from GitHub's API and once from `sablecourt_${VERSION}_provenance.intoto.jsonl`.":
        lambda p: blk(p, "A pass shows that each archive").startswith("for FILE in sablecourt_*.tar.gz sablecourt_*.spdx.json checksums.txt; do\n")
        and folder_ok(blk(p, "A pass puts ")) and len([n for n in FOURTEEN if re.match(r"sablecourt_.*\.(tar\.gz|spdx\.json)$", n)]) == 9
        and gh_ok(blk(p, "A pass shows that each archive"), 2)
        and blk(p, "A pass shows that each archive").count('--bundle "sablecourt_${VERSION}_provenance.intoto.jsonl"') == 1,
    "The next three blocks check the daemon image. To check the agent image too, add them to the end of the file a second time, with `IMAGE=ghcr.io/shadewright-labs/sablecourt-agent` and the agent's digest.":
        lambda p: "AGENT_IMAGE: ghcr.io/shadewright-labs/sablecourt-agent\n" in WF and len(blocks(p)) == 8
        and blocks(p)[5].startswith("IMAGE=ghcr.io/shadewright-labs/sablecourt\nDIGEST=sha256:")
        and re.search(r"mkdir|gh release download", "".join(blocks(p)[5:])) is None,
    "A pass shows that the image index `$IMAGE@$DIGEST` carries a signature from a certificate for `release.yml` at the tag `v$VERSION`.":
        lambda p: 'cosign verify \\\n' in blk(p, "A pass shows that the image index `")
        and all(f in blk(p, "A pass shows that the image index `") for f in COSIGN_FLAGS),
    "A pass shows that, for linux/amd64 and for linux/arm64, the index lists one image, that image has SPDX attestations verified with a certificate for `release.yml` at the tag `v$VERSION`, and the predicate of one of them equals the release's SBOM file for that image, which holds one JSON value.":
        lambda p: blk(p, "A pass shows that, for linux").startswith("for ARCH in amd64 arm64; do\n")
        and "if length == 1 then .[0] else error" in blk(p, "A pass shows that, for linux")
        and "cosign verify-attestation --type spdxjson" in blk(p, "A pass shows that, for linux")
        and "| .predicate) | any([.] == $want)" in blk(p, "A pass shows that, for linux")
        and sbom_ok(sbom_filter(blk(p, "A pass shows that, for linux"), "SBOM")),
    "A pass shows that the image index has an SLSA build provenance attestation signed with a certificate for `release.yml` at the tag `v$VERSION`, with the source ref `refs/tags/v$VERSION`, the source repository digest `$SHA` and no self-hosted runner, once as fetched from GitHub's API and once from `sablecourt_${VERSION}_image_${IMAGE##*/}.intoto.jsonl`.":
        lambda p: blk(p, "A pass shows that the image index has").count('gh attestation verify "oci://$IMAGE@$DIGEST"') == 2
        and gh_ok(blk(p, "A pass shows that the image index has"), 2) and "--bundle-from-oci" not in p
        and blk(p, "A pass shows that the image index has").count('--bundle "sablecourt_${VERSION}_image_${IMAGE##*/}.intoto.jsonl"') == 1,
    "## What this does not prove": lambda p: True,
    "- Who created the tag `v$VERSION`. An active tag ruleset with a creation restriction that matches the tag limits who may create it, but `release.yml` does not check for one, and a ruleset does not show who created this tag.":
        lambda p: "git " not in guide_code(p) and actor_unused(),
    "- Which checks the release workflow ran. The commands check the certificate's identity, issuer, source ref and source repository digest; they do not read `release.yml` at `$SHA`.":
        lambda p: cmds_ok(p) and re.search(r"\bgit |curl|gh api|release\.yml[^@]", guide_code(p)) is None,
    "- That the commit `$SHA` is on `main`.": lambda p: "main" not in guide_code(p),
    "- That an image tag such as `$VERSION` points at `$DIGEST`: deploy by digest.":
        lambda p: "$IMAGE:" not in guide_code(p),
    "- That the images have no known vulnerabilities.": lambda p: "trivy" not in guide_code(p).lower(),
}
CI_CLAIMS = {
    "## Releases": lambda c: True,
    "Two workflows handle releases: `release-please.yml` cuts stable releases from Conventional Commits on `main`, and `release.yml` builds, scans, signs, verifies and publishes the release and container images when a `v*` tag is pushed. [Verify a release](how-to/verify-a-release.md) lists the commands that check a release.":
        lambda c: re.search(r"^on:\n  push:\n    branches: \[main\]\n\n", RP_WF, re.M) is not None
        and "googleapis/release-please-action@" in RP_WF
        and re.search(r"^on:\n  push:\n    tags: \[\"v\*\"\]\n\n", WF, re.M) is not None
        and os.path.exists(os.path.join(root, "docs/how-to/verify-a-release.md")),
    "### Cutting a release": lambda c: True,
    "`release-please.yml` runs on pushes to `main` under concurrency group `release-please` (`cancel-in-progress: false`), in the `release` environment, with job-level `contents: read` and a short-lived token minted from the `shadewright-release` GitHub App (`vars.RELEASE_APP_CLIENT_ID` and `secrets.RELEASE_APP_PRIVATE_KEY`) with `contents: write`, `pull-requests: write` and `issues: write` scoped to `sablecourt`.":
        lambda c: re.search(r"^on:\n  push:\n    branches: \[main\]\n\n", RP_WF, re.M) is not None
        and re.search(r"^permissions: \{\}$", RP_WF, re.M) is not None
        and re.search(r"^concurrency:\n  group: release-please\n  cancel-in-progress: false\n\n", RP_WF, re.M) is not None
        and re.search(r"^    environment: release$", RP_J.get("release-please", ""), re.M) is not None
        and re.search(r"^    permissions:\n      contents: read\n    steps:", RP_J.get("release-please", ""), re.M) is not None
        and "actions/create-github-app-token@" in RP_WF
        and "client-id: ${{ vars.RELEASE_APP_CLIENT_ID }}" in RP_WF
        and "private-key: ${{ secrets.RELEASE_APP_PRIVATE_KEY }}" in RP_WF
        and "repositories: sablecourt" in RP_WF
        and "permission-contents: write" in RP_WF
        and "permission-pull-requests: write" in RP_WF
        and "permission-issues: write" in RP_WF,
    "Configured by `release-please-config.json` and `.release-please-manifest.json`, it keeps one open release pull request (`chore: release ${version}`). On 0.x versions, `feat` and breaking changes bump minor (`bump-minor-pre-major: true`, `bump-patch-for-minor-pre-major: false`), while `fix`, `perf` and `revert` bump patch; `refactor`, `test`, `docs`, `build`, `ci` and `chore` stay hidden from `CHANGELOG.md`.":
        lambda c: "config-file: release-please-config.json" in RP_WF
        and "manifest-file: .release-please-manifest.json" in RP_WF
        and RP_CFG.get("pull-request-title-pattern") == "chore: release ${version}"
        and RP_PKG.get("bump-minor-pre-major") is True
        and RP_PKG.get("bump-patch-for-minor-pre-major") is False
        and "." in RP_MANIFEST
        and {s["type"] for s in RP_PKG.get("changelog-sections", []) if not s.get("hidden")} == {"feat", "fix", "perf", "revert"}
        and {s["type"] for s in RP_PKG.get("changelog-sections", []) if s.get("hidden")} == {"refactor", "test", "docs", "build", "ci", "chore"}
        and os.path.exists(os.path.join(root, "CHANGELOG.md")),
    "The release pull request carries the edits to `CHANGELOG.md`, `.release-please-manifest.json`, `deploy/k8s/base/deployment.yaml` and `deploy/local/docker-compose.yml`. When a maintainer merges that pull request, the push to `main` triggers `release-please.yml` to create a draft GitHub release and push a plain `vX.Y.Z` tag (`draft: true`, `force-tag-creation: true`) through the App token so the tag push starts `release.yml`. Do not push plain `vX.Y.Z` tags by hand, and do not edit `CHANGELOG.md` or `.release-please-manifest.json` outside the release pull request.":
        lambda c: RP_CFG.get("include-component-in-tag") is False
        and RP_PKG.get("draft") is True
        and RP_PKG.get("force-tag-creation") is True
        and {"type": "generic", "path": "deploy/k8s/base/deployment.yaml"} in RP_PKG.get("extra-files", [])
        and {"type": "generic", "path": "deploy/local/docker-compose.yml"} in RP_PKG.get("extra-files", [])
        and re.search(r"ghcr\.io/shadewright-labs/sablecourt:[0-9.]+ # x-release-please-version$", read("deploy/k8s/base/deployment.yaml"), re.M) is not None
        and re.search(r"ghcr\.io/shadewright-labs/sablecourt:[0-9.]+ # x-release-please-version$", read("deploy/local/docker-compose.yml"), re.M) is not None
        and "token: ${{ steps.app-token.outputs.token }}" in RP_WF,
    "For a pre-release (`vX.Y.Z-rc.N`), a maintainer tags a commit on `main` whose `ci-ok` check passed and pushes the tag by hand. `release.yml` builds, scans, signs, verifies and publishes it as a pre-release without moving `latest`. Because `release-please` tracks the last release from `.release-please-manifest.json`, hand-cut release candidate tags stay separate from `release-please`.":
        lambda c: r"(-rc\.[1-9][0-9]*)?$ ]]" in J.get("check", "")
        and "hack/release-ci-ok.sh" in J.get("check", "")
        and 'if [ "${PRERELEASE}" = true ]; then\n            gh release edit "${TAG}" --draft=false --prerelease\n' in J.get("publish", "")
        and "manifest-file: .release-please-manifest.json" in RP_WF,
    "### Building and publishing a tag": lambda c: True,
    "`release.yml` runs on a pushed `v*` tag. In `check`, `hack/release-ci-ok.sh --wait` polls `ci.yml` on `main` for the tagged commit at 30-second intervals for up to 25 minutes while CI is in progress; it fails immediately if the newest run concluded with a failure, or at the 25-minute bound (after which a maintainer can rerun `release.yml` once `ci-ok` passes).":
        lambda c: re.search(r"^on:\n  push:\n    tags: \[\"v\*\"\]\n\n", WF, re.M) is not None
        and 'hack/release-ci-ok.sh --wait "${GH_REPO}" "${GITHUB_SHA}" 1500 30' in J.get("check", "")
        and "timeout-minutes: 30" in J.get("check", "")
        and "Wait for the CI run on main to pass, then rerun this workflow." in read("hack/release-ci-ok.sh"),
    "In `publish`, `release.yml` deletes existing drafts for the tag (including the draft created by `release-please`), creates a fresh draft with the fourteen verified files, image digests, build details and release notes (from the matching section of `CHANGELOG.md` via `hack/release-notes.sh` for a plain `vX.Y.Z` tag when present, or `--generate-notes` for pre-releases and as a fallback), checks the assets byte for byte, tags the verified image digests, publishes the release and checks the published assets again.":
        lambda c: 'gh api --method DELETE "repos/${GH_REPO}/releases/${id}"' in wf_step("publish", "Remove the drafts of earlier runs")
        and 'gh release create "${TAG}" "${flags[@]}" "${DIST}"/*' in wf_step("publish", "Create the draft release")
        and 'bash hack/release-notes.sh "${VERSION}" CHANGELOG.md' in wf_step("publish", "Create the draft release")
        and "--generate-notes" in wf_step("publish", "Create the draft release")
        and 'hack/release-assets.sh "${TAG}" "${RUNNER_TEMP}/dist"' in wf_step("publish", "Check the draft's assets byte for byte")
        and 'hack/release-assets.sh "${TAG}" "${RUNNER_TEMP}/dist"' in wf_step("publish", "Check the published assets byte for byte")
        and "docker buildx imagetools create" in wf_step("publish", "Tag the verified image digests")
        and 'gh release edit "${TAG}" --draft=false' in wf_step("publish", "Publish the release"),
    "No job or step of `release.yml` has an `if:` or `continue-on-error`. GitHub skips a job when a job in its `needs` fails or is skipped ([`needs`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idneeds)), so no job of `release.yml` runs after a job it needs has failed.":
        lambda c: not re.search(r"^\s+(?:- )?(?:if|continue-on-error):", WF, re.M),
    "A job whose `needs` have succeeded can still wait. A run stays pending while another run of the concurrency group `release` is in progress, and a newer run of the group cancels a pending one and waits in its place ([`concurrency`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#concurrency)).":
        lambda c: re.search(r"^concurrency:\n  group: release\n  cancel-in-progress: false\n\n", WF, re.M) is not None
        and "queue:" not in WF
        and not any(re.search(r"^\s+environment:", J[j], re.M) for j in J),
    "The `needs` of each job:":
        lambda c: "the needs table of docs/ci.md is the one of the workflow" in WORKFLOWS_TEST,
    "What can exist outside the run when a job other than `publish` fails, run artifacts left out. `hack/test/workflows_test.sh` derives both tables from `release.yml`. It reads the rows whose first cell is a job name in backticks, and fails when they differ from what it derived or name a job twice.":
        lambda c: "the failure table of docs/ci.md is the one the needs graph gives" in WORKFLOWS_TEST
        and "the needs table of docs/ci.md is the one of the workflow" in WORKFLOWS_TEST
        and "def unique(pairs):" in WORKFLOWS_TEST and "docs table mutation cases" in WORKFLOWS_TEST,
    "### Scan policy": lambda c: True,
    "- A HIGH or CRITICAL finding fails `scan-images`, with or without a fix, unless `.trivyignore.yaml` in the tagged commit accepts it, and the signing jobs need `scan-images`.":
        lambda c: "--severity HIGH,CRITICAL --exit-code 1 --ignorefile .trivyignore.yaml" in scan_cmd()
        and "--ignore-unfixed" not in WF and "ref:" not in J.get("scan-images", "")
        and all("scan-images" in needs(j) for j in ("sign-images", "sign-binaries")),
    "- Record an accepted finding as an entry in `.trivyignore.yaml`, in a reviewed pull request.":
        lambda c: re.search(r"^vulnerabilities:", TRIVYIGNORE, re.M) is not None,
    "- The vulnerability database is pinned by digest in `TRIVY_DB` in `release.yml`; change the pin in a pull request.":
        lambda c: re.search(r"^      TRIVY_DB: ghcr\.io/aquasecurity/trivy-db@sha256:[0-9a-f]{64}$", WF, re.M) is not None
        and '--db-repository "${TRIVY_DB}"' in scan_cmd(),
    "- When a tag fails, fix the cause on `main`, cut the next release candidate from the new commit, and do not reuse the tag.":
        lambda c: "git merge-base --is-ancestor" in J.get("check", "") and "hack/release-state.sh" in J.get("check", ""),
    "### Repository settings": lambda c: True,
    "`release.yml` and `release-please.yml` do not set these repository settings, and no file in this repository holds their values:":
        lambda c: not any(os.path.exists(os.path.join(root, ".github", f)) for f in ("rulesets", "settings.yml"))
        and re.search(r"rulesets|environments/|immutable", WF + RP_WF, re.I) is None,
    "- The `shadewright-release` GitHub App installed on this repository, with `RELEASE_APP_CLIENT_ID` set as a variable and `RELEASE_APP_PRIVATE_KEY` set as a secret in the `release` environment, which `release-please.yml` uses alone (`release.yml` names no `environment:`). Restrict `release` to the `main` branch so tag runs do not have access to the App key, and leave required reviewers unset so pushes to `main` are not paused.":
        lambda c: "vars.RELEASE_APP_CLIENT_ID" in RP_WF and "secrets.RELEASE_APP_PRIVATE_KEY" in RP_WF
        and re.search(r"^    environment: release$", RP_J.get("release-please", ""), re.M) is not None
        and re.search(r"^\s+environment:", WF, re.M) is None
        and "RELEASE_APP_PRIVATE_KEY" not in WF
        and re.search(r"^\s+(?:reviewers|required_reviewers|wait_timer):", WF + RP_WF, re.M) is None,
    "- A tag ruleset that limits who may create `v*` tags. Add the `shadewright-release` App as a bypass actor on tag creation while keeping tag updates and deletions blocked. No step of `release.yml` checks who pushed the tag.":
        lambda c: actor_unused(),
    "- Immutable releases. GitHub's documentation says that the assets of a published immutable release are protected from modification and deletion, and that its tag is locked to its commit ([immutable releases](https://docs.github.com/en/code-security/supply-chain-security/understanding-your-software-supply-chain/immutable-releases)). `publish` attaches the files to a draft before it publishes it.":
        lambda c: "gh release upload" not in WF and "--clobber" not in WF
        and 'flags=(--verify-tag --draft ' in wf_step("publish", "Create the draft release")
        and 'gh release create "${TAG}" "${flags[@]}" "${DIST}"/*' in wf_step("publish", "Create the draft release")
        and WF.find("- name: Create the draft release") < WF.find("--draft=false"),
    "### When a release goes wrong": lambda c: True,
    "- A release pull request fails `repo-checks` because a commit subject added to `CHANGELOG.md` trips `hack/docs-links.py` (such as an undefined `[a][b]` reference) or `hack/check-image-refs.sh`. Direct edits to `CHANGELOG.md` on the bot's branch are overwritten on the next push to `main`, so fix it either by using a `release-please` release-notes override in the pull request body (untested locally, as the action's docs are not vendored here) or by updating the checker's allowance on `main` before merging.":
        lambda c: os.path.exists(os.path.join(root, "hack/docs-links.py"))
        and os.path.exists(os.path.join(root, "hack/check-image-refs.sh"))
        and "hack/docs-links.py check" in read(".github/workflows/repo-checks.yml")
        and "hack/check-image-refs.sh" in read(".github/workflows/repo-checks.yml"),
    "- `release-please` pushes the `vX.Y.Z` tag and `release.yml` publishes the immutable release, yet `release-please` fails before creating its draft release. Later pushes to `main` then fail trying to create a draft for a tag that already has a published release; relabel the merged release pull request from `autorelease: pending` to `autorelease: tagged` by hand to clear the retry loop.":
        lambda c: RP_PKG.get("draft") is True and RP_PKG.get("force-tag-creation") is True,
    "### Known limits": lambda c: True,
    "- Not pinned by a checksum in this repository: the Go toolchain that `actions/setup-go` installs from `go.mod`, and the runner's `gh`, `jq`, Docker and preinstalled Buildx.":
        lambda c: "go-version-file: go.mod" in J.get("binaries", "")
        and not any("hack/setup-buildx.sh" in J.get(j, "") for j in ("scan-images", "sign-images", "verify"))
        and not re.search(r"install[^\n]*\b(gh|jq)\b", WF),
    "- The tests read the text of `release.yml`, `release-please.yml`, `release-please-config.json` and `.release-please-manifest.json` and run the release scripts under `hack/` on fixtures and stub commands. No test runs `release.yml` or `release-please.yml`.":
        lambda c: release_scripts_tested() and 'read(".github/workflows/release.yml")' in WORKFLOWS_TEST
        and 'read(".github/workflows/release-please.yml")' in read("hack/test/verify-guide_test.sh"),
    "- No step of `release.yml` creates a Git tag: a run starts from a pushed tag (created by `release-please.yml` for `vX.Y.Z`, or pushed by hand for `vX.Y.Z-rc.N`).":
        lambda c: re.search(r"git tag|git push|/git/refs|--method POST", WF) is None and "--verify-tag" in WF
        and re.search(r"^on:\n  push:\n    tags:", WF, re.M) is not None
        and RP_PKG.get("force-tag-creation") is True,
    "- `publish` writes the image tags before it publishes the draft release, and no step removes them, so a failure in between can leave image tags next to a draft release.":
        lambda c: 0 < WF.find("- name: Tag the verified image digests") < WF.find("- name: Publish the release")
        and re.search(r"imagetools rm|/packages/|--method DELETE \"repos/\$\{GH_REPO\}/(?!releases/)", WF) is None,
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
        ("guide: each URL in the code is in the verify job", all(
            norm(u) in VERIFY_NORM for u in re.findall(r"https://[^\s\"`)]+", code))),
        ("guide and ci.md, Releases: each link in the prose is a documentation page the claims name",
         all(u in DOC_URLS for u in re.findall(r"\]\((https://[^)\s]+)\)", "\n".join(prose_lines(guide) + prose_lines(rel))))),
        ("guide: each image name on the page is one release.yml pushes", set(re.findall(r"ghcr\.io/shadewright-labs/[a-z-]+", guide))
         == {"ghcr.io/shadewright-labs/sablecourt", "ghcr.io/shadewright-labs/sablecourt-agent"}
         and "DAEMON_IMAGE: ghcr.io/shadewright-labs/sablecourt\n" in WF and "AGENT_IMAGE: ghcr.io/shadewright-labs/sablecourt-agent\n" in WF),
        ("guide: the SBOM comparison fails on a second JSON value or another value", sbom_ok(sbom_filter(code, "SBOM"))),
        ("release.yml: the SBOM comparison fails on a second JSON value or another value", sbom_ok(sbom_filter(VERIFY, "sbom"))),
        ("guide: the checksum check requires the nine release files, each once", sums_ok(guide_sums(guide))),
        ("release.yml: the checksum check requires the nine release files, each once", sums_ok(verify_sums())),
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
    sub('| LC_ALL=C sort)\nNAMES', '| LC_ALL=C sort -u)\nNAMES'),
    sub('-maxdepth 1 -exec basename', "-maxdepth 1 -name 'sablecourt_*' -exec basename"),
    sub('test "$FOUND" = "$EXPECTED"\n', ''),
    sub('NAMES=$(cut -c 67- checksums.txt | LC_ALL=C sort)', 'NAMES=$(cut -c 67- checksums.txt | LC_ALL=C sort -u)'),
    sub('_linux_{amd64,arm64}.spdx.json | LC_ALL=C sort)\nNAMES', '_linux_amd64.spdx.json | LC_ALL=C sort)\nNAMES'),
    sub("| .predicate) | any", ") | any"),
    sub("  --source-digest \"$SHA\" \\\n  --deny-self-hosted-runners > /dev/null\ngh attestation verify \"oci",
        "  --deny-self-hosted-runners > /dev/null\ngh attestation verify \"oci"),
    sub("--certificate-identity \"$IDENTITY\"", "--certificate-identity-regexp \"$IDENTITY\""),
    sub('NAMES=$(cut -c 67- checksums.txt | LC_ALL=C sort)\ntest "$NAMES" = "$WANT"',
        'test "$(cut -c 67- checksums.txt | LC_ALL=C sort)" = "$WANT"'),
    sub("once as fetched from GitHub's API and once from `sablecourt_${VERSION}_provenance.intoto.jsonl`.", "from GitHub."),
    sub("- That the images have no known vulnerabilities.\n", "- That the images have no known vulnerabilities.\n- That the release is reproducible.\n"),
    sub("A HIGH or CRITICAL finding", "A CRITICAL finding", "ci"),
    sub("| `verify` | `check`, `images`, `sign-binaries`, `sign-images` |", "| `verify` | `check`, `sign-binaries`, `sign-images` |", "ci"),
    sub("- Immutable releases. ", "- Immutable releases, which `release.yml` turns on. ", "ci"),
    sub("limits who may create `v*` tags.", "limits who may create `v*` tags and records who did.", "ci"),
    sub("[`needs`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idneeds)",
        "[`needs`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobs)", "ci"),
    sub("| `scan-images` | untagged image digests in GHCR | nothing |\n",
        "| `scan-images` | nothing | nothing |\n| `scan-images` | untagged image digests in GHCR | nothing |\n", "ci"),
    sub("| `scan-images` | `images` |\n", "| `scan-images` | `check` |\n| `scan-images` | `images` |\n", "ci"),
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
