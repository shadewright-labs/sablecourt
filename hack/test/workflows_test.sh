#!/usr/bin/env bash
# Structural tests of the workflows, for properties that a workflow edit could quietly lose:
#   - no job downloads an executable without a checksum kept in this repository: no setup-buildx-action, no
#     sbom-action; Buildx and BuildKit (by digest) come from hack/setup-buildx.sh, which checks a sha256.
#   - the coverage upload: the race run writes the profile, a separate job uploads it only after the tests
#     passed, skips the upload without a token and cannot hide a failure from ci-ok (no continue-on-error), and
#     ci.yml passes it only the Codecov token.
#   - release.yml: per-job permissions and needs, the checks of the check job (version, main, release state, ci-ok),
#     push, scan and signing by digest, SBOMs and provenance files, the exact file set, verification of every
#     artifact with the exact identity, issuer, commit and runner before the publish job, and in publish the
#     refusals before any write, the latest rule and a draft that is published last. Each check reads the job and
#     step it is about. A mutation table removes or weakens each property in a copy of the file and requires the
#     matching check to fail.
#   - every action is pinned by commit with its version, and every checkout keeps no credentials; the verify page
#     uses the exact identity of release.yml and --deny-self-hosted-runners in its commands.
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

# release.yml: the properties that keep a release signed, scanned, verified before it is public, and safe to rerun.
# Each check reads the job and the step it is about (jobs(), steps()), so a property found in another step does
# not count. Each check runs on the real file and must pass; each entry of MUTATIONS then removes or changes one
# thing in a copy of the file and requires that the named check fails on it, so no check passes without what it
# guards.
RELEASE_JOBS = {
    "check": {"contents: read", "checks: read"},
    "binaries": {"contents: read"},
    "images": {"contents: read", "packages: write"},
    "scan-images": {"contents: read", "packages: read"},
    "sign-images": {"contents: read", "packages: write", "id-token: write", "attestations: write"},
    "sign-binaries": {"contents: read", "id-token: write", "attestations: write"},
    "verify": {"contents: read", "packages: read", "attestations: read"},
    "publish": {"contents: write", "packages: write"},
}
RELEASE_NEEDS = {
    "check": set(),
    "binaries": {"check"},
    "images": {"check"},
    "scan-images": {"images"},
    "sign-images": {"check", "images"},
    "sign-binaries": {"check", "binaries", "sign-images"},
    "verify": {"check", "images", "sign-binaries", "sign-images"},
    "publish": {"check", "binaries", "images", "scan-images", "verify"},
}
IDENTITY = 'identity="https://github.com/${REPOSITORY}/.github/workflows/release.yml@refs/tags/${TAG}"'
ISSUER = 'issuer="https://token.actions.githubusercontent.com"'
# The provenance checks of gh: the exact certificate identity, the issuer, this tag and this commit, and a
# GitHub-hosted runner. The indentation differs between the two verify steps, so it is matched loosely.
ATTEST = re.compile(r'--repo "\$\{REPOSITORY\}" \\\n\s+--cert-identity "\$\{identity\}" \\\n'
                    r'\s+--cert-oidc-issuer "\$\{issuer\}" \\\n\s+--source-ref "refs/tags/\$\{TAG\}" \\\n'
                    r'\s+--source-digest "\$\{GITHUB_SHA\}" \\\n\s+--deny-self-hosted-runners > /dev/null\n')
CHILD = ('[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $a) | .digest]'
         ' | if length == 1 then .[0] else error("not one \\($a) image") end')
REFS = 'for ref in "${DAEMON_IMAGE}@${DAEMON_DIGEST}" "${AGENT_IMAGE}@${AGENT_DIGEST}"; do'


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


def steps(job_text):
    """Maps each step name of a job to its text, in order."""
    out = {}
    for part in re.split(r"^      - name: ", job_text, flags=re.M)[1:]:
        name, _, body = part.partition("\n")
        out[name.strip()] = body
    return out


def in_order(text, *parts):
    """True when each part occurs in text, each after the previous one."""
    at = 0
    for part in parts:
        at = text.find(part, at)
        if at < 0:
            return False
        at += len(part)
    return True


def line(text, cmd):
    """True when cmd is a whole line of text (indentation aside): not in an if, not followed by || or &&."""
    return re.search(r"^\s*" + re.escape(cmd) + r"$", text, re.M) is not None


