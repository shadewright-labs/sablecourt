# CI and releases

This page describes the workflows under `.github/workflows/` and how a release is cut. The layout follows the sister project [Shadewright](https://github.com/shadewright-labs/shadewright).

## Pull requests and pushes to main

`ci.yml` is the one entry point. Its first job, `Changed files`, runs `hack/ci-changed.sh` on the files the pull request changes and sets four groups:

| Group | Runs when these change | Jobs |
| --- | --- | --- |
| go | Go code, `go.mod`, `go.sum`, `cmd/`, `internal/`, `examples/`, the root `Dockerfile`, `.dockerignore`, `Makefile`, `hack/`, `.golangci.yml`, `docs/reference/`, and the workflows that build Go outside `ci.yml` (`fuzz.yml`, `release.yml`, `images.yml`) | lint, test, govulncheck, gosec |
| images | the inputs of the root `Dockerfile` build (Go code, `go.mod`, `go.sum`, `cmd/`, `internal/`, `examples/`, the `Dockerfile`, `.dockerignore`), `LICENSE` and `images.yml` | image build (pull requests only) |
| docs | `docs/reference/` and what renders or checks it (`cmd/`, `internal/`, `Makefile`) | reference pages check |
| codeql | Go code, `go.mod`, `go.sum`, workflows, composite actions under `.github/actions/`, CodeQL configuration under `.github/codeql/` | CodeQL (pull requests only) |

A change to `ci.yml`, to a workflow it calls, to `hack/ci-changed.sh` or to `hack/ci-ok.sh` selects every group, and so does a push to main or a manual run. Selecting a group does not lift a job's own event condition: the image build and CodeQL still run only on pull requests. The script reads the diff as NUL-separated paths, so names with spaces, tabs, non-ASCII characters or a leading dash are classified like any other, and any error while computing or classifying the diff (a missing base commit, a failed `git diff`, a failed temporary file) selects every group. Its offline tests are `hack/test/ci-changed_test.sh`.

The nested Dockerfiles, `cmd/sablecourt-agent/Dockerfile` and `examples/demo-agent/Dockerfile`, are not built in CI: only the root `Dockerfile` is. A change to either selects the go group through its directory, and Dependabot keeps their base images current.

Each group calls a workflow of its own:

- `lint.yml`: `go mod tidy -diff`, then golangci-lint through `make lint-config` and `make lint`. The configuration is `.golangci.yml`; the linters it leaves off are listed at the end of that file with the reason for each.
- `test.yml`: `go vet ./...`, `go test -race -count=1 ./...` (which also checks the generated reference pages), a 10 second fuzz run of the Model Gate SSRF guard, a build of `sablecourtd`, `sablectl`, `sablecourt-agent` and `examples/demo-agent`, and `sablectl validate` on `examples/local-demo.yaml`.
- `vulncheck.yml`: `make vulncheck` (govulncheck, version pinned in the Makefile). It also runs every Monday and can be started by hand.
- `gosec.yml`: gosec v2.30.0 with G104, G304 and G204 excluded, on the Linux build. The job fails when gosec reports a package it could not load or analyse, even if gosec itself exits 0.
- `images-build.yml`: builds the `daemon` and `agent` targets of the root `Dockerfile` for linux/amd64 without pushing.
- `docs-check.yml`: `make docs-check`, which compares the reference pages under `docs/reference/` with the code.
- `codeql.yml`: CodeQL for Go and for the workflows. It also runs on pushes to main and every Monday.

Three jobs run on every pull request whatever changed, and start at the same time as `Changed files`:

- `repo-checks.yml`: `hack/docs-links.py check`, which fails on a relative link to a missing file or heading (inline, angle-bracket and reference links, and HTML `href` and `src`), and on a reference link with no definition. Links to pages or headings not written yet are listed in `hack/docs-pages-pending.txt` as `<source> -> <target>[#fragment]`; an entry allows only that link from that file, and the check fails when the target exists or the link is gone, so remove the line in the pull request that adds the page. The same job runs `hack/check-image-refs.sh`, which fails when a tracked file names an image under `ghcr.io/shadewright-labs/` other than `sablecourt` and `sablecourt-agent`, or a tag the Images workflow never pushes (such as `v0.1.0`), and the offline tests under `hack/test/`.
- `workflow-lint.yml`: actionlint, shellcheck over `hack/*.sh` and `hack/test/*.sh`, and zizmor over `.github/workflows`. zizmor is installed with `pip --require-hashes` from `hack/zizmor-requirements.txt`.
- `secret-scan.yml`: gitleaks over the full history with the default rules plus `.gitleaks.toml`. Each allowlist entry there names one file and the exact test value (or the shape of a test key) it allows, never a whole directory. Before the scan, `hack/test/gitleaks_test.sh` checks that a new token in an ordinary file, a new token in an allowlisted file and a private key are still found.

The last job, `ci-ok`, needs all of them and runs `hack/ci-ok.sh` on the results. It passes only when `Changed files` succeeded and each of its four outputs is exactly `true` or `false`, every job that the groups and the event select succeeded, and every job that always runs succeeded. A job counts as legitimately skipped only when its group is `false` or the event excludes it. A job missing from the results, a selected job that was skipped, and any failure or cancellation fail `ci-ok`. Its tests are `hack/test/ci-ok_test.sh`, which also checks that the job list in the script matches the `needs` of `ci-ok`. A new pull request run cancels the older run of the same pull request; runs on main are never cancelled.

Two more workflows run on pull requests outside `ci.yml`. `dependency-review.yml` (job `Dependency review`) fails on a new or bumped dependency with a known vulnerability of moderate severity or higher, or with one of ten licences named by exact SPDX identifier: GPL-2.0, GPL-3.0, AGPL-3.0, LGPL-2.1 and LGPL-3.0, each in its `-only` and `-or-later` form. `pr-title.yml` (job `Conventional PR title`) checks the title against Conventional Commits: an allowed type, a scope from its list if there is one, and a summary of at least two characters that starts with a lower-case letter and does not end with a full stop.

Every workflow except `pr-title.yml` starts with `permissions: {}` and grants each job only what it uses; `pr-title.yml` grants `pull-requests: read` at workflow level and never checks out code. Every workflow checks out with `persist-credentials: false` and pins every action to a full commit SHA.

### Required checks

The checks that must pass before merging are `ci-ok`, `Conventional PR title` and `Dependency review`. A repository admin sets them in the branch ruleset for main, and replaces the current required checks `verify` and `security` with them in the same change: `ci-ok` covers everything those two ran. Until then the old names stay required and the new workflows only report.

## Scheduled runs

| Workflow | When | What |
| --- | --- | --- |
| `vulncheck.yml` | Mondays, by hand | govulncheck on main |
| `codeql.yml` | Mondays, pushes to main | CodeQL for Go and workflows |
| `scorecard.yml` | Mondays, pushes to main, branch protection changes | OpenSSF Scorecard, results to the Security tab |
| `fuzz.yml` | Tuesdays, by hand | every fuzz test for 4 minutes each, one job per target; a failing input is kept as an artifact |
| `docs-links.yml` | Wednesdays, by hand | reports external links that do not answer, as warnings; never fails on a dead link |
| `trivy.yml` | Mondays, v* tags | scans the released images and uploads the results to the Security tab |

`fuzz.yml` finds its targets with `hack/fuzz-targets.sh`, which lists every top-level `func FuzzX(<name> *testing.F)` in a `_test.go` file under `cmd/`, `internal/` and `examples/`, whatever the parameter is called, so a new fuzz test needs no list entry; the job fails when the list is empty or the search fails. `trivy.yml` resolves each image tag to its index digest and scans the image for the runner's own platform (linux/amd64); findings are reported, not failed on. Until the first release exists its weekly run fails, because there is no image to resolve. Dependabot (`.github/dependabot.yml`) opens weekly updates for Go modules, GitHub Actions and the base images of the five Dockerfile directories. The base images of the root `Dockerfile` are pinned by tag and digest, and Dependabot updates both.

## Images

`images.yml` runs on every push to main and on every v* tag. It builds the `daemon` target as `ghcr.io/shadewright-labs/sablecourt` and the `agent` target as `ghcr.io/shadewright-labs/sablecourt-agent`, for linux/amd64 and linux/arm64, and pushes them. Main gets the tags `main` and `sha-<commit>`. A release tag `v0.1.0` gets `0.1.0`; it also gets `0.1` when no stable tag with a higher patch exists in the 0.1 line, and `latest` when no newer stable tag exists at all (`hack/floating-tags.sh`), so a patch release of an older line never moves `latest` back. A pre-release tag `v0.2.0-rc.1` gets only `0.2.0-rc.1`. Image tags never carry the `v`.

Before anything is pushed, a gate job checks a tag: it must point at a commit on main, it must pass `hack/check-release-tag.sh` (see below), and if the GitHub release of the tag is already published nothing is pushed, so a rerun cannot replace published images.

After the push the same job signs each image by digest with cosign keyless signing (GitHub OIDC, no stored key), attests an SPDX SBOM for each platform image (Syft pinned through the action's `syft-version` input), records a GitHub build provenance attestation, and verifies all three against the identity of `images.yml` at exactly the ref and commit of the run. A mismatch fails the run.

## Cutting a release

Releases come from release-please (`release-please.yml`, configuration in `release-please-config.json` and `.release-please-manifest.json`). On every push to main it keeps one release pull request, titled `chore: release X.Y.Z`, up to date from the Conventional Commits since the last release: `feat` raises the minor version, `fix` and `perf` the patch version, and a breaking change raises the minor version while the version is 0.x. The manifest starts empty, so the first release pull request proposes 0.1.0.

Merge the release pull request when the milestone of that version is done. Merging it updates `CHANGELOG.md` and `.release-please-manifest.json`, tags `vX.Y.Z` and creates a draft GitHub release. Never tag a plain `vX.Y.Z` by hand, and never edit `CHANGELOG.md` or `.release-please-manifest.json` outside the release pull request.

The tag starts `images.yml` and `release.yml`. `release.yml` then:

1. Checks the tag (the gate job): it must be on main and pass `hack/check-release-tag.sh`, and when the release is already published the run stops there, so a full rerun after publication ends green and changes nothing.
2. Builds `sablecourtd` and `sablectl` for darwin/arm64, darwin/amd64, linux/amd64 and linux/arm64 into `sablecourt_<version>_<os>_<arch>.tar.gz`, with `checksums.txt` over the four archives. `sablectl version` prints the version from the tag.
3. Signs `checksums.txt` keyless (`checksums.txt.sigstore.json`), records a build provenance attestation for each archive, and verifies both against `release.yml` at this tag and commit.
4. At the same time as steps 2 and 3, waits up to 30 minutes for the images of the tag and verifies, for both index digests, the signature, the SBOM of each platform image and the build provenance (`hack/resolve-image-digests.sh`). An image without all three does not count.
5. Uploads the archives, `checksums.txt` and the signature bundle to the draft release.
6. Appends the image digests and the verify commands to the release notes. The notes claim only the checks step 4 made.
7. Publishes the draft only if its draft and pre-release states are booleans, the pre-release flag matches the tag (set for `-rc.N`, clear otherwise), those notes are present, and all six assets are uploaded with the sha256 digests of the files steps 2 and 3 made (`hack/publish-release.sh`). Otherwise the job fails and the release stays a draft; fix the failed job and re-run it.

The draft only keeps the GitHub release hidden. After a failed run, the registry can already hold the images, their tags and signatures, and the draft can hold some of the assets.

### Which tags are accepted

`hack/check-release-tag.sh` accepts two forms and refuses anything else (another pre-release form, build metadata, leading zeros):

- `vX.Y.Z` only when X.Y.Z equals the version in `.release-please-manifest.json` at the tagged commit. Only the merged release pull request writes that version, so a stable tag on any other commit is refused.
- `vX.Y.Z-rc.N` only when X.Y.Z is newer than the manifest's version (or the manifest has no version yet), so a candidate is always for a release not yet made.

The workflows cannot stop someone who may push tags from tagging other code. A repository admin limits that with a tag ruleset on `refs/tags/v*`: creation, update and deletion restricted, with the release GitHub App (below) as the only bypass actor for stable tags. A release candidate tag is pushed by a maintainer with that ruleset's bypass, by hand, as described next. Tests for the tag check, the floating tags, the release state and the publish step are in `hack/test/release-scripts_test.sh`.

### Release candidates

A pre-release is the one tag made by hand. Tag a commit that is on main and create a draft pre-release for it:

```bash
git tag v0.1.0-rc.1 <commit on main>
git push origin v0.1.0-rc.1
gh release create v0.1.0-rc.1 --verify-tag --draft --prerelease
```

The tag push runs the same workflows as a release. The images get only the tag `0.1.0-rc.1`, and release-please is not affected, because it reads its last version from the manifest. A draft created without `--prerelease` is not published: the last job refuses it.

### Before the first release

These settings are made by a repository admin; no workflow changes them:

- A GitHub App installed on this repository with read and write access to contents, pull requests and issues. Its client ID goes in the variable `RELEASE_APP_CLIENT_ID` and its private key in the secret `RELEASE_APP_PRIVATE_KEY`, in an environment named `release` that only main can deploy to. Without them the release-please job fails and names what is missing.
- The tag ruleset on `refs/tags/v*` described above.
- The required checks described above.
- Code scanning default setup switched off (Settings, Code security), because `codeql.yml` uploads as an advanced setup and GitHub refuses both at once.
- The dependency graph switched on, which `Dependency review` reads.

The Homebrew formula in `deploy/homebrew/sablecourt.rb` is not maintained by the release yet: its checksums are placeholders, and no workflow updates it or publishes it to a tap.

### Verify a release

Every release body ends with a section that lists the image digests and the commands that check them. In short, for the release `v0.1.0` (escape the dots of the tag in the identity patterns):

```bash
TAG=v0.1.0
TAG_RE="${TAG//./\\.}"
ISSUER=https://token.actions.githubusercontent.com
cosign verify ghcr.io/shadewright-labs/sablecourt@<digest> \
  --certificate-identity-regexp "^https://github\.com/shadewright-labs/sablecourt/\.github/workflows/images\.yml@refs/tags/${TAG_RE}\$" \
  --certificate-oidc-issuer "$ISSUER"
gh attestation verify oci://ghcr.io/shadewright-labs/sablecourt@<digest> --repo shadewright-labs/sablecourt \
  --signer-workflow shadewright-labs/sablecourt/.github/workflows/images.yml --source-ref "refs/tags/${TAG}"
gh release download "$TAG" --repo shadewright-labs/sablecourt
cosign verify-blob --bundle checksums.txt.sigstore.json checksums.txt \
  --certificate-identity-regexp "^https://github\.com/shadewright-labs/sablecourt/\.github/workflows/release\.yml@refs/tags/${TAG_RE}\$" \
  --certificate-oidc-issuer "$ISSUER"
sha256sum --check --strict checksums.txt
```

The release body has the full set, including the SBOM of each platform image and the provenance of each archive. Use cosign 3.x or later: the signatures are stored as Sigstore bundles, which cosign 2.x does not find.
