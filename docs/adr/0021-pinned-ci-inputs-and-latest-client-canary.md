# 0021: Pinned CI inputs and a latest-clients canary

Status: accepted
Date: 2026-10-18

## Problem and constraints

Sproozi is a public repository, and its CI runs on pull requests. The `E2E`
workflow and local `make test-e2e` built the acceptance clients with
`npm install --global @openai/codex @anthropic-ai/claude-code opencode-ai`.
That resolved whatever each package's `latest` tag named at build time and ran
its install scripts. The suite then pulled `nousresearch/hermes-agent` without a
tag or digest and ran it in the cluster, and CI applied a Calico manifest
fetched without a checksum. The demo Codex image and the combined Docker client
image also installed unversioned packages.

[ADR 0015](0015-default-clients-and-native-tool-schemas.md) chose this on
purpose, to stop coordinated version edits across checks, parsers and
registrations. But it also meant that every pull request ran code that no one
in the project had reviewed, chosen by an upstream publisher at that moment. A
compromised or broken upstream release would run in CI without a change to
this repository, and two runs of the same commit could test different code.

The other CI inputs were already fixed: workflows pin actions by SHA, use
`pull_request` with read-only tokens and no secrets, and Go modules, tools,
base images, Kind and git downloads are pinned by digest or checksum.
Repository settings require approval for outside pull requests and code-owner
review.

## Decision

Pull request CI and the documented verifiers execute only inputs pinned in this
repository, and changes to those pins are reviewed by a code owner.

- **npm clients.** Each client has a `package.json` and `package-lock.json` in
  [`hack/clients`](../../hack/clients/README.md) with an exact version and an
  integrity hash for the wrapper and every platform package. Client images
  install them with `npm ci --ignore-scripts`. Claude Code and OpenCode ship a
  `postinstall` that only copies the platform binary from an optional
  dependency over a placeholder. `hack/clients/install.sh` does that copy
  itself instead of enabling package scripts.
- **Hermes.** `hack/verify/orchestration-kind/build.py` runs
  `nousresearch/hermes-agent` by index digest, and the guides and the
  coordinator example use the same digest.
- **Calico.** [`hack/verify/fetch-calico.sh`](../../hack/verify/fetch-calico.sh)
  holds the only copy of the expected SHA-256. CI, `make demo` and every
  documented fetch call it, and it verifies an existing file rather than
  trusting it.
- **Ownership.** [`.github/CODEOWNERS`](../../.github/CODEOWNERS) names the
  maintainer as owner of workflows, `hack/`, `config/`, the Makefile,
  Dockerfiles, Go module files and package lockfiles.

The [Latest Clients Canary](../../.github/workflows/latest-clients.yml) is the
only channel that runs unreviewed upstream releases. It runs weekly and on
manual dispatch, never on a pull request. Its first job, with `contents: read`,
rewrites the lockfiles to the current `latest` releases in its own checkout,
resolves the current Hermes digest, builds the clients with scripts still
disabled, runs each `--version` and then `make test-e2e`. Its second job, with
only `issues: write`, runs no repository or upstream code. It checks the first
job's outputs against fixed patterns and opens or comments on one tracking
issue when the latest releases differ from the pins or fail.

