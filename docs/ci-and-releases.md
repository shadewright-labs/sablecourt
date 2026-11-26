# CI and releases

This page describes the workflows under `.github/workflows/` and how a release is cut. The layout follows the sister project [Shadewright](https://github.com/shadewright-labs/shadewright).

## Pull requests and pushes to main

`ci.yml` is the one entry point. Its `Changed files` job starts together with the jobs that always run (below), runs `hack/ci-changed.sh` on the files the pull request changes and sets four groups:

| Group | Runs when these change | Jobs |
| --- | --- | --- |
| go | Go code, `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `cmd/`, `internal/`, `examples/`, the root `Dockerfile`, `.dockerignore`, `Makefile`, `hack/`, `.golangci.yml`, `docs/reference/`, and the workflows that build Go outside `ci.yml` (`fuzz.yml`, `release.yml`, `images.yml`) | lint, test, govulncheck, gosec |
| images | the inputs of the root `Dockerfile` build (Go code, `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `cmd/`, `internal/`, `examples/`, the `Dockerfile`, `.dockerignore`), `LICENSE` and `images.yml` | image build (pull requests only) |
| docs | `docs/reference/` and what renders or checks it (`cmd/`, `internal/`, `Makefile`) | reference pages check |
| codeql | Go code, `go.mod`, `go.sum`, workflows, composite actions under `.github/actions/`, CodeQL configuration under `.github/codeql/` | CodeQL (pull requests only) |

A change to `ci.yml`, to a workflow it calls, to `hack/ci-changed.sh` or to `hack/ci-ok.sh` selects every group, and so does a push to main or a manual run. Selecting a group does not lift a job's own event condition: the image build and CodeQL still run only on pull requests. The script reads the diff as NUL-separated paths, so names with spaces, tabs, non-ASCII characters or a leading dash are classified like any other, and any error while computing or classifying the diff (a missing base commit, a failed `git diff`, a failed temporary file) selects every group. Its offline tests are `hack/test/ci-changed_test.sh`.

CI builds and tests with `GOWORK=off`, and so does every Dockerfile that runs `go` (`ENV GOWORK=off` in the build stage; `hack/check-dockerfiles.sh` fails a stage that runs `go` before it), so a `go.work` in the tree cannot make the image build differ from what CI checked.

Besides the root `Dockerfile` there are four nested Dockerfiles: `cmd/sablecourt-agent/Dockerfile`, `examples/demo-agent/Dockerfile`, and the council proxy and harness images under `internal/gate/councilproxy/` and `internal/harnessimages/`, which the daemon and `sablectl` build at run time. CI builds none of them; only the root `Dockerfile` is built. A change to one selects the groups of its directory, and Dependabot keeps their base images current.

Each group calls a workflow of its own:

- `lint.yml`: `go mod tidy -diff`, then golangci-lint through `make lint-config` and `make lint`. The configuration is `.golangci.yml`; the linters it leaves off are listed at the end of that file with the reason for each.
- `test.yml`: `go vet ./...`, `go test -race -count=1 ./...` (which also checks the generated reference pages), a 10 second fuzz run of the Model Gate SSRF guard, a build of `sablecourtd`, `sablectl`, `sablecourt-agent` and `examples/demo-agent`, and `sablectl validate` on `examples/local-demo.yaml`.
- `vulncheck.yml`: `make vulncheck` (govulncheck, version pinned in the Makefile). It also runs every Monday and can be started by hand.
- `gosec.yml`: gosec v2.30.0 with G104, G304 and G204 excluded, on the Linux build. The job fails when gosec reports a package it could not load or analyse, even if gosec itself exits 0.
- `images-build.yml`: builds the `daemon` and `agent` targets of the root `Dockerfile` for linux/amd64 without pushing, with the pinned Buildx and BuildKit of `hack/setup-buildx.sh`.
- `docs-check.yml`: `make docs-check`, which compares the reference pages under `docs/reference/` with the code.
- `codeql.yml`: CodeQL for Go and for the workflows. It also runs on pushes to main, every Monday and by hand.

