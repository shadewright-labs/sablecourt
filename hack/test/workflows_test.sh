#!/usr/bin/env bash
# Structural tests of the workflows, for properties that a workflow edit could quietly lose:
#   - no job downloads an executable without a checksum kept in this repository: no setup-buildx-action, no
#     sbom-action; Buildx and BuildKit (by digest) come from hack/setup-buildx.sh, which checks a sha256.
#   - the coverage upload: the race run writes the profile, a separate job uploads it only after the tests
#     passed, skips the upload without a token and cannot hide a failure from ci-ok (no continue-on-error), and
#     ci.yml passes it only the Codecov token.
#   - release.yml: per-job permissions, push and sign by digest, SBOMs and provenance, verification of every
#     artifact with the public commands before the publish job, and a draft that is published last. A mutation
#     table removes each of these from a copy of the file and requires the matching check to fail.
#   - every action is pinned by commit with its version; the verify page names release.yml as the signer.
# Usage: hack/test/workflows_test.sh [repository root]
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
python3 -I - "${root}" <<'PY'
import os, re, sys

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
    """Maps each job name to its text (the lines indented under it in the jobs: block)."""
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
            continue
        if cur:
            out[cur] += line + "\n"
    return out


for wf in sorted(os.listdir(os.path.join(root, ".github/workflows"))):
    text = read(".github/workflows/" + wf)
    check(f"{wf}: no setup-buildx-action", "docker/setup-buildx-action" not in text)
    check(f"{wf}: no sbom-action", "anchore/sbom-action" not in text)
for wf in ("images-build.yml", "release.yml"):
    check(f"{wf}: Buildx from hack/setup-buildx.sh", "hack/setup-buildx.sh" in read(".github/workflows/" + wf))
setup = read("hack/setup-buildx.sh")
check("setup-buildx: BuildKit pinned by digest", re.search(r"^BUILDKIT_IMAGE=moby/buildkit:v[0-9.]+@sha256:[0-9a-f]{64}$", setup, re.M) is not None)
check("setup-buildx: Buildx pinned by sha256", re.search(r"^BUILDX_SHA256=[0-9a-f]{64}$", setup, re.M) is not None)
for script in sorted(os.listdir(os.path.join(root, "hack"))):
    if script.endswith(".sh"):
        text = read("hack/" + script)
        if re.search(r"^\s*curl ", text, re.M):
            check(f"{script}: every download is checked against a sha256",
                  text.count("sha256sum --check --strict") >= len(re.findall(r"^\s*curl ", text, re.M)))

tj = jobs(read(".github/workflows/test.yml"))
test_job, cov = tj.get("test", ""), tj.get("coverage", "")
check("test: the race run writes the coverage profile",
      "go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./..." in test_job)
check("test: the profile is uploaded as an artifact",
      "actions/upload-artifact@" in test_job and "path: coverage.out" in test_job and "if-no-files-found: error" in test_job)
check("coverage: a separate job after a successful test job",
      "needs: test" in cov and "needs.test.result == 'success'" in cov)
check("coverage: no continue-on-error anywhere in test.yml", "continue-on-error" not in read(".github/workflows/test.yml"))
check("coverage: every step but the notice needs the token",
      cov.count("if: env.CODECOV_TOKEN != ''") == 3 and cov.count("if: env.CODECOV_TOKEN == ''") == 1)
check("coverage: pinned action, uploader version and slug",
      re.search(r"codecov/codecov-action@[0-9a-f]{40} ", cov) is not None and "version: v11.3.2" in cov
      and "slug: shadewright-labs/sablecourt" in cov and "files: coverage.out" in cov and "disable_search: true" in cov)
check("coverage: only contents: read", re.findall(r"^\s+[a-z-]+: (?:read|write)$", cov, re.M) == ["      contents: read"])
cj = jobs(read(".github/workflows/ci.yml"))
check("ci: the test job gets the Codecov token and nothing else",
      re.search(r"secrets:\n\s+CODECOV_TOKEN: \$\{\{ secrets\.CODECOV_TOKEN \}\}\n", cj.get("test", "")) is not None
      and "secrets: inherit" not in read(".github/workflows/ci.yml"))

