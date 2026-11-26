#!/usr/bin/env bash
# Structural tests of the workflows, for properties that a workflow edit could quietly lose:
#   - no job downloads an executable without a checksum kept in this repository: no setup-buildx-action, no
#     sbom-action; Buildx and BuildKit (by digest) come from hack/setup-buildx.sh, which checks a sha256.
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

if fails:
    print(f"{fails} case(s) failed", file=sys.stderr)
    sys.exit(1)
print("all cases passed")
PY