Three jobs run on every pull request whatever changed:

- `repo-checks.yml`: `hack/docs-links.py check`, which reads each tracked Markdown file paragraph by paragraph and fails on a relative link to a missing file or heading, and on a reference link with no definition. It follows inline links (also over several lines, with a title in double quotes, single quotes or parentheses), angle-bracket links, full and collapsed reference links, and `href` and `src` of `<a>` and `<img>` in any quoting; headings count in both the `#` and the underlined (Setext) form. It does not follow shortcut reference links (`[text]` alone) or links in other raw HTML. Links to pages or headings not written yet are listed in `hack/docs-pages-pending.txt` as `<source> -> <target>[#fragment]`, one line per occurrence: two links from one page to the same pending target need two lines, so a second broken link cannot hide behind the first one's entry. The check fails when a target exists or a line is no longer used, so remove the lines in the pull request that adds the page. The same job runs `hack/check-image-refs.sh`, which fails when a tracked file names an image under `ghcr.io/shadewright-labs/` other than `sablecourt` and `sablecourt-agent`, a tag the Images workflow never pushes (such as `v0.1.0`, or an empty tag after a colon), or a digest that is not `sha256:` and 64 lower-case hex digits; it sees only references written out in full, not ones assembled in shell variables. The job also runs `hack/check-dockerfiles.sh` and the offline tests under `hack/test/`, including `workflows_test.sh`, which checks the structure of the publishing workflows described below.
- `workflow-lint.yml`: actionlint, shellcheck over `hack/*.sh` and `hack/test/*.sh`, and zizmor over `.github/workflows`. zizmor is installed with `pip --require-hashes` from `hack/zizmor-requirements.txt`.
- `secret-scan.yml`: gitleaks with the default rules plus `.gitleaks.toml`, over every ref and the full history, including the changes that only a merge commit introduces (`hack/secret-scan.sh`, which passes `--diff-merges=first-parent`; without it a token added while resolving a merge is never scanned). Each allowlist entry names one file and the exact test values it allows, anchored at both ends; for the private-key fixtures that is the whole match of gitleaks' rule (a PEM header and the test code after it, with no key body), so editing the lines around such a fixture makes the scan report it until the entry is updated. Before the scan, `hack/test/gitleaks_test.sh` checks that a token in an ordinary file, a different key block or token in each allowlisted file, and a token added only in a merge commit are all found. The scan is only as good as gitleaks' rules: its private-key rule matches lazily from a header to the next `KEY-----` at least 64 characters on, so a short PEM fixture header can swallow the header of a real key that follows it in the same file, and stock gitleaks then reports nothing.

The last job, `ci-ok`, needs all of them and runs `hack/ci-ok.sh` on their results. It passes only when `Changed files` succeeded and each of its four outputs is exactly `true` or `false`, every job that the groups and the event select succeeded, and every job that always runs succeeded. A job counts as legitimately skipped only when its group is `false` or the event excludes it. A job missing from the results, a selected job that was skipped, and any failure or cancellation fail `ci-ok`. It sees the result of each called workflow, not the steps inside it, so a check removed from a called workflow that still succeeds goes unnoticed there. Its tests are `hack/test/ci-ok_test.sh`, which also checks that the job list in the script matches the `needs` of `ci-ok`. A new pull request run cancels the older run of the same pull request; runs on main are never cancelled.

Two more workflows run on pull requests outside `ci.yml`. `dependency-review.yml` (job `Dependency review`) fails on a new or bumped dependency with a known vulnerability of moderate severity or higher, or with one of ten licences named by exact SPDX identifier: GPL-2.0, GPL-3.0, AGPL-3.0, LGPL-2.1 and LGPL-3.0, each in its `-only` and `-or-later` form. `pr-title.yml` (job `Conventional PR title`) checks the title against Conventional Commits: an allowed type, a scope from its list if there is one, and a summary of at least two characters that starts with a lower-case letter and does not end with a full stop. It runs on `pull_request_target`, so GitHub takes the workflow from the base branch, not from the pull request.