# release.yml: the properties that keep a release signed, verified before it is public, and safe to rerun.
# Each check runs on the real file and must pass; each entry of MUTATIONS then removes or changes one thing in a
# copy of the file and requires that the named check fails on it, so no check passes without what it guards.
RELEASE_JOBS = {
    "check": {"contents: read"},
    "binaries": {"contents: read"},
    "sign-binaries": {"contents: read", "id-token: write", "attestations: write"},
    "images": {"contents: read", "packages: write"},
    "sign-images": {"contents: read", "packages: write", "id-token: write", "attestations: write"},
    "verify": {"contents: read", "packages: read", "attestations: read"},
    "publish": {"contents: write", "packages: write"},
}
RELEASE_NEEDS = {
    "check": set(),
    "binaries": {"check"},
    "sign-binaries": {"binaries"},
    "images": {"check"},
    "sign-images": {"images"},
    "verify": {"check", "images", "sign-binaries", "sign-images"},
    "publish": {"check", "binaries", "images", "verify"},
}
IDENTITY = ('identity="^https://github\\.com/${REPOSITORY//./\\\\.}/\\.github/workflows/release\\.yml'
            '@refs/tags/${TAG//./\\\\.}$"')
ATTEST_VERIFY = ('--signer-workflow "${REPOSITORY}/.github/workflows/release.yml" \\\n'
                 '              --source-ref "refs/tags/${TAG}" \\\n'
                 '              --source-digest "${GITHUB_SHA}" \\\n'
                 '              --deny-self-hosted-runners')


def grants(job_text):
    m = re.search(r"^    permissions:\n((?:      \S.*\n)+)", job_text, re.M)
    if not m:
        return None
    return {re.sub(r"\s*#.*$", "", line).strip() for line in m.group(1).splitlines()}


def needs(job_text):
    m = re.search(r"^    needs: (.+)$", job_text, re.M)
    if not m:
        return set()
    return {n.strip() for n in m.group(1).strip("[]").split(",")}


def in_order(text, *parts):
    """True when each part occurs in text, each after the previous one."""
    at = 0
    for part in parts:
        at = text.find(part, at)
        if at < 0:
            return False
        at += len(part)
    return True