def release_checks(text):
    j = jobs(text)
    s = {name: steps(body) for name, body in j.items()}

    def st(job, step):
        return s.get(job, {}).get(step, "")

    meta = st("check", "Check the version, the commit and the release state")
    ciok = st("check", "Require a passed ci-ok on main for the tagged commit")
    scan = st("scan-images", "Scan every platform image by digest")
    sign = st("sign-images", "Sign the images and attest their SBOMs (keyless)")
    keep_img = st("sign-images", "Keep the provenance bundles as release files")
    sums = st("sign-binaries", "Write checksums.txt")
    keep_bin = st("sign-binaries", "Keep the provenance bundle as a release file")
    files = st("verify", "Check the set of files")
    varch = st("verify", "Verify the archives")
    vimg = st("verify", "Verify the images")
    refuse = st("publish", "Refuse a moved tag or a published release")
    keep_tag = st("publish", "Refuse to move a version image tag")
    latest = st("publish", "Decide whether this release moves latest")
    tagging = st("publish", "Tag the verified image digests")
    publish = st("publish", "Publish the release")
    pub_steps = list(s.get("publish", {}))
    checkouts = [(n, b) for job in s.values() for n, b in job.items() if "actions/checkout@" in b]

    def step_order(*names):
        return all(n in pub_steps for n in names) and [pub_steps.index(n) for n in names] == sorted(pub_steps.index(n) for n in names)

    out = [
        ("permissions: {} at the top", re.search(r"^permissions: \{\}$", text, re.M) is not None),
        ("runs only on a pushed v* tag",
         re.search(r"^on:\n  push:\n    tags: \[\"v\*\"\]\n\n", text, re.M) is not None
         and not re.search(r"workflow_dispatch|pull_request|workflow_run|schedule:|workflow_call", text)),
        ("one release at a time for every tag, never cancelled",
         re.search(r"^concurrency:\n  group: release\n  cancel-in-progress: false\n", text, re.M) is not None),
        ("exactly the eight jobs", set(j) == set(RELEASE_JOBS)),
        ("no job or step is skipped by a condition or allowed to fail",
         not re.search(r"^\s+(?:- )?(?:if|continue-on-error):", text, re.M)),
        ("the version and the tag come from the check job and the pushed ref everywhere",
         len(re.findall(r"^\s+VERSION: .*$", text, re.M)) == 6
         and all(v == "${{ needs.check.outputs.version }}" for v in re.findall(r"^\s+VERSION: (.*)$", text, re.M))
         and len(re.findall(r"^\s+TAG: .*$", text, re.M)) == 3
         and all(v == "${{ github.ref_name }}" for v in re.findall(r"^\s+TAG: (.*)$", text, re.M))),
        ("every checkout keeps no credentials",
         len(checkouts) == 6 and all("persist-credentials: false" in b and "persist-credentials: true" not in b
                                     for _, b in checkouts)),
    ]
    for name, want in RELEASE_JOBS.items():
        out.append((f"{name}: grants exactly {', '.join(sorted(want))}", grants(j.get(name, "")) == want))
    for name, want in RELEASE_NEEDS.items():
        out.append((f"{name}: needs {', '.join(sorted(want)) or 'nothing'}", needs(j.get(name, "")) == want))
    out += [
        ("check: version is X.Y.Z or X.Y.Z-rc.N",
         r"=~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$ ]]" in meta),
        ("check: the tagged commit is on main",
         'if ! git merge-base --is-ancestor "${GITHUB_SHA}" origin/main; then' in meta
         and "fetch-depth: 0" in st("check", "Clone the code")),
        ("check: a published release or a failed lookup stops the run",
         line(meta, 'hack/release-state.sh "${TAG}" > /dev/null') and "set -euo pipefail" in meta),
        ("check: ci-ok passed on main for the tagged commit",
         in_order(ciok, "set -euo pipefail",
                  'gh api "repos/${GH_REPO}/commits/${GITHUB_SHA}/check-runs?check_name=ci-ok&per_page=100" > "${RUNNER_TEMP}/runs.json"',
                  'gh api "repos/${GH_REPO}/commits/${GITHUB_SHA}/check-suites?per_page=100" > "${RUNNER_TEMP}/suites.json"')
         and line(ciok, 'hack/release-ci-ok.sh "${GITHUB_SHA}" "${RUNNER_TEMP}/runs.json" "${RUNNER_TEMP}/suites.json"')),
        ("binaries: the pinned package script, no setup-go cache, no target override",
         'hack/package-binaries.sh "${VERSION}"' in st("binaries", "Build and pack the archives")
         and "cache: false" in st("binaries", "Set up Go") and "RELEASE_TARGETS" not in text),
        ("binaries: SBOM of the binaries from the pinned syft",
         "hack/install-syft.sh" in st("binaries", "Install syft (pinned)")
         and in_order(st("binaries", "Write the SBOM of the binaries"), 'syft" scan "dir:${out}/bin"',
                      '-o "spdx-json=${out}/dist/sablecourt_${VERSION}_sbom.spdx.json"')),
        ("images: both images for linux/amd64 and linux/arm64",
         re.findall(r"^\s+platforms: (.*)$", text, re.M) == ["linux/amd64,linux/arm64"] * 2
         and in_order(j.get("images", ""), "target: daemon\n          platforms: linux/amd64,linux/arm64",
                      "target: agent\n          platforms: linux/amd64,linux/arm64")),
        ("images: both pushed by digest, with no tag",
         j.get("images", "").count(",push-by-digest=true,name-canonical=true,push=true") == 2
         and not re.search(r"^\s+tags:", j.get("images", ""), re.M) and "push: true" not in j.get("images", "")),
        ("scan-images: Trivy from the pinned installer",
         line(st("scan-images", "Install Trivy (pinned)"), 'run: hack/install-trivy.sh "${RUNNER_TEMP}/trivy-bin"')),
        ("scan-images: every platform image of both images, by digest",
         in_order(scan, "set -euo pipefail", REFS, "for arch in amd64 arm64; do", CHILD,
                  '"${RUNNER_TEMP}/trivy-bin/trivy" image --image-src remote --scanners vuln', '"${image}@${child}"\n')),
        ("scan-images: HIGH and CRITICAL fail unless .trivyignore.yaml accepts them",
         "--severity HIGH,CRITICAL --exit-code 1 --ignorefile .trivyignore.yaml" in scan
         and "--ignore-unfixed" not in text and "|| true" not in scan),
        ("sign-images: signs the digests, never a tag",
         in_order(sign, REFS, 'cosign sign --yes "${ref}"') and re.findall(r"cosign sign --yes (\S+)", text) == ['"${ref}"']),
        ("sign-images: an SPDX SBOM attestation per platform image, kept as a release file",
         in_order(sign, "for arch in amd64 arm64; do", CHILD,
                  'sbom="${RUNNER_TEMP}/images-out/sablecourt_${VERSION}_image_${image##*/}_linux_${arch}.spdx.json"',
                  'syft" scan "registry:${image}@${child}" -o "spdx-json=${sbom}"',
                  'cosign attest --yes --type spdxjson --predicate "${sbom}" "${image}@${child}"')),
        ("sign-images: provenance on both index digests, stored in the registry",
         "subject-digest: ${{ env.DAEMON_DIGEST }}\n          push-to-registry: true"
         in st("sign-images", "Attest the build provenance of sablecourtd")
         and "subject-digest: ${{ env.AGENT_DIGEST }}\n          push-to-registry: true"
         in st("sign-images", "Attest the build provenance of sablecourt-agent")),
        ("sign-images: both provenance bundles kept as .intoto.jsonl release files",
         "DAEMON_BUNDLE: ${{ steps.daemon-provenance.outputs.bundle-path }}" in keep_img
         and "AGENT_BUNDLE: ${{ steps.agent-provenance.outputs.bundle-path }}" in keep_img
         and line(keep_img, 'jq -ec . "${DAEMON_BUNDLE}" > "${RUNNER_TEMP}/images-out/sablecourt_${VERSION}_image_sablecourt.intoto.jsonl"')
         and line(keep_img, 'jq -ec . "${AGENT_BUNDLE}" > "${RUNNER_TEMP}/images-out/sablecourt_${VERSION}_image_sablecourt-agent.intoto.jsonl"')),
        ("sign-binaries: checksums.txt lists the archives and every SBOM",
         line(sums, '(cd "${DIST}" && sha256sum -- sablecourt_*.tar.gz sablecourt_*.spdx.json > checksums.txt)')),
        ("sign-binaries: keyless signature bundle of checksums.txt",
         'cosign sign-blob --yes --bundle "${DIST}/checksums.txt.sigstore.json" "${DIST}/checksums.txt"'
         in st("sign-binaries", "Sign checksums.txt (keyless)")),
        ("sign-binaries: provenance for the archives, the SBOMs and checksums.txt",
         in_order(st("sign-binaries", "Attest the build provenance"), "actions/attest-build-provenance@", "subject-path: |\n",
                  "/dist/sablecourt_*.tar.gz\n", "/dist/sablecourt_*.spdx.json\n", "/dist/checksums.txt\n")),
        ("sign-binaries: the provenance bundle kept as a .intoto.jsonl release file",
         "BUNDLE: ${{ steps.provenance.outputs.bundle-path }}" in keep_bin
         and line(keep_bin, 'jq -ec . "${BUNDLE}" > "${RUNNER_TEMP}/signed/sablecourt_${VERSION}_provenance.intoto.jsonl"')),
        ("verify: exactly the expected fourteen files",
         in_order(files, 'expected="$(printf', "checksums.txt checksums.txt.sigstore.json",
                  '"sablecourt_${VERSION}_sbom.spdx.json" "sablecourt_${VERSION}_provenance.intoto.jsonl"',
                  '"sablecourt_${VERSION}_"{darwin,linux}_{amd64,arm64}.tar.gz',
                  '"sablecourt_${VERSION}_image_"{sablecourt,sablecourt-agent}_linux_{amd64,arm64}.spdx.json',
                  '"sablecourt_${VERSION}_image_"{sablecourt,sablecourt-agent}.intoto.jsonl',
                  '"${expected}" ]; then', "exit 1")),
        ("verify: the checksums signature against this tag's exact identity",
         in_order(varch, "set -euo pipefail", IDENTITY, ISSUER,
                  'cosign verify-blob \\\n            --bundle "${DIST}/checksums.txt.sigstore.json" \\\n'
                  '            --certificate-identity "${identity}" \\\n            --certificate-oidc-issuer "${issuer}" \\\n'
                  '            "${DIST}/checksums.txt"\n', '(cd "${DIST}" && sha256sum --check --strict checksums.txt)')),
        ("verify: checksums.txt lists exactly nine files",
         'if [ "$(wc -l < "${DIST}/checksums.txt")" -ne 9 ]; then' in varch),
        ("verify: provenance of every file from the API and from the release bundle, bound to this tag and commit",
         in_order(varch, 'for file in "${DIST}"/sablecourt_*.tar.gz "${DIST}"/sablecourt_*.spdx.json "${DIST}/checksums.txt"; do',
                  'gh attestation verify "${file}" \\\n              --repo',
                  'gh attestation verify "${file}" \\\n              --bundle "${DIST}/sablecourt_${VERSION}_provenance.intoto.jsonl"')
         and len(ATTEST.findall(varch)) == 2),
        ("verify: the stamped sablectl prints the version",
         line(varch, '"${RUNNER_TEMP}/sablectl" version | grep -F -- "${VERSION}"')),
        ("verify: image signatures and SBOM attestations against this tag's exact identity",
         in_order(vimg, "set -euo pipefail", IDENTITY, ISSUER, REFS,
                  'cosign verify \\\n              --certificate-identity "${identity}" \\\n'
                  '              --certificate-oidc-issuer "${issuer}" \\\n              "${ref}" > /dev/null\n',
                  "for arch in amd64 arm64; do", CHILD,
                  'cosign verify-attestation --type spdxjson \\\n                --certificate-identity "${identity}" \\\n'
                  '                --certificate-oidc-issuer "${issuer}" \\\n                "${image}@${child}" > ')),
        ("verify: every release SBOM of an image is a verified attestation's predicate",
         in_order(vimg, 'sbom="${DIST}/sablecourt_${VERSION}_image_${image##*/}_linux_${arch}.spdx.json"',
                  """jq -se --slurpfile want "${sbom}" 'map(.payload | @base64d | fromjson | .predicate) | any(. == $want[0])'""")),
        ("verify: image provenance from the registry and from the release bundle, bound to this tag and commit",
         in_order(vimg, 'gh attestation verify "oci://${ref}" \\\n              --repo',
                  'gh attestation verify "oci://${ref}" \\\n              --bundle "${DIST}/sablecourt_${VERSION}_image_${image##*/}.intoto.jsonl"')
         and len(ATTEST.findall(vimg)) == 2),
        ("verify: exact identities only, no regular expression or signer-workflow prefix",
         not re.search(r"identity-regexp|cert-identity-regex|--signer-workflow|--signer-repo", text)),
        ("publish: refuses a moved tag and a published release before writing",
         'sha="$(gh api "repos/${GH_REPO}/commits/tags/${TAG}" --jq .sha)"' in refuse
         and 'if [ "${sha}" != "${GITHUB_SHA}" ]; then' in refuse
         and line(refuse, 'hack/release-state.sh "${TAG}" > "${RUNNER_TEMP}/drafts"')
         and step_order("Refuse a moved tag or a published release", "Refuse to move a version image tag",
                        "Decide whether this release moves latest", "Remove the drafts of earlier runs",
                        "Create the draft release")),
        ("publish: never moves a version image tag to another digest",
         in_order(keep_tag, "set -euo pipefail", REFS, 'hack/release-image-tag.sh "${ref%@*}" "${VERSION}" "${ref#*@}"')
         and in_order(tagging, 'if [ "${tag}" = "${VERSION}" ]; then', 'hack/release-image-tag.sh "${image}" "${tag}" "${digest}"',
                      'docker buildx imagetools create --tag "${image}:${tag}" "${ref}"')),
        ("publish: latest only for the highest full release",
         in_order(latest, "set -euo pipefail", 'gh api --paginate "repos/${GH_REPO}/git/matching-refs/tags/v"',
                  'latest="$(hack/release-latest.sh "${VERSION}" < "${RUNNER_TEMP}/tags")"', 'echo "latest=${latest}" >> "$GITHUB_OUTPUT"')
         and "LATEST: ${{ steps.latest.outputs.latest }}" in tagging
         and 'if [ "${LATEST}" = true ]; then\n            tags+=(latest)' in tagging
         and "LATEST: ${{ steps.latest.outputs.latest }}" in publish
         and 'elif [ "${LATEST}" = true ]; then\n            gh release edit "${TAG}" --draft=false --latest\n'
             '          else\n            gh release edit "${TAG}" --draft=false --latest=false\n' in publish
         and len(re.findall(r"tags\+=\(latest\)", text)) == 1 and len(re.findall(r"--latest\n", text)) == 1),
        ("publish: draft, assets checked, images tagged, then published and checked again",
         step_order("Create the draft release", "Check the draft's assets byte for byte", "Tag the verified image digests",
                    "Publish the release", "Check the published assets byte for byte")
         and pub_steps[-1] == "Check the published assets byte for byte"
         and line(st("publish", "Check the draft's assets byte for byte"), 'run: hack/release-assets.sh "${TAG}" "${RUNNER_TEMP}/dist"')
         and line(st("publish", "Check the published assets byte for byte"), 'run: hack/release-assets.sh "${TAG}" "${RUNNER_TEMP}/dist"')),
        ("publish: the release is created as a draft on the existing tag",
         "flags=(--verify-tag --draft " in st("publish", "Create the draft release")),
        ("publish: version tags before latest, each read back against the verified digest",
         in_order(tagging, 'tags=("${VERSION}")', "tags+=(latest)", "imagetools create",
                  'got="$(docker buildx imagetools inspect "${image}:${tag}"', 'if [ "${got}" != "${digest}" ]; then', "exit 1")),
        ("publish: a release candidate stays a pre-release",
         'if [ "${PRERELEASE}" = true ]; then\n            flags+=(--prerelease)' in st("publish", "Create the draft release")
         and 'if [ "${PRERELEASE}" = true ]; then\n            gh release edit "${TAG}" --draft=false --prerelease\n' in publish),
        ("publish: runs in the release environment, the only job that does",
         re.search(r"^    environment: release$", j.get("publish", ""), re.M) is not None
         and len(re.findall(r"^\s+environment:", text, re.M)) == 1),
        ("only publish writes releases", all(
            not re.search(r"gh release (create|edit|upload|delete)|--method DELETE", t)
            for n, t in j.items() if n != "publish")),
        ("no asset replacement, tag deletion or git tag write",
         not re.search(r"--clobber|--cleanup-tag|git push|git tag|gh release upload|gh release delete", text)),
        ("cosign is installed at a pinned release",
         text.count("sigstore/cosign-installer@") == text.count("cosign-release: v3.1.4") == 3),
    ]
    return out