Pins move only by pull request, following
[Bumping pinned clients](../../hack/clients/README.md#bumping-pinned-clients).

This replaces the package-selection part of ADR 0015: "install with
unversioned package names", "pull Hermes without a release tag or digest" and
"rebuild client images with fresh package resolution in acceptance tests". The
rest of ADR 0015 stands. The product still has no exact-version checks in
activation, state parsing or registration, and an administrator can build
images with any client release.

## Amendment: CI caching

Added 2026-10-18. With every input pinned, CI reuses earlier work instead of
rebuilding it on each run:

- **Go modules and build output.** `actions/setup-go` caches them, keyed on
  `go.sum`.
- **golangci-lint.** `make lint` installs the release archive checked against a
  pinned SHA-256, then builds the logcheck plugin binary from the pinned
  `.custom-gcl.yml`. The `Lint` workflow caches that binary, keyed on the files
  that define it, and the analysis cache, keyed on the golangci-lint version,
  `go.sum` and `.golangci.yml`.
- **Docker layers.** In the `E2E` workflow, `SPROOZI_BUILD_CACHE=gha` makes
  `make docker-build` and `build.py` build with buildx and the GitHub Actions
  cache backend, then load the image into Docker for Kind. The BuildKit image
  is pinned by digest. Local builds use plain `docker build`.

This is safe because a cached result can only come from the same pinned
inputs: lockfiles with integrity hashes, base images by digest and modules
checked by `go.sum`. So `build.py` no longer passes `--pull --no-cache`. A
cache hit skips repeating work; it does not choose different code. The
canary still builds with `--pull --no-cache`, because it resolves new releases.

Cache scope: only caches written by pushes to `main` are shared with pull
requests. GitHub scopes a cache written by a pull request run to that pull
request's ref, so it can read `main`'s caches but not another pull request's,
and its own writes reach no other branch. The workflows rely on this rule
rather than adding their own. The `E2E` workflow passes the Actions cache
service variables to the `make test-e2e` step because buildx reads them from
the environment. Code in that job could already reach them through the
runner, so this grants no new access.

Revisit if a cached input stops being pinned, if GitHub changes cache scoping
for pull requests, or if a cache hit is ever found to hide a build failure.

## Alternatives and tradeoffs

- **Keep upstream defaults in PR CI.** This finds upstream breakage sooner
  with no bump work, but it runs unreviewed code on every pull request, and a
  red build can come from upstream rather than the change under test.
- **Pin versions without lockfiles** (`npm install -g pkg@1.2.3`). Exact
  versions stop silent upgrades, but npm resolves the optional platform
  packages at install time without integrity hashes from the repository, and a
  republished or substituted dependency would go unnoticed.
- **Enable install scripts for a pinned version.** The scripts are short today,
  but they are code that runs as root during the build, and the OpenCode
  script can run a second `npm install` outside the lockfile when its binary is
  missing. Replacing the copy step costs one small script that must be checked
  at each bump.
- **Let a bot open bump pull requests.** That would need a token that can push
  branches and could chain into PR CI. Opening an issue needs only
  `issues: write` and leaves the change to a reviewed pull request.
- **Vendor client tarballs or mirror images.** This removes the registry from
  the build but adds hundreds of megabytes per release to the repository or a
  registry to run. Integrity hashes and digests give the same content
  guarantee.

Costs: someone must bump pins, and PR CI can lag behind upstream by up to a
week plus review time. The canary's runs can be red for reasons outside this
repository; it reports in an issue and never blocks a pull request.
`install.sh` chooses the AVX2 OpenCode binary on x64, so it does not run on
x64 CPUs without AVX2.

## Revisit conditions

Revisit if a client needs a real install step that the repository cannot
reproduce, if npm adds provenance checks that make the lockfile redundant, if
the canary's drift issues go unhandled long enough that the pins become
unsupported upstream, or if the project starts publishing release images that
need a separate client release policy.

## Links

- Implementation: [`hack/clients`](../../hack/clients/), the
  [client image Dockerfiles](../../hack/verify/orchestration-kind/clients.Dockerfile),
  [`build.py`](../../hack/verify/orchestration-kind/build.py),
  [`fetch-calico.sh`](../../hack/verify/fetch-calico.sh),
  [`install-golangci-lint.sh`](../../hack/verify/install-golangci-lint.sh),
  [`lint.yml`](../../.github/workflows/lint.yml),
  [`test-e2e.yml`](../../.github/workflows/test-e2e.yml) and
  [`latest-clients.yml`](../../.github/workflows/latest-clients.yml).
- [Running acceptance checks](../../test/acceptance/running.md#harnesses-and-hermes-in-kind).
- Checks for this change: the pinned clients image was built for linux/arm64
  and linux/amd64 and each `--version` passed with install scripts disabled;
  `make test-e2e` passed on linux/arm64 with the pinned clients, Hermes digest
  and verified Calico; `actionlint` passed on the workflows. The canary
  workflow itself has not yet run on GitHub.
