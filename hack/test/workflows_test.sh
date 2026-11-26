#!/usr/bin/env bash
# Structural tests of the publishing workflows, for properties that a workflow edit could quietly lose:
#   - images.yml pushes by digest only (no tags input on the build), and only its promote job applies tags:
#     after the verify job, for both images in one job, in a concurrency group that never cancels a running
#     promotion; a published release gets a read-only verification job, not a green no-op.
#   - no job downloads an executable without a checksum kept in this repository: no setup-buildx-action, no
#     sbom-action; Buildx, BuildKit (by digest) and Syft come from hack/ scripts that check a sha256.
#   - release.yml binds the image provenance to the tagged commit and verifies a published release.
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


images = read(".github/workflows/images.yml")
ij = jobs(images)
build, verify, promote = ij.get("build", ""), ij.get("verify", ""), ij.get("promote", "")
check("images: build pushes by digest", "push-by-digest=true" in build and "push=true" in build)
check("images: build sets no tags", not re.search(r"^\s+tags:", build, re.M))
check("images: no job but promote creates tags",
      all("imagetools create" not in t and "promote-images.sh" not in t for n, t in ij.items() if n != "promote"))
check("images: promote applies the tags", "hack/promote-images.sh" in promote)
check("images: verify checks both images by digest", "hack/verify-image.sh" in verify and "needs: [gate, build]" in verify)
check("images: promote waits for verify", re.search(r"needs: \[[^\]]*\bverify\b[^\]]*\]", promote) is not None)
check("images: promote is one job for both images (no matrix)", "matrix" not in promote)
check("images: promote serializes without cancelling",
      re.search(r"concurrency:\s*\n\s+group: images-promote-\$\{\{ github\.repository \}\}-", promote) is not None
      and re.search(r"cancel-in-progress: false", promote) is not None)
check("images: promote reads the tags again", "git fetch --force --tags" in promote)
check("images: a published release is verified, not skipped",
      "verify-published" in ij and "resolve-image-digests.sh" in ij["verify-published"]
      and "write" not in ij["verify-published"])

for wf in sorted(os.listdir(os.path.join(root, ".github/workflows"))):
    text = read(".github/workflows/" + wf)
    check(f"{wf}: no setup-buildx-action", "docker/setup-buildx-action" not in text)
    check(f"{wf}: no sbom-action", "anchore/sbom-action" not in text)
for wf in ("images.yml", "images-build.yml"):
    check(f"{wf}: Buildx from hack/setup-buildx.sh", "hack/setup-buildx.sh" in read(".github/workflows/" + wf))
check("images: Syft from hack/install-syft.sh", "hack/install-syft.sh" in build)
setup = read("hack/setup-buildx.sh")
check("setup-buildx: BuildKit pinned by digest", re.search(r"^BUILDKIT_IMAGE=moby/buildkit:v[0-9.]+@sha256:[0-9a-f]{64}$", setup, re.M) is not None)
check("setup-buildx: Buildx pinned by sha256", re.search(r"^BUILDX_SHA256=[0-9a-f]{64}$", setup, re.M) is not None)
check("install-syft: pinned by sha256", re.search(r"^SYFT_SHA256=[0-9a-f]{64}$", read("hack/install-syft.sh"), re.M) is not None)
for script in sorted(os.listdir(os.path.join(root, "hack"))):
    if script.endswith(".sh"):
        text = read("hack/" + script)
        if re.search(r"^\s*curl ", text, re.M):
            check(f"{script}: every download is checked against a sha256",
                  text.count("sha256sum --check --strict") >= len(re.findall(r"^\s*curl ", text, re.M)))

release = read(".github/workflows/release.yml")
rj = jobs(release)
check("release: images resolved for the tagged commit", 'resolve-image-digests.sh "${TAG}" "${REPOSITORY}" "${OWNER}" "${GITHUB_SHA}"' in rj.get("images", ""))
vp = rj.get("verify-published", "")
check("release: a published release is verified", "publish-release.sh" in vp and "verify-only" in vp and "cosign verify-blob" in vp)
check("release: verify-published has no write permission", vp != "" and ": write" not in vp)
check("release: upload checks the draft before every attempt", "hack/upload-release-assets.sh" in rj.get("upload", ""))

if fails:
    print(f"{fails} case(s) failed", file=sys.stderr)
    sys.exit(1)
print("all cases passed")
PY