def in_job(job, old, new):
    """A mutation that replaces old by new inside one job of the file."""
    def mutate(text):
        start = text.index(f"\n  {job}:\n")
        nxt = re.search(r"\n  [A-Za-z0-9_-]+:\n", text[start + 1:])
        end = start + 1 + nxt.start() if nxt else len(text)
        block = text[start:end]
        return text if old not in block else text[:start] + block.replace(old, new, 1) + text[end:]
    return mutate


def in_step(job, step, old, new):
    """A mutation that replaces old by new inside one step of one job."""
    def mutate(text):
        start = text.index(f"\n  {job}:\n")
        start = text.index(f"\n      - name: {step}\n", start)
        nxt = re.search(r"\n      - name: |\n  [A-Za-z0-9_-]+:\n", text[start + 1:])
        end = start + 1 + nxt.start() if nxt else len(text)
        block = text[start:end]
        return text if old not in block else text[:start] + block.replace(old, new, 1) + text[end:]
    return mutate


def drafts_first(text):
    """A mutation that moves the deletion of old drafts in front of the refusals of publish."""
    start = text.index("      - name: Remove the drafts of earlier runs\n")
    end = text.index("      - name: Create the draft release\n")
    block = text[start:end]
    text = text[:start] + text[end:]
    at = text.index("      - name: Refuse a moved tag or a published release\n")
    return text[:at] + block + text[at:]