Every workflow except `pr-title.yml` starts with `permissions: {}` and grants each job only what it uses; `pr-title.yml` grants `pull-requests: read` at workflow level and never checks out code. Every workflow checks out with `persist-credentials: false` and pins every action to a full commit SHA. Executables that a job downloads itself (actionlint, gitleaks, Trivy, Syft, Buildx) are checked against a sha256 kept in the script that installs them, and the BuildKit image is pinned by digest.

### Required checks

What blocks a merge is whatever the branch ruleset for main requires; nothing in this repository can set or check that. The intended required checks are `ci-ok`, `Conventional PR title` and `Dependency review`. Until a repository admin changes the ruleset, the required checks stay `verify` and `security`, the jobs of the old `ci.yml`.

### Switching the required checks

The pull request that brings this layout cannot pass under the current ruleset: it removes the jobs `verify` and `security`, so it never reports them and the required checks never complete. The switch therefore goes in this order:

1. When the pull request is ready to merge, a repository admin changes the ruleset's required checks to `ci-ok` and `Dependency review`, at the moment of the merge, and merges.
2. `Conventional PR title` runs from the base branch (`pull_request_target`), and `pr-title.yml` does not exist on main before this change, so the check cannot report on this pull request. Add it to the required checks only after this change is on main.

## Scheduled runs

| Workflow | When | What |
| --- | --- | --- |
| `vulncheck.yml` | Mondays, by hand | govulncheck on main |
| `codeql.yml` | Mondays, pushes to main, by hand | CodeQL for Go and workflows |
| `scorecard.yml` | Mondays, pushes to main, branch protection changes, by hand | OpenSSF Scorecard, results to the Security tab |
| `fuzz.yml` | Tuesdays, by hand | every fuzz test that `go test -list '^Fuzz' ./...` finds in the module, for 4 minutes each, one job per target; a failing input is kept as an artifact |
| `docs-links.yml` | Wednesdays, by hand | reports external links that do not answer, as warnings; never fails on a dead link |
| `trivy.yml` | Mondays, v* tags, by hand | scans the released images and uploads the results to the Security tab |

`fuzz.yml` finds its targets with `hack/fuzz-targets.sh`, which asks the go command (`go test -list`), so a target with an unnamed parameter, a signature over several lines or an alias of `testing` is listed like any other. It sees the files that build on linux/amd64 with no extra build tags; the job fails when a package does not build or the list is empty. `trivy.yml` resolves each image tag to its index digest and scans the image for the runner's own platform (linux/amd64); findings are reported, not failed on. Until the first release exists its weekly run fails, because there is no image to resolve. Dependabot (`.github/dependabot.yml`) opens weekly updates for Go modules, GitHub Actions and the base images of the five Dockerfile directories. The base images of the root `Dockerfile` are pinned by tag and digest, and Dependabot updates both.

## Images

`images.yml` runs on every push to main and on every v* tag. It builds the `daemon` target as `ghcr.io/shadewright-labs/sablecourt` and the `agent` target as `ghcr.io/shadewright-labs/sablecourt-agent`, for linux/amd64 and linux/arm64. Its jobs:

1. `gate`: for a tag, the commit must be on main and pass `hack/check-release-tag.sh` (see below), and the release state is read (`hack/release-state.sh`). Only gh's own `release not found` counts as no release; any other error fails the run. If the release is already published, the run writes nothing and only `verify-published` runs.
2. `build`, one job per image: pushes the image index by digest only, with no tag, signs the index with cosign keyless signing (GitHub OIDC, no stored key), attests an SPDX SBOM for each platform image (Syft from `hack/install-syft.sh`) and records a GitHub build provenance attestation. Buildx and BuildKit come from `hack/setup-buildx.sh`.
3. `verify`: checks both indexes by digest (`hack/verify-image.sh`): the signature and each platform's SBOM against the identity of `images.yml` at exactly this ref, and the provenance, which must also name this ref and commit.
4. `promote`: one job for both images, after `verify`. It holds a concurrency group, one for tags and one for main, so one promotion runs at a time and a running one is never cancelled. It reads the release state and fetches the tags again, then applies the tags with `docker buildx imagetools create` and reads each one back (`hack/promote-images.sh`): first the run's own tags on both images, then the floating tags on both.
5. `verify-published`, only when the gate found the release published: resolves the version tags and verifies both images against this tag and commit, with no write permission. A rerun after publication ends green only if they verify.

Main gets the tags `main` and `sha-<commit>`. A release tag `v0.1.0` gets `0.1.0`; it also gets `0.1` when no stable tag with a higher patch exists in the 0.1 line, and `latest` when no newer stable tag exists at all (`hack/floating-tags.sh`, which compares version parts as strings of digits, so a part of any length compares correctly). The decision is made inside the promote job from the Git tags at that moment, and promotions run one at a time, so an older release promoted after a newer one has been tagged does not take `latest`. The decision is about Git tags: a newer stable tag whose images never got promoted still keeps `latest` on the older release. A pre-release tag `v0.2.0-rc.1` gets only `0.2.0-rc.1`. Image tags never carry the `v`.

The daemon and the agent are tagged by one job, but not atomically: if a write fails partway, the job fails, some tags may be on one image only, and re-running the job applies the same tags again. GitHub keeps at most one run waiting in a concurrency group; a third run replaces the waiting one, which ends cancelled without applying a tag and has to be re-run.

## Cutting a release

Releases come from release-please (`release-please.yml`, configuration in `release-please-config.json` and `.release-please-manifest.json`). On every push to main it keeps one release pull request, titled `chore: release X.Y.Z`, up to date from the Conventional Commits since the last release: `feat` raises the minor version, `fix` and `perf` the patch version, and a breaking change raises the minor version while the version is 0.x. The manifest starts empty, so the first release pull request proposes 0.1.0.

Merge the release pull request when the milestone of that version is done. Merging it updates `CHANGELOG.md` and `.release-please-manifest.json`, tags `vX.Y.Z` and creates a draft GitHub release. Never tag a plain `vX.Y.Z` by hand, and never edit `CHANGELOG.md` or `.release-please-manifest.json` outside the release pull request.

The tag starts `images.yml` and `release.yml`. `release.yml` then:

1. Checks the tag (the gate job): it must be on main and pass `hack/check-release-tag.sh`, and the release state is read as in the Images gate. When the release is already published, no job writes anything and only `verify-published` runs (below).
2. Builds `sablecourtd` and `sablectl` for darwin/arm64, darwin/amd64, linux/amd64 and linux/arm64 into `sablecourt_<version>_<os>_<arch>.tar.gz`, with `checksums.txt` over the four archives. `sablectl version` prints the version from the tag.
3. Signs `checksums.txt` keyless (`checksums.txt.sigstore.json`), records a build provenance attestation for each archive, and verifies both against `release.yml` at this tag and commit.
4. At the same time as steps 2 and 3, waits up to 30 minutes for the Images run to promote the version tags, then verifies, for both index digests, the signature, the SBOM of each platform image and the build provenance from this tag and this commit (`hack/resolve-image-digests.sh`). An image without all of them does not count.
5. Uploads the archives, `checksums.txt` and the signature bundle to the draft release (`hack/upload-release-assets.sh`). It reads the release before each attempt and stops at anything but a draft, so `--clobber` never replaces an asset of a published release.
6. Appends the image digests and the verify commands to the release notes, only while the release is a draft. `hack/release-verify-notes.sh` replaces its own earlier section on a rerun and refuses a body whose section markers are unpaired, repeated or inside a line of prose, so it cannot pair a stray marker with a new one and delete the text between them.
7. Publishes the draft only if its draft and pre-release states are booleans, the pre-release flag matches the tag (set for `-rc.N`, clear otherwise), the body contains the notes section for those digests, and its assets are exactly the six files steps 2 and 3 made, each with the same sha256 digest; an extra asset fails too (`hack/publish-release.sh`). It reads the draft again just before `gh release edit --draft=false` and publishes only if it is still a complete draft. Otherwise the job fails and the release stays a draft; fix the failed job and re-run it.

