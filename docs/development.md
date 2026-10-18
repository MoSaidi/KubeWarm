# Development and validation

[Project README](../README.md) | [Documentation index](README.md)

## Validation

See [validation results](#validation-results). Local builds and race-enabled tests were run.

The [KubeWarm validation workflow](../.github/workflows/test.yaml) checks Go
formatting, runs `go vet`, race-enabled tests and builds, compiles the Python
client for syntax validation, lints and renders the Helm chart (including
optional resources in a non-default namespace), and builds both Docker images.
It runs directly on pull requests and non-`main` branch pushes. Main-branch
pushes and release tags run the same validation through reusable-workflow jobs
in the version-bump and release workflows, avoiding a duplicate standalone run
for those events.

## Git hooks

The repository's [pre-commit configuration](../.pre-commit-config.yaml) runs
file hygiene and YAML, JSON, and TOML checks, detects private keys, formats Go
files, and lints and renders changed Helm charts before commits. Helm templates
are excluded from plain YAML validation and checked through Helm instead.
Commitizen validates commit messages using the same version as CI. `go vet`
runs before pushes when Go source or module files change.

Install Git 2.31 or newer,
[uv](https://docs.astral.sh/uv/getting-started/installation/), Go, and Helm,
then run from the repository root:

```bash
uv tool install pre-commit
pre-commit install
pre-commit run --all-files
pre-commit run --all-files --hook-stage pre-push
```

`pre-commit install` installs the pre-commit, commit-msg, and pre-push hooks.
The first run downloads the pinned remote hooks and creates their environments.
Formatting hooks may modify files; review and stage those changes before
committing again. Go and Helm hooks use the executables on your `PATH`.

Race-enabled tests, builds, and Docker builds remain CI checks; run `task test`
locally when validating Go behavior. These hooks do not replace the existing CI
workflow.

## Source code layout

| Path | Responsibility |
|---|---|
| `app/cmd/controller/` | Controller process and component startup. |
| `app/cmd/worker/` | Node worker process. |
| `app/internal/controller/warming.go` | Warming jobs, policy reconciliation, readiness, and retries. |
| `app/internal/controller/topology.go` | Zone-aware source selection. |
| `app/internal/kube/nodes.go` | Kubernetes node discovery and metadata. |
| `app/internal/controller/` | Scheduling, image distribution, recovery and controller API. |
| `app/internal/worker/` | Containerd operations, transfers, reporting, mirror and cleanup. |
| `helm/kubewarm/` | Helm chart and warming-policy ConfigMap. |
| `examples/` | Local cluster, policy, request and CI examples. |
| `scripts/warm-and-wait.py` | CI job submission and readiness polling. |
| `grafana/` | Metrics dashboard. |
| `.github/workflows/` | Automated checks. |
| `Taskfile.yml` | Build, image, test and lint tasks (`task test`, `task build`, `task images`, `task lint`). |

## Release process

Pull requests and pushes to non-`main` branches are checked for Conventional
Commit messages. These message checks remain independent of validation.
After a qualifying commit reaches `main`, the
[version-bump workflow](../.github/workflows/bump-version.yml) first validates
that commit. Only after validation succeeds does Commitizen select
the next SemVer version, updates this repository's version files and
`CHANGELOG.md`, creates a `bump:` commit and an annotated `vX.Y.Z` tag, then
pushes both. The [tag workflow](../.github/workflows/create-release.yml) first
validates the tagged commit, then builds Linux/amd64 controller and worker
binaries, packages the Helm chart, publishes version-tagged images to GHCR,
and creates a GitHub Release with the binaries, chart archive and checksums.
Both publishing jobs use `needs: validation`: failed or cancelled validation
prevents the dependent job from running. The generated `bump:` commit is also
validated on `main`, but does not trigger another version bump.

Commit types determine the increment: `fix:` and `perf:` produce a patch
release, `feat:` produces a minor release, and a breaking change produces the
next minor while the project remains on `0.x` (`major_version_zero`). For
example, from `0.1.0`, a `fix:` results in `0.1.1`; a `feat:` or breaking
change results in `0.2.0`. Commits such as `docs:` do not trigger a version
bump.

Before enabling automatic bumps, ensure `v0.1.0` exists as the baseline tag
for the current `0.1.0` chart and images. The bump workflow requires a
repository secret named `RELEASE_TOKEN`: a token with contents write access
so the version commit/tag push triggers the release workflow. The `main`
branch must permit that token to push. Make the GHCR packages public if users
should be able to pull them without authentication. The chart package is
attached to the GitHub Release; the published image names are
`ghcr.io/<owner>/kubewarm-controller:<version>` and
`ghcr.io/<owner>/kubewarm-worker:<version>`.

Configure `RELEASE_TOKEN` under repository **Settings > Secrets and variables >
Actions > New repository secret**. Use a fine-grained personal access token
with access to this repository and **Contents: Read and write**. The token
owner must have repository write access, and any required organization approval
must be completed. This workflow does not declare a GitHub environment, so an
environment-only secret is not available to it.

An unset or unavailable secret resolves to an empty string; the version-bump
job checks for this before checkout. Do not substitute `GITHUB_TOKEN`: pushes
made with that token do not trigger the push-based release workflow.

<a id="validation-results"></a>


## Validation results



Environment: Linux/amd64, Go 1.24.7, Helm 3.17.3.

| Check | Result |
| --- | --- |
| `go test -race ./...` from app/ | Passed: all test packages, no race reports. |
| `go build ./cmd/controller ./cmd/worker` | Passed. |
| Controller race tests after final source-ordering change | Passed. |
| `helm lint helm/kubewarm` | Passed (optional chart icon informational message only). |
| Helm render in a non-default namespace | Passed. |
| Rendered namespace URL, optional registry credentials, mandatory tokens, one controller, Recreate strategy, no CRDs | Passed. |
| Python CI client syntax compilation | Passed. |
| Docker builds | Not run: Docker unavailable in this environment. |
| Live kind/EKS/containerd end-to-end tests | Not run: no cluster or container runtime available. |

Additional tests cover digest/selector/threshold validation, zone preference and
source saturation fallback, stale inventories, per-zone readiness, unauthorized
requests, timeout, newly eligible policy targets, pull acknowledgement versus
inventory readiness, peer failure fallback, and invalid policy reload.

Containerd integration tests requiring a live socket are not evidence of
a live-runtime validation here. The installation and testing runbook above documents the remaining cluster tests.
No production performance or AWS cost measurements were performed.