def release_checks(text):
    j = jobs(text)
    pub, ver, img, simg, sbin, bins, chk = (j.get(n, "") for n in
                                            ("publish", "verify", "images", "sign-images", "sign-binaries", "binaries", "check"))
    out = [
        ("permissions: {} at the top", re.search(r"^permissions: \{\}$", text, re.M) is not None),
        ("runs only on a pushed v* tag",
         re.search(r"^on:\n  push:\n    tags: \[\"v\*\"\]\n\n", text, re.M) is not None
         and not re.search(r"workflow_dispatch|pull_request|workflow_run|schedule:|workflow_call", text)),
        ("one run per tag, never cancelled", "group: release-${{ github.ref }}\n  cancel-in-progress: false" in text),
        ("exactly the seven jobs", set(j) == set(RELEASE_JOBS)),
    ]
    for name, want in RELEASE_JOBS.items():
        out.append((f"{name}: grants exactly {', '.join(sorted(want))}", grants(j.get(name, "")) == want))
    for name, want in RELEASE_NEEDS.items():
        out.append((f"{name}: needs {', '.join(sorted(want)) or 'nothing'}", needs(j.get(name, "")) == want))
    out += [
        ("check: version is X.Y.Z or X.Y.Z-rc.N",
         r"=~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$ ]]" in chk),
        ("check: the tagged commit is on main",
         'git merge-base --is-ancestor "${GITHUB_SHA}" origin/main' in chk and "fetch-depth: 0" in chk),
        ("check: refuses a published release", '[ "${state}" = false ]' in chk),
        ("binaries: the pinned package script, no Go cache, no target override",
         'hack/package-binaries.sh "${VERSION}"' in bins and "cache: false" in bins and "RELEASE_TARGETS" not in text),
        ("binaries: SBOM of the binaries, listed in checksums.txt",
         in_order(bins, "hack/install-syft.sh", 'syft" scan "dir:${out}/bin"', "sablecourt_${VERSION}_sbom.spdx.json",
                  "sha256sum -- sablecourt_* > checksums.txt")),
        ("sign-binaries: keyless signature bundle of checksums.txt",
         'cosign sign-blob --yes --bundle "${DIST}/checksums.txt.sigstore.json" "${DIST}/checksums.txt"' in sbin),
        ("sign-binaries: provenance for the archives, the SBOM and checksums.txt",
         in_order(sbin, "actions/attest-build-provenance@", "subject-path: |\n",
                  "/dist/sablecourt_*.tar.gz\n", "/dist/sablecourt_*_sbom.spdx.json\n", "/dist/checksums.txt\n")),
        ("images: both pushed by digest, with no tag",
         img.count(",push-by-digest=true,name-canonical=true,push=true") == 2
         and not re.search(r"^\s+tags:", img, re.M) and "push: true" not in img),
        ("sign-images: signs the digests, never a tag",
         in_order(simg, 'for ref in "${DAEMON_IMAGE}@${DAEMON_DIGEST}" "${AGENT_IMAGE}@${AGENT_DIGEST}"; do',
                  'cosign sign --yes "${ref}"')
         and re.findall(r"cosign sign --yes (\S+)", text) == ['"${ref}"']),
        ("sign-images: an SPDX SBOM attestation per platform image",
         in_order(simg, "for arch in amd64 arm64; do", 'syft" scan "registry:${image}@${child}"',
                  'cosign attest --yes --type spdxjson --predicate "${sbom}" "${image}@${child}"')),
        ("sign-images: provenance on both index digests, stored in the registry",
         in_order(simg, "subject-digest: ${{ env.DAEMON_DIGEST }}\n          push-to-registry: true",
                  "subject-digest: ${{ env.AGENT_DIGEST }}\n          push-to-registry: true")),
        ("verify: exactly the expected files",
         in_order(ver, 'expected="$(printf', '"${expected}" ]; then')),
        ("verify: the checksums signature against this tag's identity",
         in_order(ver, IDENTITY, 'cosign verify-blob \\\n            --bundle "${DIST}/checksums.txt.sigstore.json"',
                  '--certificate-identity-regexp "${identity}"',
                  '--certificate-oidc-issuer "${issuer}"', "sha256sum --check --strict checksums.txt")),
        ("verify: provenance of every file, bound to this tag and commit",
         in_order(ver, 'for file in "${DIST}"/sablecourt_* "${DIST}/checksums.txt"; do',
                  'gh attestation verify "${file}"', ATTEST_VERIFY)),
        ("verify: image signatures and SBOM attestations against this tag's identity",
         in_order(ver, "- name: Verify the images", IDENTITY, "cosign verify \\\n",
                  '--certificate-identity-regexp "${identity}"', '"${ref}" > /dev/null',
                  "cosign verify-attestation --type spdxjson", '"${image}@${child}" > /dev/null')),
        ("verify: image provenance bound to this tag and commit",
         in_order(ver, "- name: Verify the images", 'gh attestation verify "oci://${ref}"',
                  ATTEST_VERIFY.replace("\n              ", "\n              "))),
        ("verify: the GitHub OIDC issuer", ver.count('issuer="https://token.actions.githubusercontent.com"') == 2),
        ("publish: refuses a moved tag and a published release before writing",
         in_order(pub, 'if [ "${sha}" != "${GITHUB_SHA}" ]', "grep -q ' false$'", "gh release create")),
        ("publish: draft, assets checked, images tagged, then published and checked again",
         in_order(pub, 'gh release create "${TAG}"', 'hack/release-assets.sh "${TAG}"',
                  "docker buildx imagetools create", "--draft=false", 'hack/release-assets.sh "${TAG}"')),
        ("publish: the release is created as a draft on the existing tag",
         "flags=(--verify-tag --draft " in pub),
        ("publish: version tags before latest, each checked against the verified digest",
         in_order(pub, 'tags=("${VERSION}")', "tags+=(latest)", "imagetools create", '"${got}" != "${digest}"')),
        ("publish: a release candidate stays a pre-release and never gets latest",
         in_order(pub, 'if [ "${PRERELEASE}" = true ]; then\n            flags+=(--prerelease)')
         and in_order(pub, 'if [ "${PRERELEASE}" != true ]; then\n            tags+=(latest)')
         and in_order(pub, '--draft=false --prerelease\n          else\n            gh release edit "${TAG}" --draft=false --latest')),
        ("only publish writes releases", all(
            not re.search(r"gh release (create|edit|upload|delete)|--method DELETE", t)
            for n, t in j.items() if n != "publish")),
        ("no asset replacement, tag deletion or git tag write",
         not re.search(r"--clobber|--cleanup-tag|git push|git tag|gh release upload|gh release delete", text)),
        ("cosign is installed at a pinned release",
         text.count("sigstore/cosign-installer@") == text.count("cosign-release: v3.1.4") == 3),
    ]
    return out


