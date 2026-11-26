# CI

This page describes the checks under `.github/workflows/` that run on pull requests, on pushes to main and on a schedule. The layout follows the sister project [Shadewright](https://github.com/shadewright-labs/shadewright).

## Pull requests and pushes to main

`ci.yml` runs on pull requests, pushes to main and by hand, and calls the workflows below. Two pull request workflows have triggers of their own (see the end of this section), and CodeQL and Scorecard also run on their own triggers (see [Scheduled runs](#scheduled-runs)). The `Changed files` job of `ci.yml` has no dependencies, like the three jobs that always run. It runs `hack/ci-changed.sh` on the files the pull request changes and sets four groups:

| Group | Runs when these change | Jobs |
| --- | --- | --- |
| go | Go code, `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `vendor/`, `cmd/`, `internal/`, `examples/`, the root `Dockerfile`, `.dockerignore`, `Dockerfile.dockerignore`, `Makefile`, `GNUmakefile`, `makefile`, `hack/`, `.golangci.yml`, `docs/reference/`, and the workflows that build Go outside `ci.yml` (`fuzz.yml`, `release.yml`) | lint, test, govulncheck, gosec |
| images | Go code, `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `vendor/`, `cmd/`, `internal/`, `examples/`, the root `Dockerfile`, `.dockerignore`, `Dockerfile.dockerignore`, `LICENSE` and `hack/setup-buildx.sh`, which the build job runs; `hack/test/ci-changed_test.sh` tests these paths, including the scripts that `images-build.yml` runs and the named `COPY` sources of the `Dockerfile` | image build (pull requests only) |
| docs | `docs/`, `zensical.toml`, `requirements-docs.in`, `requirements-docs.txt`, `overrides/`, `hack/docs-site-check.py`, `hack/docs-pages-pending.txt`, `cmd/`, `internal/`, `Makefile`, `GNUmakefile`, `makefile` | reference pages check and docs site build |
| codeql | Go code, `go.mod`, `go.sum`, workflows, composite actions under `.github/actions/`, CodeQL configuration under `.github/codeql/` | CodeQL (pull requests only) |

A change to `ci.yml`, to a workflow it calls, to `hack/ci-changed.sh` or to `hack/ci-ok.sh` selects every group, and so does a push to main or a manual run. Selecting a group does not lift a job's own event condition: in `ci.yml` the image build and CodeQL run only on pull requests. The script reads the diff as NUL-separated paths, so names with spaces, tabs, non-ASCII characters or a leading dash are classified like any other. The script tries to fetch a missing base commit, and selects every group when the diff still fails or it cannot create its temporary file. Its offline tests are `hack/test/ci-changed_test.sh`.

The Go jobs that `ci.yml` calls set `GOWORK=off`. The root `Dockerfile` is the only Dockerfile that runs `go`, and it sets `ENV GOWORK=off` in its build stage. `hack/check-dockerfiles.sh` reads each Dockerfile line by line and fails a stage in which a `RUN` line calls `go` before the `ENV GOWORK=off` line. It does not join lines continued with a backslash, so a `go` call on a continuation line is not seen.

Besides the root `Dockerfile` there are two nested Dockerfiles: `internal/gate/councilproxy/Dockerfile` and `internal/harnessimages/Dockerfile`. The second one is embedded in the Go package `internal/harnessimages`, and `sablectl harness build <role>` builds the harness images from it. CI builds only the root `Dockerfile`. A change to a nested one selects the groups of its directory, and Dependabot is configured to check their base images weekly.

The groups call these workflows:

- `lint.yml` (go): `go mod tidy -diff`, then golangci-lint through `make -f Makefile lint-config` and `make -f Makefile lint`. The configuration is `.golangci.yml`, which enables a chosen set of linters; a comment at its end gives the reason for leaving out several others.
- `test.yml` (go): `go vet ./...`, `go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...` (which also runs the tests of the generated reference pages), a 10 second fuzz run of the Model Gate SSRF guard, a build of `sablecourtd`, `sablectl`, `sablecourt-agent` and `examples/demo-agent`, and `sablectl validate` on `examples/local-demo.yaml`. The coverage profile goes up as an artifact. A second job, `coverage upload`, runs only after the test job succeeded and sends the profile to Codecov with the pinned `codecov/codecov-action` and uploader version. `ci.yml` passes it only the secret `CODECOV_TOKEN`. When the token is empty, the job prints a notice and passes without uploading. The upload step sets `fail_ci_if_error: false`, so an upload error that the action handles does not fail the job; a failure of the checkout, the artifact download or the action itself does. `codecov.yml` makes the project and patch statuses informational, so the coverage figures do not block a merge. `ci-ok` sees one result for `test.yml`: if the tests fail, the upload is skipped and `test` fails; if the upload job fails, `test` fails too.
- `vulncheck.yml` (go): `make -f Makefile vulncheck` (govulncheck, version pinned in the Makefile). It also runs every Monday and can be started by hand.
- `gosec.yml` (go): gosec v2.30.0 with G104, G304 and G204 excluded, on the Linux build. The job also fails when gosec's output matches one of its load or analysis error patterns, or reports zero files, even if gosec exits 0.
- `images-build.yml` (images): builds the `daemon` and `agent` targets of the root `Dockerfile` for linux/amd64 without pushing, with the pinned Buildx and BuildKit of `hack/setup-buildx.sh`.
- `docs-check.yml` (docs): sets up Go and Python, installs `requirements-docs.txt` with `pip install --require-hashes` into `.venv` (`make docs-venv`), and runs `make -f Makefile docs-check`. That runs the reference page tests under `docs/reference/`, builds the Zensical site with `--clean --strict`, and runs `hack/docs-site-check.py` over the built `site/` and `docs/` (checking links, anchors, images, assets and rejecting mermaid code fences).
- `codeql.yml` (codeql): CodeQL for Go and for the workflows. It also runs on pushes to main, every Monday and by hand.

Three jobs of `ci.yml` run on every pull request, whatever changed:

- `repo-checks.yml`: `python3 -I hack/docs-links.py check`, which reads the tracked Markdown files, reports full and collapsed reference links with no definition, checks that linked local files exist and checks heading fragments in Markdown files. It recognises inline links, angle-bracket links, full and collapsed reference links, the `href` and `src` of `<a>` and `<img>`, and headings in both the `#` and the underlined (Setext) form. It skips fenced code blocks and code spans, but it reads a fence inside a block quote as ordinary text. It does not follow shortcut reference links (`[text]` alone). Links to pages or headings not written yet are listed in `hack/docs-pages-pending.txt` as `<source> -> <target>[#fragment]`. Each line covers one link, and each use of a reference link counts as one link, so two links from one page to the same pending target need two lines. The check fails when a target exists or a line is no longer used, so remove the lines in the pull request that adds the page. The same job runs `hack/check-image-refs.sh`, which searches tracked text for the full prefix `ghcr.io/shadewright-labs/` and leaves out its own script and test. It fails on an image name other than `sablecourt` and `sablecourt-agent`, on a tag other than `X.Y.Z`, `X.Y.Z-rc.N` and `latest` (such as `v0.1.0` or an empty tag), and on a digest, alone or after a tag, that is not `sha256:` and 64 lower-case hex digits. `release.yml` accepts the same two version forms. The job also runs `hack/check-dockerfiles.sh` and fifteen of the offline suites under `hack/test/` (the sixteenth, `gitleaks_test.sh`, runs in `secret-scan.yml`). One of them, `workflows_test.sh`, searches the workflow text for the two action names `docker/setup-buildx-action` and `anchore/sbom-action`, in each shell script under `hack/` compares the number of checksum commands with the number of lines that start with `curl`, checks that every action is pinned by commit with its version in a comment, and checks the structure of `release.yml` (see [Releases](#releases)).
- `workflow-lint.yml`: actionlint, shellcheck over `hack/*.sh` and `hack/test/*.sh`, and zizmor over `.github/workflows`. zizmor is installed with `pip --require-hashes` from `hack/zizmor-requirements.txt`.
- `secret-scan.yml`: gitleaks with the default rules plus `.gitleaks.toml`, over every ref and the full history of the checkout (`fetch-depth: 0`), including the changes that only a merge commit introduces (`hack/secret-scan.sh` passes `--diff-merges=first-parent`).

gitleaks ignores the exit status of the `git log` it runs, so `hack/secret-scan.sh` adds its own checks. It reads the same range first and fails when that `git log` or `git rev-list` fails, or when the log does not list exactly the commits `git rev-list --count` counts. It then runs gitleaks with a small `git` wrapper first in `PATH`, which records the exit status of each `git log` that gitleaks starts through `PATH`; the scan fails when one of them exits non-zero or when none was recorded. Last, it fails when gitleaks reports fewer scanned commits than the history has commits with a text change in a file that is not deleted. Known limit: a `git log` that stops part way through a commit and still exits 0 is not detected, because gitleaks counts commits and not bytes.

The test fixtures that this change edited are joined from split strings, so stock gitleaks finds nothing in them. The history still holds their earlier literals. Sixteen of the 17 entries of `.gitleaks.toml` each allow one exact value, anchored at both ends, in one file and only in the listed commits, so the same value in a new commit is reported. The 17th entry, for `internal/gate/mcp/upstream_test.go`, lists no commits: that file still holds its fake token as a literal, because this change leaves `internal/gate` as it is. It allows that one token in that one file in any commit, and no other value. Before the scan, `hack/test/gitleaks_test.sh` runs the same script on throwaway repositories. Its cases are: a token in an ordinary file, a key block or token added before or after the current fixtures, new content at the three paths only the history holds, a commit-scoped value in a new commit, and tokens added only in a merge commit. It also checks that each entry has one value and, except the one above, names commits, and that four kinds of broken history read fail the scan (a failed `git log` of the script, a failed or empty `git log` of gitleaks, and one cut short inside the last commit). A commit-scoped fixture value that comes back at its original path in a new commit is reported, so a branch made before this change that adds one again needs a rebase on main. The exact token of the unscoped entry stays allowed at its path.

The last job, `ci-ok`, needs all of them and runs `hack/ci-ok.sh` on their results. It passes only when `Changed files` succeeded and each of its four outputs is exactly `true` or `false`, every job that the groups and the event select succeeded, and every job that always runs succeeded. A job counts as legitimately skipped only when its group is `false` or the event excludes it. A job missing from the results, a selected job that was skipped, and any failure or cancellation fail `ci-ok`. It sees the result of each called workflow, not the steps inside it, so a check removed from a called workflow that still succeeds goes unnoticed there. Its tests are `hack/test/ci-ok_test.sh`, which also checks that the job list in the script matches the `needs` of `ci-ok`. A new run for a pull request cancels the run in progress for the same pull request. Runs on main are grouped by commit and do not cancel a run in progress.

Two more workflows run on pull requests outside `ci.yml`. `dependency-review.yml` (job `Dependency review`) is configured to fail on a new or bumped dependency with a known vulnerability of moderate severity or higher, or with one of ten licences named by exact SPDX identifier: GPL-2.0, GPL-3.0, AGPL-3.0, LGPL-2.1 and LGPL-3.0, each in its `-only` and `-or-later` form. `pr-title.yml` (job `Conventional PR title`) checks the title against Conventional Commits: an allowed type, a scope from its list if there is one, and a summary of at least two characters that starts with a lower-case letter and does not end with a full stop. It runs on `pull_request_target`, so GitHub takes the workflow from the base branch, not from the pull request.

Every workflow except `pr-title.yml` starts with `permissions: {}` and grants each job its own permissions; `pr-title.yml` grants `pull-requests: read` at workflow level and has no checkout step. Every checkout step sets `persist-credentials: false`, and every action from another repository is pinned to a full commit SHA. The executables that a job downloads itself (actionlint, gitleaks, Buildx, syft, Trivy) are checked against a sha256 kept in the file that installs them, and the BuildKit image is pinned by digest. Actions and tools may download further components of their own.

### Required checks

The checks that block a merge are a repository setting: the branch ruleset for main. No file in this repository sets or checks it, so this section describes a plan, not something the workflows enforce. The intended required checks are `ci-ok`, `Conventional PR title` and `Dependency review`. The old `ci.yml` had the jobs `verify` and `security`; while the ruleset still requires them, a pull request on this layout cannot satisfy it.

### Switching the required checks

The pull request that brings this layout removes the jobs `verify` and `security`, so it never reports them. If the ruleset still requires them when it is ready, the switch goes in this order:

1. When the pull request is ready to merge, a repository admin changes the ruleset's required checks to `ci-ok` and `Dependency review`, and merges.
2. `Conventional PR title` runs from the base branch (`pull_request_target`), and `pr-title.yml` does not exist on main before this change, so the check cannot report on this pull request. Add it to the required checks only after this change is on main.

## Scheduled runs

| Workflow | When | What |
| --- | --- | --- |
| `vulncheck.yml` | Mondays, by hand | govulncheck |
| `codeql.yml` | Mondays, pushes to main, by hand | CodeQL for Go and workflows |
| `scorecard.yml` | Mondays, pushes to main, branch protection changes, by hand | OpenSSF Scorecard, results to the Security tab |
| `fuzz.yml` | Tuesdays, by hand | one job per fuzz target that `hack/fuzz-targets.sh` lists, each asking for 4 minutes of fuzzing; on failure or cancellation, later steps try to collect and upload new corpus files that Git does not ignore |
| `docs-links.yml` | Wednesdays, by hand | reports external links that do not answer, as warnings; a dead link does not fail the job |

`hack/fuzz-targets.sh` takes its list from `go test -list '^Fuzz' ./...`, so a target with an unnamed parameter, a signature over several lines or an alias of `testing` is listed like any other. It keeps the names made of ASCII letters, digits and underscores; a target with another letter in its name is not run. It sees the files that build on linux/amd64 with no extra build tags, and it fails when `go test` fails or no name is left. `hack/fuzz-failures.sh` copies the untracked files that Git does not ignore under `testdata/fuzz/` at the module root and in nested packages, keeping their paths. Dependabot (`.github/dependabot.yml`) is configured to check Go modules, GitHub Actions and the base images of the three Dockerfile directories weekly. The base images of the root `Dockerfile` are pinned by tag and digest.

## Docs site

Two workflows handle the static documentation site built with Zensical from `zensical.toml`:

- `docs-check.yml` runs inside `ci.yml` when the `docs` group of `hack/ci-changed.sh` is selected (any change under `docs/`, `overrides/`, `cmd/` or `internal/`, or to `zensical.toml`, `requirements-docs.in`, `requirements-docs.txt`, `hack/docs-site-check.py`, `hack/docs-pages-pending.txt` or a Makefile, as well as pushes to `main` and manual `ci.yml` runs). It installs the hash-locked packages from `requirements-docs.txt` into `.venv` and runs `make -f Makefile docs-check`, which executes the reference page Go tests, runs `.venv/bin/zensical build --clean --strict` and checks the built HTML in `site/` and Markdown files under `docs/` with `hack/docs-site-check.py`, including rejecting mermaid code fences (locally, `docs-check` creates `.venv` via `docs-venv` when `.venv/bin/zensical` is missing or `requirements-docs.txt` is newer). `ci-ok` requires `docs-check` to pass whenever `docs` is selected.
- `docs.yml` runs on pushes to `main` touching `docs/**`, `overrides/**`, `zensical.toml`, `requirements-docs.in`, `requirements-docs.txt` or `.github/workflows/docs.yml`, and on `workflow_dispatch`. Its `build` job installs `requirements-docs.txt` with `--require-hashes`, runs `.venv/bin/zensical build --clean --strict` and uploads `site` with `actions/upload-pages-artifact`. Its `deploy` job deploys that artifact with `actions/configure-pages` and `actions/deploy-pages` to the `github-pages` environment under concurrency group `pages` (`cancel-in-progress: false`).

Publishing requires GitHub Pages to be enabled in the repository settings with "GitHub Actions" selected as the source. No workflow file configures that setting.

## Releases

`release.yml` runs on a pushed `v*` tag. [Verify a release](how-to/verify-a-release.md) lists the commands that check a release.

No job or step of `release.yml` has an `if:` or `continue-on-error`. GitHub skips a job when a job in its `needs` fails or is skipped ([`needs`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idneeds)), so no job of `release.yml` runs after a job it needs has failed.
A job whose `needs` have succeeded can still wait. A run stays pending while another run of the concurrency group `release` is in progress, and a newer run of the group cancels a pending one and waits in its place ([`concurrency`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#concurrency)), and `publish` goes to a runner after the deployment protection rules of the `release` environment pass ([`environment`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idenvironment)).
The `needs` of each job:

| Job | Needs |
| --- | --- |
| `check` | nothing |
| `binaries` | `check` |
| `images` | `check` |
| `scan-images` | `images` |
| `sign-images` | `check`, `binaries`, `images`, `scan-images` |
| `sign-binaries` | `check`, `binaries`, `scan-images`, `sign-images` |
| `verify` | `check`, `images`, `sign-binaries`, `sign-images` |
| `publish` | `check`, `binaries`, `images`, `scan-images`, `verify` |

What can exist outside the run when a job other than `publish` fails, run artifacts left out. `hack/test/workflows_test.sh` derives both tables from `release.yml`. It reads the rows whose first cell is a job name in backticks, and fails when they differ from what it derived or name a job twice.

| Job that failed | Can exist from the other jobs | Can exist in part from the failed job |
| --- | --- | --- |
| `check` | nothing | nothing |
| `binaries` | untagged image digests in GHCR | nothing |
| `images` | nothing | untagged image digests in GHCR |
| `scan-images` | untagged image digests in GHCR | nothing |
| `sign-images` | untagged image digests in GHCR | image signatures, SBOM attestations and image provenance |
| `sign-binaries` | untagged image digests in GHCR; image signatures, SBOM attestations and image provenance | the signature of `checksums.txt` and the provenance of the release files |
| `verify` | untagged image digests in GHCR; image signatures, SBOM attestations and image provenance; the signature of `checksums.txt` and the provenance of the release files | nothing |

### Scan policy

- A HIGH or CRITICAL finding fails `scan-images`, with or without a fix, unless `.trivyignore.yaml` in the tagged commit accepts it, and the signing jobs need `scan-images`.
- Record an accepted finding as an entry in `.trivyignore.yaml`, in a reviewed pull request.
- The vulnerability database is pinned by digest in `TRIVY_DB` in `release.yml`; change the pin in a pull request.
- When a tag fails, fix the cause on `main`, cut the next release candidate from the new commit, and do not reuse the tag.

### Repository settings

`release.yml` does not set these repository settings, and no file in this repository holds their values:

- A tag ruleset that limits who may create `v*` tags. No step of `release.yml` checks who pushed the tag.
- Immutable releases. GitHub's documentation says that the assets of a published immutable release are protected from modification and deletion, and that its tag is locked to its commit ([immutable releases](https://docs.github.com/en/code-security/supply-chain-security/understanding-your-software-supply-chain/immutable-releases)). `publish` attaches the files to a draft before it publishes it.
- Required reviewers on the `release` environment, if wanted. `publish` names the environment and `release.yml` sets no reviewers. A workflow that names an environment that does not exist creates it with no protection rules ([managing environments](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments)).

### Known limits

- Not pinned by a checksum in this repository: the Go toolchain that `actions/setup-go` installs from `go.mod`, and the runner's `gh`, `jq`, Docker and preinstalled Buildx.
- The tests read the text of `release.yml` and run the release scripts under `hack/` on fixtures and stub commands. No test runs `release.yml`.
- No step of `release.yml` creates a Git tag: a run starts from a pushed tag.
- `publish` writes the image tags before it publishes the draft release, and no step removes them, so a failure in between can leave image tags next to a draft release.