A rerun after publication runs `verify-published` instead: it downloads the release's assets, verifies the signature of `checksums.txt`, the checksums and each archive's provenance (this tag and commit), resolves and verifies both images, and requires the exact asset set and the notes section for those digests. It has no write permission, and the run is green only if all of it passes.

### What a release's state does and does not tell you

A published release went through the checks above at the moment it was published, or, on a later rerun, through `verify-published`. Publication alone does not prove that: a person can publish a draft by hand, and the gate cannot tell who did. That is why a published release is verified on every rerun rather than taken as complete. The publish step checks that the body contains the generated section, not that the section ends the body or that the rest of the body is unchanged. A draft hides only the GitHub release; it does not hide anything in the registry.

### What a failed run leaves behind

- Images: a failed build or verify job leaves image indexes in the registry that have a digest but no tag, possibly with signatures and attestations. They are pullable by digest; nothing points consumers at them. A failed promote job can leave the tags on one image and not the other; re-run it. An untagged index can be deleted from the package's settings on GitHub; leaving it costs storage only.
- Attestations: build provenance attestations of a failed run stay in the repository's attestation store and in the public Sigstore transparency log. They cannot be removed, and they name the commit they came from, so they do not verify for any other commit.
- The draft release: it can hold some or all of the assets, and the notes section. Re-running the failed jobs replaces the assets (`--clobber`) and the section. An asset that a person added by hand stops publication until it is deleted from the draft.
- A tag that should not have been made: delete the draft release and the tag, then delete the untagged image digests. Tag deletion is limited by the rulesets below.

After a failed verification no tag of that run exists on any image. A rerun of the whole Images run builds new digests; if the Release assets run has already resolved the old ones, re-run its images, release-notes and publish-release jobs as well, so the notes name the images that were promoted.

### Which tags are accepted

`hack/check-release-tag.sh` accepts two forms and refuses anything else (another pre-release form, build metadata, leading zeros). The manifest must be `{}` (no release yet, the one bootstrap case) or `{".": "X.Y.Z"}`; an empty file, `null`, another type, other keys or a malformed version are refused.

- `vX.Y.Z` only when X.Y.Z equals the version in `.release-please-manifest.json` at the tagged commit. Only the merged release pull request writes that version, but every later commit carries it too until the next release, so this alone does not prove that the tag is on the release commit.
- `vX.Y.Z-rc.N` only when X.Y.Z is newer than the manifest's version at the tagged commit and newer than the newest `vX.Y.Z` tag that exists when the check runs, so a candidate on an old commit cannot be for a release already made.

### Who may create release tags

The scripts are a second line of defence, not the trust anchor. A tag runs the workflows as they are at the tagged commit: tag an old commit on main whose `release.yml` predates these checks, and that old workflow runs, accepts any `v*` tag and moves `latest`. Nothing in the current branch can stop that. Only the repository's tag rulesets can, so a repository admin sets two:

1. Stable tags: target `refs/tags/v*`, excluding `refs/tags/v*-rc.*`. Restrict creation, update and deletion. The only bypass actor is the release GitHub App (below), which creates the tag when the release pull request merges.
2. Release candidate tags: target `refs/tags/v*-rc.*`. Restrict creation, update and deletion, with maintainers as bypass actors.