MUTATIONS = [
    ("permissions: {} at the top", "permissions: {}\n\nconcurrency", "permissions:\n  contents: read\n\nconcurrency"),
    ("runs only on a pushed v* tag", 'tags: ["v*"]\n', 'tags: ["v*"]\n  workflow_dispatch:\n'),
    ("one run per tag, never cancelled", "cancel-in-progress: false", "cancel-in-progress: true"),
    ("exactly the seven jobs", "\n  verify:\n", "\n  verify-all:\n"),
    ("check: version is X.Y.Z or X.Y.Z-rc.N", "(-rc\\.[1-9][0-9]*)?$ ]]", "(-.*)?$ ]]"),
    ("check: the tagged commit is on main", 'git merge-base --is-ancestor "${GITHUB_SHA}" origin/main', "true"),
    ("check: refuses a published release", '[ "${state}" = false ]', "false"),
    ("binaries: the pinned package script, no Go cache, no target override", "cache: false #", "cache: true #"),
    ("binaries: SBOM of the binaries, listed in checksums.txt", "sha256sum -- sablecourt_* > checksums.txt",
     "sha256sum -- *.tar.gz > checksums.txt"),
    ("sign-binaries: keyless signature bundle of checksums.txt", "cosign sign-blob --yes --bundle", "echo"),
    ("sign-binaries: provenance for the archives, the SBOM and checksums.txt",
     "            ${{ runner.temp }}/dist/checksums.txt\n", ""),
    ("images: both pushed by digest, with no tag", "push-by-digest=true,name-canonical=true,push=true\n\n  sign-images",
     "push=true\n          tags: ${{ env.AGENT_IMAGE }}:latest\n\n  sign-images"),
    ("sign-images: signs the digests, never a tag", 'cosign sign --yes "${ref}"', 'cosign sign --yes "${image}:latest"'),
    ("sign-images: an SPDX SBOM attestation per platform image", "cosign attest --yes --type spdxjson", "echo"),
    ("sign-images: provenance on both index digests, stored in the registry",
     "subject-digest: ${{ env.AGENT_DIGEST }}\n          push-to-registry: true",
     "subject-digest: ${{ env.AGENT_DIGEST }}\n          push-to-registry: false"),
    ("verify: exactly the expected files", '"${expected}" ]; then', '"${expected}" ] && false; then'),
    ("verify: the checksums signature against this tag's identity", "sha256sum --check --strict checksums.txt", "true"),
    ("verify: provenance of every file, bound to this tag and commit",
     'for file in "${DIST}"/sablecourt_* "${DIST}/checksums.txt"; do', 'for file in "${DIST}/checksums.txt"; do'),
    ("verify: image signatures and SBOM attestations against this tag's identity",
     "cosign verify-attestation --type spdxjson", "echo"),
    ("verify: image provenance bound to this tag and commit", 'gh attestation verify "oci://${ref}"', 'echo "oci://${ref}"'),
    ("verify: the GitHub OIDC issuer", 'issuer="https://token.actions.githubusercontent.com"\n          for ref',
     'issuer="https://example.com"\n          for ref'),
    ("publish: refuses a moved tag and a published release before writing", "grep -q ' false$'", "false"),
    ("publish: draft, assets checked, images tagged, then published and checked again",
     '      - name: Check the draft\'s assets byte for byte\n        run: hack/release-assets.sh "${TAG}" "${RUNNER_TEMP}/dist"\n', ""),
    ("publish: the release is created as a draft on the existing tag", "flags=(--verify-tag --draft ", "flags=(--draft "),
    ("publish: version tags before latest, each checked against the verified digest",
     '"${got}" != "${digest}"', '"${got}" = ""'),
    ("publish: a release candidate stays a pre-release and never gets latest",
     'if [ "${PRERELEASE}" != true ]; then\n            tags+=(latest)', "if true; then\n            tags+=(latest)"),
    ("only publish writes releases", "      - name: Install syft (pinned)\n        run: hack/install-syft.sh",
     "      - name: Early release\n        run: gh release create x\n\n      - name: Install syft (pinned)\n        run: hack/install-syft.sh"),
    ("no asset replacement, tag deletion or git tag write", '"${flags[@]}" "${DIST}"/*', '"${flags[@]}" --clobber "${DIST}"/*'),
    ("cosign is installed at a pinned release", "          cosign-release: v3.1.4\n\n      - name: Sign checksums",
     "\n      - name: Sign checksums"),
]



