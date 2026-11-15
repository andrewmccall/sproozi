# 0059: Renovate proposes pin updates

Status: accepted
Date: 2026-11-15

## Problem and constraints

[ADR 0021](0021-pinned-ci-inputs-and-latest-client-canary.md) fixed every input
that pull request CI runs: agent clients by lockfile, Hermes by digest, actions
and base images by digest or SHA. It left the moving of those pins to a person
following [Bumping pinned clients](../../hack/clients/README.md#bumping-pinned-clients),
and made the weekly canary report drift in one issue.

In practice the pins were behind every week, and the issue never closed: the
canary says that a newer release exists, but someone must still run the update,
review the diff and open a pull request. The work is mechanical for the
version, the lockfile and the digest copies, and needs judgement only for what
the new release does at install time.

Constraints: pull request CI keeps running only pinned inputs, and every pin
change is reviewed by a code owner. The bot must not merge, must not hold
secrets that reach untrusted code, and must not move what has its own gate:
Go modules and the toolchain (`make verify-deps`).

## Decision

Adopt [Renovate](https://docs.renovatebot.com/), configured in
[`.github/renovate.json5`](../../.github/renovate.json5), scoped to the pins.
It proposes pull requests; people review and merge them. The canary stays as
the early warning that the latest clients break.

- **Clients.** One weekly pull request, "agent clients", updates the four
  `package.json` and lockfile in each `hack/clients` directory. Renovate runs npm with install
  scripts disabled, as the pins require, and holds a release for three days.
- **Hermes.** A separate pull request moves the index digest of the `latest`
  tag. A regex manager finds it in the five places it is written (the
  verifier, an example, a guide twice and a verifier README), so they move
  together.
- **Actions and base images.** One pull request each for GitHub Actions SHAs
  and for container image digests, including BuildKit in workflow
  `driver-opts`. Image tags stay as chosen; only digests move, and `golang`
  never moves because the Go toolchain pin is moved by hand.
- **Off.** Only the `github-actions`, `dockerfile`, `npm` and regex managers
  are enabled. Go modules, the toolchain pin, Python requirements, kustomize
  image tags and the devcontainer are not touched.
- **No automerge, no dashboard.** Branch protection and CODEOWNERS apply to
  these pull requests as to any other. A single maintainer reads the pull
  request list; a dashboard issue would be a second list.
- **Calico stays manual.** `hack/verify/fetch-calico.sh` pins a version and the
  SHA-256 of the downloaded manifest. Renovate can propose the version but
  cannot compute the checksum, and a pull request that changes one without the
  other fails closed in the script.
- **Review.** What Renovate cannot judge is a reviewer's checklist in
  [`hack/clients/README.md`](../../hack/clients/README.md#reviewing-a-renovate-pull-request):
  wrapper install scripts, platform integrity entries and `install.sh`.
- **Canary.** Its report job no longer opens an issue for drift alone, since
  Renovate's pull request is the record of drift. It still opens or updates the
  issue when the latest clients fail to build or fail `make test-e2e`.

ADR 0021's rule that changes to pins are reviewed by a code owner is unchanged.
This record replaces its statement that a person moves the pins, and its
canary's issue on drift.

## Alternatives and tradeoffs

- **Keep the manual process.** No new automation or app permissions, but it has
  not kept up, and the issue became noise that hid real failures.
- **Let the canary open the pull request.** The canary already knows the new
  pins. But a workflow that pushes branches needs `contents` and
  `pull-requests: write`, and a pull request opened with the workflow token does
  not start other workflows, so reviewers see no CI. ADR 0021 rejected a
  bot with push rights for the same reason. A GitHub App's rights are held
  outside the repository's workflows.
- **Dependabot.** It updates npm lockfiles, actions and Dockerfile digests, with
  no new app. It cannot read a digest from a Python file, a guide or an example
  manifest, so the Hermes pin would stay manual, and it has no regex rule for
  digests written in other file types.
- **Renovate for everything, Go included.** Rejected: Go dependencies have a
  vulnerability and tidiness gate and are moved with that gate in view.

Costs: a third-party app can push branches. It cannot merge, its pull requests
need a code owner's review, and CI on those pull requests runs only what the
branch pins. Renovate's lockfile output must match the reviewed process; the
checklist covers what it does not. One more file to maintain, and a regex that
must be updated if the Hermes image is written in a new place.

## Revisit conditions

Revisit if Renovate cannot reproduce the lockfile contents the review expects
(missing Linux x64 or arm64 integrity entries), if the pull requests go
unreviewed long enough for pins to age, if the app's permissions change, if the
Hermes digest appears in more places than the regex lists, or if drift without
a pull request (for example when Renovate is down) needs the canary's issue
again.

## Links

- [`.github/renovate.json5`](../../.github/renovate.json5)
- [Pinned agent clients](../../hack/clients/README.md)
- [Latest Clients Canary](../../.github/workflows/latest-clients.yml)
- [ADR 0021](0021-pinned-ci-inputs-and-latest-client-canary.md)