They are two rulesets because a bypass applies to everything its ruleset targets: a maintainer bypass on a single `v*` ruleset would let a maintainer create stable tags as well.

A release candidate must be cut from a commit that already contains the trusted workflows, that is this layout of `release.yml` and `images.yml`. Check it before tagging: `git merge-base --is-ancestor <first commit of this layout on main> <commit>` must succeed, and `git show <commit>:.github/workflows/images.yml` must show the `verify` and `promote` jobs.

Tests for the tag check, the floating tags, the release state, the upload, the notes, the image resolver, the promotion and the publish step are in `hack/test/release-scripts_test.sh`.

### Release candidates

A pre-release is the one tag made by hand, by a maintainer allowed by the candidate ruleset. Tag a commit on main that contains the trusted workflows (above) and create a draft pre-release for it:

```bash
git tag v0.1.0-rc.1 <commit on main>
git push origin v0.1.0-rc.1
gh release create v0.1.0-rc.1 --verify-tag --draft --prerelease
```

The tag push runs the same workflows as a release. The images get only the tag `0.1.0-rc.1`, and release-please is not affected, because it reads its last version from the manifest. A draft created without `--prerelease` is not published: the last job refuses it.

### Before the first release

These settings are made by a repository admin; no workflow changes them:

- A GitHub App installed on this repository with read and write access to contents, pull requests and issues. Its client ID goes in the variable `RELEASE_APP_CLIENT_ID` and its private key in the secret `RELEASE_APP_PRIVATE_KEY`, in an environment named `release` that only main can deploy to. Without them the release-please job fails and names what is missing.
- The two tag rulesets described above.
- The required checks described above, switched in the order given there.
- Code scanning default setup switched off (Settings, Code security), because `codeql.yml` uploads as an advanced setup and GitHub refuses both at once.
- The dependency graph switched on, which `Dependency review` reads.

The Homebrew formula in `deploy/homebrew/sablecourt.rb` is not maintained by the release yet: its checksums are placeholders, and no workflow updates it or publishes it to a tap.

### Verify a release

Every release body has a section that lists the image digests and the commands that check them. The cosign identities are anchored to the tag, so a signature made for another tag or branch does not match. In short, for the release `v0.1.0` (escape the dots of the tag in the identity patterns, and take the commit from the tag):

```bash
TAG=v0.1.0
TAG_RE="${TAG//./\\.}"
COMMIT="$(git rev-list -n 1 "$TAG")"
ISSUER=https://token.actions.githubusercontent.com
IMAGE=ghcr.io/shadewright-labs/sablecourt
DIGEST=sha256:...   # from the release body
cosign verify "${IMAGE}@${DIGEST}" \
  --certificate-identity-regexp "^https://github\.com/shadewright-labs/sablecourt/\.github/workflows/images\.yml@refs/tags/${TAG_RE}\$" \
  --certificate-oidc-issuer "$ISSUER"
gh attestation verify "oci://${IMAGE}@${DIGEST}" --repo shadewright-labs/sablecourt \
  --signer-workflow shadewright-labs/sablecourt/.github/workflows/images.yml \
  --source-ref "refs/tags/${TAG}" --source-digest "$COMMIT"
gh release download "$TAG" --repo shadewright-labs/sablecourt
cosign verify-blob --bundle checksums.txt.sigstore.json checksums.txt \
  --certificate-identity-regexp "^https://github\.com/shadewright-labs/sablecourt/\.github/workflows/release\.yml@refs/tags/${TAG_RE}\$" \
  --certificate-oidc-issuer "$ISSUER"
sha256sum --check --strict checksums.txt
```

The release body has the full set, including the SBOM of each platform image and the provenance of each archive. Use cosign 3.x or later: the signatures are stored as Sigstore bundles, which cosign 2.x does not find.