def in_job(job, old, new):
    """A mutation that replaces old by new inside one job of the file."""
    def mutate(text):
        start = text.index(f"\n  {job}:\n")
        nxt = re.search(r"\n  [A-Za-z0-9_-]+:\n", text[start + 1:])
        end = start + 1 + nxt.start() if nxt else len(text)
        block = text[start:end]
        return text if old not in block else text[:start] + block.replace(old, new, 1) + text[end:]
    return mutate


for job, want in RELEASE_JOBS.items():
    MUTATIONS.append((f"{job}: grants exactly {', '.join(sorted(want))}",
                      in_job(job, "    permissions:\n", "    permissions:\n      actions: write\n")))
for job, want in RELEASE_NEEDS.items():
    MUTATIONS.append((f"{job}: needs {', '.join(sorted(want)) or 'nothing'}",
                      in_job(job, "    runs-on:", "    needs: images\n    runs-on:") if not want
                      else in_job(job, "    needs: ", "    needs: [publish]\n    # needs: ")))

release = read(".github/workflows/release.yml")
for name, ok in release_checks(release):
    check(f"release.yml: {name}", ok)
names = {name for name, _ in release_checks(release)}
for name, *change in MUTATIONS:
    mutated = change[0](release) if callable(change[0]) else release.replace(change[0], change[1], 1)
    results = dict(release_checks(mutated))
    check(f"release.yml without it: {name} fails", name in names and mutated != release and results.get(name) is False)
check("release.yml: every check has a mutation that breaks it", names == {m[0] for m in MUTATIONS})
for wf in sorted(os.listdir(os.path.join(root, ".github/workflows"))):
    for use in re.findall(r"^\s+(?:- )?uses: (.+)$", read(".github/workflows/" + wf), re.M):
        check(f"{wf}: {use.split('@')[0].split(' ')[0]} pinned by commit with its version",
              use.startswith("./") or re.match(r"^[\w.-]+/[\w./-]+@[0-9a-f]{40} # v\d", use) is not None)
verify_doc = read("docs/how-to/verify-a-release.md")
check("verify-a-release.md: the signer is release.yml of this repository",
      "--signer-workflow shadewright-labs/sablecourt/.github/workflows/release.yml" in verify_doc
      and "IDENTITY='^https://github\\.com/shadewright-labs/sablecourt/\\.github/workflows/release\\.yml@refs/tags/" in verify_doc)

if fails:
    print(f"{fails} case(s) failed", file=sys.stderr)
    sys.exit(1)
print("all cases passed")
PY
