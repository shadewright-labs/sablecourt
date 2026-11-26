# CI and releases

This page describes the workflows under `.github/workflows/` and how a release is cut. The layout follows the sister project [Shadewright](https://github.com/shadewright-labs/shadewright).

## Pull requests and pushes to main

`ci.yml` is the one entry point. Its first job, `Changed files`, runs `hack/ci-changed.sh` on the files the pull request changes and sets four groups:

| Group | Runs when these change | Jobs |
| --- | --- | --- |
| go | Go code, `go.mod`, `go.sum`, `cmd/`, `internal/`, `examples/`, the Dockerfile, `Makefile`, `hack/`, `.golangci.yml`, `docs/reference/` | lint, test, govulncheck, gosec |
| images | the inputs of the Dockerfile build and `LICENSE` | image build (pull requests only) |
| docs | `docs/`, any `*.md`, `hack/docs-*`, `Makefile` | docs check |
| codeql | Go code, `go.mod`, `go.sum`, workflows | CodeQL (pull requests only) |

A change to `ci.yml`, to a workflow it calls or to the script runs every group, and so does a push to main or a manual run. If the script cannot compute the diff, it runs everything. Workflow lint and the secret scan run on every pull request whatever changed.

Each group calls a workflow of its own:

- `lint.yml`: `go mod tidy -diff`, then golangci-lint through `make lint-config` and `make lint`. The configuration is `.golangci.yml`; the linters it leaves off are listed at the end of that file with the reason for each.
- `test.yml`: `go vet ./...`, `go test -race -count=1 ./...` (which also checks the generated reference pages), a 10 second fuzz run of the Model Gate SSRF guard, a build of `sablecourtd`, `sablectl`, `sablecourt-agent` and `examples/demo-agent`, and `sablectl validate` on `examples/local-demo.yaml`.
- `vulncheck.yml`: `make vulncheck` (govulncheck, version pinned in the Makefile). It also runs every Monday.
- `gosec.yml`: gosec v2.30.0 with G104, G304 and G204 excluded. The job fails when gosec reports a package it could not load or analyse, even if gosec itself exits 0.
- `images-build.yml`: builds the `daemon` and `agent` targets of the Dockerfile for linux/amd64 without pushing.
- `docs-check.yml`: `make docs-check`, then `hack/docs-links.py check`, which fails on a relative link to a missing file or heading. Pages that the docs link to before they are written are listed in `hack/docs-pages-pending.txt`; the check fails when a listed page exists or nothing links to it any more, so remove the line in the pull request that adds the page.
- `codeql.yml`: CodeQL for Go and for the workflows. It also runs on pushes to main and every Monday.
- `workflow-lint.yml`: actionlint and zizmor over `.github/workflows`.
- `secret-scan.yml`: gitleaks over the full history with the allowlist in `.gitleaks.toml`.

The last job, `ci-ok`, needs all of them. It passes when every job succeeded or was skipped by the changed-files decision, and fails when `Changed files` failed or any job failed or was cancelled. A new pull request run cancels the older run of the same pull request; runs on main are never cancelled.

Two more workflows run on pull requests outside `ci.yml`: `dependency-review.yml` (job `Dependency review`) fails on a new or bumped dependency with a known vulnerability of moderate severity or higher, or with a GPL, AGPL or LGPL licence, and `pr-title.yml` (job `Conventional PR title`) checks that the title follows Conventional Commits with a lower-case summary and no full stop at the end.

The required checks for merging are `ci-ok`, `Conventional PR title` and `Dependency review`.

Every workflow starts with `permissions: {}` and grants each job only what it uses, checks out with `persist-credentials: false`, and pins every action to a full commit SHA.

## Scheduled runs

| Workflow | When | What |
| --- | --- | --- |
| `vulncheck.yml` | Mondays | govulncheck on main |
| `codeql.yml` | Mondays, pushes to main | CodeQL for Go and workflows |
| `scorecard.yml` | Mondays, pushes to main, branch protection changes | OpenSSF Scorecard, results to the Security tab |
| `fuzz.yml` | Tuesdays, by hand | every `Fuzz*` test for 4 minutes each, one job per target; a failing input is kept as an artifact |
| `docs-links.yml` | Wednesdays, by hand | reports external links that do not answer; never fails on a dead link |
| `trivy.yml` | Mondays, v* tags | scans the released images and uploads the results to the Security tab |

`fuzz.yml` finds its targets with `hack/fuzz-targets.sh`, which lists every `func FuzzX(f *testing.F)` under `cmd/`, `internal/` and `examples/`, so a new fuzz test needs no list entry. Dependabot (`.github/dependabot.yml`) opens weekly updates for Go modules, GitHub Actions and the base images of every Dockerfile.

## Images

`images.yml` runs on every push to main and on every v* tag. It builds the `daemon` target as `ghcr.io/shadewright-labs/sablecourt` and the `agent` target as `ghcr.io/shadewright-labs/sablecourt-agent`, for linux/amd64 and linux/arm64, and pushes them. Main gets the tags `main` and `sha-<commit>`. A release tag `v0.1.0` gets `0.1.0`, `0.1` and `latest`; a pre-release tag `v0.2.0-rc.1` gets only `0.2.0-rc.1`. On a tag the job first checks that the tagged commit is on main.

After the push the same job signs each image by digest with cosign keyless signing (GitHub OIDC, no stored key), attests an SPDX SBOM for each platform image, records a GitHub build provenance attestation, and verifies all three against the identity of `images.yml`. A mismatch fails the run.

## Cutting a release

Releases come from release-please (`release-please.yml`, configuration in `release-please-config.json` and `.release-please-manifest.json`). On every push to main it keeps one release pull request, titled `chore: release X.Y.Z`, up to date from the Conventional Commits since the last release: `feat` raises the minor version, `fix` and `perf` the patch version, and a breaking change raises the minor version while the version is 0.x. The manifest starts empty, so the first release pull request proposes 0.1.0.

Merge the release pull request when the milestone of that version is done. Merging it updates `CHANGELOG.md`, tags `vX.Y.Z` and creates a draft GitHub release. Never tag a plain `vX.Y.Z` by hand, and never edit `CHANGELOG.md` or `.release-please-manifest.json` outside the release pull request.

The tag starts `images.yml` and `release.yml`. `release.yml` then:

1. Checks that the tag is on main and builds `sablecourtd` and `sablectl` for darwin/arm64, darwin/amd64, linux/amd64 and linux/arm64 into `sablecourt_<version>_<os>_<arch>.tar.gz`, with `checksums.txt` over the four archives. `sablectl version` prints the version from the tag.
2. Signs `checksums.txt` keyless (`checksums.txt.sigstore.json`), records a build provenance attestation for each archive, and verifies both.
3. Waits up to 30 minutes for the images of the tag and verifies their signatures and SBOMs.
4. Uploads the archives, `checksums.txt` and the signature bundle to the draft release.
5. Appends the image digests and the verify commands to the release notes.
6. Publishes the draft only if those notes and all six assets are present. Otherwise the job fails and the release stays a draft; fix the failed job and re-run it.

### Release candidates

A pre-release is the one tag made by hand. Tag a commit that is on main and create a draft pre-release for it:

```bash
git tag v0.1.0-rc.1 <commit on main>
git push origin v0.1.0-rc.1
gh release create v0.1.0-rc.1 --verify-tag --draft --prerelease
```

The tag push runs the same workflows as a release. The images get only the tag `0.1.0-rc.1`, and release-please is not affected, because it reads its last version from the manifest.

### Before the first release

These settings are made by a repository admin; no workflow changes them:

- A GitHub App installed on this repository with read and write access to contents, pull requests and issues. Its client ID goes in the variable `RELEASE_APP_CLIENT_ID` and its private key in the secret `RELEASE_APP_PRIVATE_KEY`, in an environment named `release` that only main can deploy to. Without them the release-please job fails and names what is missing.
- Code scanning default setup switched off (Settings, Code security), because `codeql.yml` uploads as an advanced setup and GitHub refuses both at once.
- The dependency graph switched on, which `Dependency review` reads.

### Verify a release

Every release body ends with a section that lists the image digests and the commands that check them. In short, with `<tag>` the release tag:

```bash
ISSUER=https://token.actions.githubusercontent.com
cosign verify ghcr.io/shadewright-labs/sablecourt@<digest> \
  --certificate-identity-regexp '^https://github\.com/shadewright-labs/sablecourt/\.github/workflows/images\.yml@refs/tags/<tag>$' \
  --certificate-oidc-issuer "$ISSUER"
gh release download <tag> --repo shadewright-labs/sablecourt
cosign verify-blob --bundle checksums.txt.sigstore.json checksums.txt \
  --certificate-identity-regexp '^https://github\.com/shadewright-labs/sablecourt/\.github/workflows/release\.yml@refs/tags/<tag>$' \
  --certificate-oidc-issuer "$ISSUER"
sha256sum --check --strict checksums.txt
```

Use cosign 3.x or later: the signatures are stored as Sigstore bundles, which cosign 2.x does not find.
