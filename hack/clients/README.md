# Pinned agent clients

The client images that CI and the documented verifiers build install Codex,
Claude Code, OpenCode and Copilot from the lockfiles in this directory, and run the
Hermes image by digest. [ADR 0021](../../docs/adr/0021-pinned-ci-inputs-and-latest-client-canary.md)
explains why.

| Client | Package | Pinned in | Used by |
| --- | --- | --- | --- |
| Codex | `@openai/codex` | `codex/package-lock.json` | `hack/demo/codex.Dockerfile` (the base of the released native image), `hack/verify/orchestration-kind/clients.Dockerfile` |
| Claude Code | `@anthropic-ai/claude-code` | `claude-code/package-lock.json` | `internal/runtime/docker/image/clients.Dockerfile` (the released native image), `hack/verify/orchestration-kind/clients.Dockerfile` |
| OpenCode | `opencode-ai` | `opencode/package-lock.json` | `hack/verify/orchestration-kind/clients.Dockerfile` |
| GitHub Copilot CLI | `@github/copilot` | `copilot/package-lock.json` | `hack/verify/orchestration-kind/clients.Dockerfile` |
| Hermes | `docker.io/nousresearch/hermes-agent` | `HERMES_IMAGE` in `hack/verify/orchestration-kind/build.py` | `make test-e2e`, Hermes guides and `examples/orchestration/hermes-coordinator.yaml` |

A new harness's client gets its own lockfile directory here. Add it to
`hack/verify/orchestration-kind/clients.Dockerfile`, the image the Kind
verifiers run AgentRuns in, when a verifier exercises it. Only a managed
harness also goes into the released native image,
`internal/runtime/docker/image/clients.Dockerfile`, which adds Claude Code to
the Codex base built from `hack/demo/codex.Dockerfile`. See
[Add a harness](../../docs/contributing/add-a-harness.md). Add the client to
[`install.sh`](install.sh)'s `case`, which links its command into
`/usr/local/bin`, and to [`update.sh`](update.sh)'s client list, which
refreshes the lockfiles.

Each lockfile records an integrity hash for the wrapper package and for every
platform package, including `linux-x64` and `linux-arm64`. Build client images
from the repository root so this directory is in the build context.

## How images install them

[`install.sh`](install.sh) runs `npm ci --ignore-scripts` for each named
client, so npm rejects any package whose content differs from the lockfile and
no package lifecycle script runs. It then links each command into
`/usr/local/bin`.

Codex and Copilot need no install script: each wrapper selects its platform
package at run time. Claude Code and OpenCode each publish a `postinstall`
that selects a platform binary and replaces a placeholder `bin/*.exe`. Their
scripts also detect the host platform; OpenCode can try a fallback download
and runs the binary to check it. Without replacing the placeholder, the
command prints an error and exits 1. `install.sh` selects the locked
dependency itself for glibc Linux: it hard-links
`@anthropic-ai/claude-code-linux-<arch>/claude` and
`opencode-linux-<arch>/bin/opencode` over the placeholders. On x64 it always
selects the AVX2 OpenCode build rather than probing the build host's CPU, so
the image does not depend on where it was built. A CPU without AVX2 needs the
`opencode-linux-x64-baseline` binary instead.

## Bumping pinned clients

[Renovate](../../.github/renovate.json5) proposes the pin updates, and a code
owner reviews and merges them ([ADR 0059](../../docs/adr/0059-renovate-proposes-pin-updates.md)).
On Mondays it opens one pull request for the four clients and their lockfiles
("agent clients"), one for the Hermes image digest, one for GitHub Actions and
one for container image digests. It never merges. Go modules, the Go toolchain
pin, Python requirements, kustomize image tags and the devcontainer are not
Renovate's; move them by hand. The weekly
[Latest Clients Canary](../../.github/workflows/latest-clients.yml) builds the
newest releases with the same scripts disabled, runs `--version` and
`make test-e2e`, and opens or updates one tracking issue when they fail.

### Reviewing a Renovate pull request

Renovate runs npm with lifecycle scripts disabled and rewrites each
`package.json` and lockfile, as `update.sh` does. It cannot judge what a new
release does at install time. Before merging a client pull request:

1. Check that each changed lockfile still has `linux-x64` and `linux-arm64`
   entries with `integrity` values for the wrapper's platform packages. Renovate
   runs on one platform, and an update that drops an optional platform entry
   must be rerun with `hack/clients/update.sh`.
2. Inspect the wrapper packages' `postinstall` and other install scripts and
   confirm that `install.sh` still selects the correct locked binary and
   command. Adjust `install.sh` in the same pull request when that contract
   changes, rather than enabling lifecycle scripts or fallback downloads.
3. Search for the old versions in prose that records them, which Renovate does
   not update: `grep -rn '<old version>' README.md docs` (the pinned Codex and
   Claude Code versions in `README.md` and `docs/reference/harnesses.md`).
4. Read the PR CI result: `make test-e2e` runs with the new pins, and the
   GitHub-authenticated Copilot verifier runs through a local provider fixture.
   The registry-policy URL has a separate authenticated gateway probe, because
   the fixture individual account can skip registry discovery.

For the Hermes pull request, check that all occurrences changed together
(`grep -rn hermes-agent@sha256` lists them; the regex rule in
`.github/renovate.json5` covers `build.py`, `hack/verify/portable-workflows/README.md`,
`docs/guides/hermes-orchestration.md` and
`examples/orchestration/hermes-coordinator.yaml`) and that PR CI passed. When
the digest is written in a new file, add that file to the rule.

### Moving a pin by hand

When Renovate is off, or to move one client to a chosen release:

1. Re-resolve every npm client to its current `latest` release. This reads
   registry metadata only; no package code runs.

   ```sh
   hack/clients/update.sh
   ```

   To move one client to a chosen release instead:

   ```sh
   npm install --prefix hack/clients/opencode --package-lock-only \
     --ignore-scripts --save-exact opencode-ai@1.19.3
   ```

2. Do the lockfile and install script checks above.
3. For Hermes, resolve the current index digest and replace the old digest
   everywhere it appears (`grep -rn hermes-agent@sha256` lists them):

   ```sh
   docker buildx imagetools inspect docker.io/nousresearch/hermes-agent:latest \
     --format '{{json .Manifest}}' | jq -r .digest
   ```

4. Build the clients and check every command, as `build.py` does:

   ```sh
   docker build -t sproozi-local/clients:bump \
     -f hack/verify/orchestration-kind/clients.Dockerfile .
   for binary in codex claude opencode; do
     docker run --rm --network none --env HOME=/tmp sproozi-local/clients:bump "$binary" --version
   done
   ```

5. Open a pull request, and read the PR CI result as above.

Calico is pinned separately, by version and SHA-256, in
[`hack/verify/fetch-calico.sh`](../verify/fetch-calico.sh). Renovate does not
move it: it cannot compute the checksum of a new manifest.