VA, VI = ("verify", "Verify the archives"), ("verify", "Verify the images")
MUTATIONS = [
    ("permissions: {} at the top", "permissions: {}\n\n#", "permissions:\n  contents: read\n\n#"),
    ("runs only on a pushed v* tag", 'tags: ["v*"]\n', 'tags: ["v*"]\n  workflow_dispatch:\n'),
    ("one release at a time for every tag, never cancelled", "cancel-in-progress: false", "cancel-in-progress: true"),
    ("one release at a time for every tag, never cancelled", "  group: release\n", "  group: release-${{ github.ref }}\n"),
    ("exactly the eight jobs", "\n  verify:\n", "\n  verify-all:\n"),
    ("every checkout keeps no credentials", in_step("publish", "Clone the code", "persist-credentials: false", "persist-credentials: true")),
    ("every checkout keeps no credentials", in_step("scan-images", "Clone the code", "          persist-credentials: false\n", "")),
    ("no job or step is skipped by a condition or allowed to fail",
     "      - name: Require a passed ci-ok on main for the tagged commit\n",
     "      - name: Require a passed ci-ok on main for the tagged commit\n        if: ${{ false }}\n"),
    ("no job or step is skipped by a condition or allowed to fail",
     "    name: Verify everything\n", "    name: Verify everything\n    continue-on-error: true\n"),
    ("the version and the tag come from the check job and the pushed ref everywhere",
     "      VERSION: ${{ needs.check.outputs.version }}\n      PRERELEASE:", "      VERSION: latest\n      PRERELEASE:"),
    ("the version and the tag come from the check job and the pushed ref everywhere",
     in_job("publish", "      TAG: ${{ github.ref_name }}\n", "      TAG: v0.0.1\n")),
    ("images: both images for linux/amd64 and linux/arm64",
     "          target: agent\n          platforms: linux/amd64,linux/arm64", "          target: agent\n          platforms: linux/amd64"),
    ("check: version is X.Y.Z or X.Y.Z-rc.N", "(-rc\\.[1-9][0-9]*)?$ ]]", "(-.*)?$ ]]"),
    ("check: the tagged commit is on main", 'git merge-base --is-ancestor "${GITHUB_SHA}" origin/main', "true"),
    ("check: the tagged commit is on main", in_step("check", "Clone the code", "fetch-depth: 0", "fetch-depth: 1")),
    ("check: a published release or a failed lookup stops the run",
     'hack/release-state.sh "${TAG}" > /dev/null', 'hack/release-state.sh "${TAG}" > /dev/null || true'),
    ("check: a published release or a failed lookup stops the run",
     'hack/release-state.sh "${TAG}" > /dev/null',
     'if state="$(gh release view "${TAG}" --json isDraft --jq .isDraft 2> /dev/null)" && [ "${state}" = false ]; then exit 1; fi'),
    ("check: ci-ok passed on main for the tagged commit",
     'hack/release-ci-ok.sh "${GITHUB_SHA}" "${RUNNER_TEMP}/runs.json" "${RUNNER_TEMP}/suites.json"',
     'hack/release-ci-ok.sh "${GITHUB_SHA}" "${RUNNER_TEMP}/runs.json" "${RUNNER_TEMP}/suites.json" || echo "no ci-ok"'),
    ("check: ci-ok passed on main for the tagged commit", "check-runs?check_name=ci-ok&", "check-runs?"),
    ("binaries: the pinned package script, no setup-go cache, no target override", "cache: false #", "cache: true #"),
    ("binaries: SBOM of the binaries from the pinned syft", 'syft" scan "dir:${out}/bin"', 'syft" scan "dir:${out}/dist"'),
    ("images: both pushed by digest, with no tag", "push-by-digest=true,name-canonical=true,push=true\n\n  # A gate",
     "push=true\n          tags: ${{ env.AGENT_IMAGE }}:latest\n\n  # A gate"),
    ("scan-images: Trivy from the pinned installer", 'run: hack/install-trivy.sh "${RUNNER_TEMP}/trivy-bin"', "run: pip install trivy"),
    ("scan-images: every platform image of both images, by digest", 'for arch in amd64 arm64; do\n              child="$(docker buildx imagetools inspect --raw "${ref}" | jq -er --arg a "${arch}" \\\n                \'[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $a) | .digest] | if length == 1 then .[0] else error("not one \\($a) image") end\')"\n              "${RUNNER_TEMP}/trivy-bin/trivy"',
     'for arch in amd64; do\n              child="$(docker buildx imagetools inspect --raw "${ref}" | jq -er --arg a "${arch}" \\\n                \'[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $a) | .digest] | if length == 1 then .[0] else error("not one \\($a) image") end\')"\n              "${RUNNER_TEMP}/trivy-bin/trivy"'),
    ("scan-images: every platform image of both images, by digest",
     in_step("scan-images", "Scan every platform image by digest", "| if length == 1 then .[0] else error(\"not one \\($a) image\") end", "| .[0]")),
    ("scan-images: HIGH and CRITICAL fail unless .trivyignore.yaml accepts them", "--exit-code 1 --ignorefile", "--exit-code 0 --ignorefile"),
    ("scan-images: HIGH and CRITICAL fail unless .trivyignore.yaml accepts them", "--severity HIGH,CRITICAL", "--severity CRITICAL"),
    ("scan-images: HIGH and CRITICAL fail unless .trivyignore.yaml accepts them", "--scanners vuln \\\n", "--scanners vuln --ignore-unfixed \\\n"),
    ("sign-images: signs the digests, never a tag", 'cosign sign --yes "${ref}"', 'cosign sign --yes "${image}:latest"'),
    ("sign-images: an SPDX SBOM attestation per platform image, kept as a release file", "cosign attest --yes --type spdxjson", "echo"),
    ("sign-images: an SPDX SBOM attestation per platform image, kept as a release file",
     in_step("sign-images", "Sign the images and attest their SBOMs (keyless)", "| if length == 1 then .[0] else error(\"not one \\($a) image\") end", "| .[0]")),
    ("sign-images: provenance on both index digests, stored in the registry",
     "subject-digest: ${{ env.AGENT_DIGEST }}\n          push-to-registry: true",
     "subject-digest: ${{ env.AGENT_DIGEST }}\n          push-to-registry: false"),
    ("sign-images: both provenance bundles kept as .intoto.jsonl release files", "_image_sablecourt-agent.intoto.jsonl\"\n", "_image_sablecourt-agent.json\"\n"),
    ("sign-binaries: checksums.txt lists the archives and every SBOM", "sha256sum -- sablecourt_*.tar.gz sablecourt_*.spdx.json >",
     "sha256sum -- sablecourt_*.tar.gz sablecourt_*_sbom.spdx.json >"),
    ("sign-binaries: keyless signature bundle of checksums.txt", "cosign sign-blob --yes --bundle", "echo"),
    ("sign-binaries: provenance for the archives, the SBOMs and checksums.txt",
     "            ${{ runner.temp }}/dist/checksums.txt\n", ""),
    ("sign-binaries: the provenance bundle kept as a .intoto.jsonl release file",
     "BUNDLE: ${{ steps.provenance.outputs.bundle-path }}", "BUNDLE: ${{ runner.temp }}/empty.json"),
    ("verify: exactly the expected fourteen files", '"${expected}" ]; then', '"${expected}" ] && false; then'),
    ("verify: exactly the expected fourteen files", '"sablecourt_${VERSION}_image_"{sablecourt,sablecourt-agent}.intoto.jsonl | LC_ALL', "| LC_ALL"),
    ("verify: the checksums signature against this tag's exact identity", "sha256sum --check --strict checksums.txt", "true"),
    ("verify: the checksums signature against this tag's exact identity", in_step(*VA, "--certificate-identity", "--certificate-identity-regexp")),
    ("verify: checksums.txt lists exactly nine files",
     '          if [ "$(wc -l < "${DIST}/checksums.txt")" -ne 9 ]; then\n'
     '            echo "checksums.txt does not list the four archives and the five SBOMs" >&2\n            exit 1\n          fi\n', ""),
    ("verify: provenance of every file from the API and from the release bundle, bound to this tag and commit",
     'for file in "${DIST}"/sablecourt_*.tar.gz "${DIST}"/sablecourt_*.spdx.json "${DIST}/checksums.txt"; do',
     'for file in "${DIST}/checksums.txt"; do'),
    ("verify: provenance of every file from the API and from the release bundle, bound to this tag and commit",
     in_step(*VA, "              --deny-self-hosted-runners > /dev/null\n", "              > /dev/null\n")),
    ("verify: provenance of every file from the API and from the release bundle, bound to this tag and commit",
     in_step(*VA, '--source-digest "${GITHUB_SHA}"', '--source-digest "${GITHUB_SHA:0:7}"')),
    ("verify: provenance of every file from the API and from the release bundle, bound to this tag and commit",
     in_step(*VA, '              --bundle "${DIST}/sablecourt_${VERSION}_provenance.intoto.jsonl" \\\n', "")),
    ("verify: the stamped sablectl prints the version", '          "${RUNNER_TEMP}/sablectl" version | grep -F -- "${VERSION}"\n', ""),
    ("verify: image signatures and SBOM attestations against this tag's identity".replace("identity", "exact identity"),
     "cosign verify-attestation --type spdxjson \\\n                --certificate-identity \"${identity}\" \\\n"
     "                --certificate-oidc-issuer \"${issuer}\" \\\n",
     "cosign verify-attestation --type spdxjson \\\n                --certificate-identity \"${identity}\" \\\n"),
    ("verify: image signatures and SBOM attestations against this tag's exact identity",
     in_step(*VI, "| if length == 1 then .[0] else error(\"not one \\($a) image\") end", "| .[0]")),
    ("verify: image signatures and SBOM attestations against this tag's exact identity", "cosign verify-attestation --type spdxjson", "echo"),
    ("verify: every release SBOM of an image is a verified attestation's predicate", "any(. == $want[0])", "true"),
    ("verify: image provenance from the registry and from the release bundle, bound to this tag and commit",
     'gh attestation verify "oci://${ref}" \\\n              --repo', 'echo "oci://${ref}" \\\n              --repo'),
    ("verify: image provenance from the registry and from the release bundle, bound to this tag and commit",
     in_step(*VI, "              --deny-self-hosted-runners > /dev/null\n", "              > /dev/null\n")),
    ("verify: exact identities only, no regular expression or signer-workflow prefix",
     in_step(*VI, '              --cert-identity "${identity}" \\\n',
             '              --cert-identity "${identity}" \\\n              --signer-workflow "${REPOSITORY}/.github/workflows/release.yml" \\\n')),
    ("publish: refuses a moved tag and a published release before writing",
     'sha="$(gh api "repos/${GH_REPO}/commits/tags/${TAG}" --jq .sha)"', 'sha="${GITHUB_SHA}"'),
    ("publish: refuses a moved tag and a published release before writing",
     'hack/release-state.sh "${TAG}" > "${RUNNER_TEMP}/drafts"', 'hack/release-state.sh "${TAG}" > "${RUNNER_TEMP}/drafts" || true'),
    ("publish: refuses a moved tag and a published release before writing", drafts_first),
    ("publish: never moves a version image tag to another digest",
     'hack/release-image-tag.sh "${ref%@*}" "${VERSION}" "${ref#*@}"', 'echo "${ref%@*}" "${VERSION}" "${ref#*@}"'),
    ("publish: never moves a version image tag to another digest",
     'hack/release-image-tag.sh "${image}" "${tag}" "${digest}"', "true"),
    ("publish: latest only for the highest full release",
     'latest="$(hack/release-latest.sh "${VERSION}" < "${RUNNER_TEMP}/tags")"', 'latest=true'),
    ("publish: latest only for the highest full release",
     'if [ "${LATEST}" = true ]; then\n            tags+=(latest)', 'if [ "${PRERELEASE}" != true ]; then\n            tags+=(latest)'),
    ("publish: latest only for the highest full release",
     'gh release edit "${TAG}" --draft=false --latest=false', 'gh release edit "${TAG}" --draft=false --latest'),
    ("publish: draft, assets checked, images tagged, then published and checked again",
     '      - name: Check the draft\'s assets byte for byte\n        run: hack/release-assets.sh "${TAG}" "${RUNNER_TEMP}/dist"\n', ""),
    ("publish: the release is created as a draft on the existing tag", "flags=(--verify-tag --draft ", "flags=(--draft "),
    ("publish: version tags before latest, each read back against the verified digest",
     '"${got}" != "${digest}"', '"${got}" = ""'),
    ("publish: a release candidate stays a pre-release", "--draft=false --prerelease\n", "--draft=false\n"),
    ("publish: runs in the release environment, the only job that does", "    environment: release\n", ""),
    ("only publish writes releases", "      - name: Install syft (pinned)\n        run: hack/install-syft.sh \"${RUNNER_TEMP}/syft\"\n\n      - name: Write the SBOM",
     "      - name: Early release\n        run: gh release create x\n\n      - name: Write the SBOM"),
    ("no asset replacement, tag deletion or git tag write", '"${flags[@]}" "${DIST}"/*', '"${flags[@]}" --clobber "${DIST}"/*'),
    ("cosign is installed at a pinned release", "          cosign-release: v3.1.4\n\n      - name: Sign checksums",
     "\n      - name: Sign checksums"),
]
MUTATIONS = [(m[0].replace("against this tag's identity", "against this tag's exact identity"), *m[1:]) for m in MUTATIONS]

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
    for step in re.findall(r"uses: actions/checkout@.*\n(?:\s+(?!- ).*\n)*", read(".github/workflows/" + wf)):
        check(f"{wf}: a checkout without persisted credentials", "persist-credentials: false" in step)
verify_doc = read("docs/how-to/verify-a-release.md")
doc_code = "".join(re.findall(r"^```bash\n(.*?)^```$", verify_doc, re.M | re.S))
check("verify-a-release.md: the exact identity of release.yml on this tag, and a GitHub-hosted runner",
      'IDENTITY="https://github.com/shadewright-labs/sablecourt/.github/workflows/release.yml@refs/tags/v$VERSION"' in doc_code
      and doc_code.count('--certificate-identity "$IDENTITY"') == doc_code.count("cosign verify") == 3
      and doc_code.count('--cert-identity "$IDENTITY"') == doc_code.count("gh attestation verify") == 3
      and doc_code.count("--deny-self-hosted-runners") == 3
      and not re.search(r"identity-regexp|--signer-workflow|cert-identity-regex", verify_doc))

if fails:
    print(f"{fails} case(s) failed", file=sys.stderr)
    sys.exit(1)
print("all cases passed")
PY
