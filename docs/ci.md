# CI

This page describes the checks under `.github/workflows/` that run on pull requests, on pushes to main and on a schedule. The layout follows the sister project [Shadewright](https://github.com/shadewright-labs/shadewright).

## Pull requests and pushes to main

`ci.yml` is the one entry point. Its `Changed files` job starts together with the jobs that always run (below), runs `hack/ci-changed.sh` on the files the pull request changes and sets four groups:

| Group | Runs when these change | Jobs |
| --- | --- | --- |
| go | Go code, `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `vendor/`, `cmd/`, `internal/`, `examples/`, the root `Dockerfile`, `.dockerignore`, `Dockerfile.dockerignore`, `Makefile`, `hack/`, `.golangci.yml`, `docs/reference/`, and the workflows that build Go outside `ci.yml` (`fuzz.yml`, `release.yml`) | lint, test, govulncheck, gosec |
| images | the inputs of the root `Dockerfile` build (Go code, `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `vendor/`, `cmd/`, `internal/`, `examples/`, the `Dockerfile`, `.dockerignore`, `Dockerfile.dockerignore`), `LICENSE` and `hack/setup-buildx.sh`, which the build job runs; `hack/test/ci-changed_test.sh` lists each input, including the scripts of `images-build.yml` and the `COPY` sources of the `Dockerfile` | image build (pull requests only) |
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

- `repo-checks.yml`: `hack/docs-links.py check`, which reads each tracked Markdown file paragraph by paragraph and fails on a relative link to a missing file or heading, and on a reference link with no definition. It follows inline links (also over several lines, with a title in double quotes, single quotes or parentheses), angle-bracket links, full and collapsed reference links, and `href` and `src` of `<a>` and `<img>` in any quoting; headings count in both the `#` and the underlined (Setext) form. It does not follow shortcut reference links (`[text]` alone) or links in other raw HTML. Links to pages or headings not written yet are listed in `hack/docs-pages-pending.txt` as `<source> -> <target>[#fragment]`, one line per occurrence: two links from one page to the same pending target need two lines, so a second broken link cannot hide behind the first one's entry. The check fails when a target exists or a line is no longer used, so remove the lines in the pull request that adds the page. The same job runs `hack/check-image-refs.sh`, which fails when a tracked file names an image under `ghcr.io/shadewright-labs/` other than `sablecourt` and `sablecourt-agent`, a tag `release.yml` never pushes (anything but `X.Y.Z`, `X.Y.Z-rc.N` and `latest`, such as `v0.1.0` or an empty tag after a colon), or a digest that is not `sha256:` and 64 lower-case hex digits; it sees only references written out in full, not ones assembled in shell variables. The job also runs `hack/check-dockerfiles.sh` and the offline tests under `hack/test/`, including `workflows_test.sh`, which checks that no workflow downloads Buildx or an SBOM tool without a checksum kept in this repository.
- `workflow-lint.yml`: actionlint, shellcheck over `hack/*.sh` and `hack/test/*.sh`, and zizmor over `.github/workflows`. zizmor is installed with `pip --require-hashes` from `hack/zizmor-requirements.txt`.
- `secret-scan.yml`: gitleaks with the default rules plus `.gitleaks.toml`, over every ref and the full history, including the changes that only a merge commit introduces (`hack/secret-scan.sh`, which passes `--diff-merges=first-parent`; without it a token added while resolving a merge is never scanned). gitleaks ignores the exit status of the `git log` it runs, so `hack/secret-scan.sh` reads the same range itself first: it fails when `git log` or `git rev-list` fails, when the log does not list exactly the commits `git rev-list --count` counts, and when gitleaks reports fewer scanned commits than the history has commits with a text change (gitleaks does not count empty commits, mode changes, pure renames, binary changes or deletions). The test fixtures in the tree are joined from split strings, so stock gitleaks finds nothing in them and they need no allowlist. The history still holds the earlier literals: each entry of `.gitleaks.toml` allows one exact value, anchored at both ends, in one file and only in the commits that hold it, so the same value in a new commit is reported. The one entry without commits allows a single token in `internal/gate/mcp/upstream_test.go`, which this layout does not change. Before the scan, `hack/test/gitleaks_test.sh` checks that a token in an ordinary file, a key block or token added before or after the fixtures of each file the allowlists name, an allowed value in a new commit, and a token added only in a merge commit are all found, that every entry is scoped this way, and that a broken history read fails the scan. A branch made before this change that edits one of the old fixture lines adds a commit the allowlists do not name, and the scan reports it; rebase it on main.

The last job, `ci-ok`, needs all of them and runs `hack/ci-ok.sh` on their results. It passes only when `Changed files` succeeded and each of its four outputs is exactly `true` or `false`, every job that the groups and the event select succeeded, and every job that always runs succeeded. A job counts as legitimately skipped only when its group is `false` or the event excludes it. A job missing from the results, a selected job that was skipped, and any failure or cancellation fail `ci-ok`. It sees the result of each called workflow, not the steps inside it, so a check removed from a called workflow that still succeeds goes unnoticed there. Its tests are `hack/test/ci-ok_test.sh`, which also checks that the job list in the script matches the `needs` of `ci-ok`. A new pull request run cancels the older run of the same pull request; runs on main are never cancelled.

Two more workflows run on pull requests outside `ci.yml`. `dependency-review.yml` (job `Dependency review`) fails on a new or bumped dependency with a known vulnerability of moderate severity or higher, or with one of ten licences named by exact SPDX identifier: GPL-2.0, GPL-3.0, AGPL-3.0, LGPL-2.1 and LGPL-3.0, each in its `-only` and `-or-later` form. `pr-title.yml` (job `Conventional PR title`) checks the title against Conventional Commits: an allowed type, a scope from its list if there is one, and a summary of at least two characters that starts with a lower-case letter and does not end with a full stop. It runs on `pull_request_target`, so GitHub takes the workflow from the base branch, not from the pull request.

Every workflow except `pr-title.yml` starts with `permissions: {}` and grants each job only what it uses; `pr-title.yml` grants `pull-requests: read` at workflow level and never checks out code. Every workflow checks out with `persist-credentials: false` and pins every action to a full commit SHA. Executables that a job downloads itself (actionlint, gitleaks, Buildx) are checked against a sha256 kept in the script that installs them, and the BuildKit image is pinned by digest.

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

`fuzz.yml` finds its targets with `hack/fuzz-targets.sh`, which asks the go command (`go test -list`), so a target with an unnamed parameter, a signature over several lines or an alias of `testing` is listed like any other. It sees the files that build on linux/amd64 with no extra build tags; the job fails when a package does not build or the list is empty. Dependabot (`.github/dependabot.yml`) opens weekly updates for Go modules, GitHub Actions and the base images of the five Dockerfile directories. The base images of the root `Dockerfile` are pinned by tag and digest, and Dependabot updates both.

## Releases

Releases still come from `release.yml` as it is on main: a `v*` tag builds the binaries for darwin and linux (amd64 and arm64), publishes them with `checksums.txt` as a GitHub release, and pushes the `daemon` and `agent` images to `ghcr.io/shadewright-labs/sablecourt` and `ghcr.io/shadewright-labs/sablecourt-agent`, tagged with the version without its `v` and `latest`. Like the other workflows it pins its actions by commit, grants each job only what it uses and installs Buildx with `hack/setup-buildx.sh`; what it builds and publishes is unchanged. A ruleset restricts the creation of `v*` tags to repository admins. A signed and verified release path is tracked in a follow-up issue.
