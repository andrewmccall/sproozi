# Pinned agent clients

The client images that CI and the documented verifiers build install Codex,
Claude Code and OpenCode from the lockfiles in this directory, and run the
Hermes image by digest. [ADR 0021](../../docs/adr/0021-pinned-ci-inputs-and-latest-client-canary.md)
explains why.

| Client | Package | Pinned in | Used by |
| --- | --- | --- | --- |
| Codex | `@openai/codex` | `codex/package-lock.json` | `hack/demo/codex.Dockerfile` (the base of the released native image), `hack/verify/orchestration-kind/clients.Dockerfile` |
| Claude Code | `@anthropic-ai/claude-code` | `claude-code/package-lock.json` | `internal/runtime/docker/image/clients.Dockerfile` (the released native image), `hack/verify/orchestration-kind/clients.Dockerfile` |
| OpenCode | `opencode-ai` | `opencode/package-lock.json` | `hack/verify/orchestration-kind/clients.Dockerfile` |
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

Codex needs no install script. Claude Code and OpenCode each publish a
`postinstall` that copies the platform binary from an optional dependency over
a placeholder `bin/*.exe`. Without it, the command prints an error and exits 1.
`install.sh` does the same step itself for glibc Linux: it hard-links
`@anthropic-ai/claude-code-linux-<arch>/claude` and
`opencode-linux-<arch>/bin/opencode` over the placeholders. On x64 it always
selects the AVX2 OpenCode build rather than probing the build host's CPU, so
the image does not depend on where it was built. A CPU without AVX2 needs the
`opencode-linux-x64-baseline` binary instead.

## Bumping pinned clients

The weekly [Latest Clients Canary](../../.github/workflows/latest-clients.yml)
builds the newest releases with the same scripts disabled, runs `--version` and
`make test-e2e`, and opens or updates one tracking issue when the newest
releases differ from the pins or fail. To move the pins:

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

2. Check that each new lockfile still has `linux-x64` and `linux-arm64`
   entries with `integrity` values, and that the wrapper packages still ship
   only the placeholder-copying `postinstall` described above. Read the new
   install script if it changed; adjust `install.sh` rather than enabling
   scripts.
3. For Hermes, resolve the current index digest and replace the old digest
   everywhere it appears (`grep -rn hermes-agent@sha256` lists them):

   ```sh
   docker buildx imagetools inspect docker.io/nousresearch/hermes-agent:latest \
     --format '{{json .Manifest}}' | jq -r .digest
   ```

   Update the version comment from `/opt/hermes/install-stamp.json`
   (`displayVersion`).
4. Build the clients and check every command, as `build.py` does:

   ```sh
   docker build -t sproozi-local/clients:bump \
     -f hack/verify/orchestration-kind/clients.Dockerfile .
   for binary in codex claude opencode; do
     docker run --rm --network none --env HOME=/tmp sproozi-local/clients:bump "$binary" --version
   done
   ```

5. Open a pull request. PR CI then runs `make test-e2e` with the new pins.

Calico is pinned separately, by version and SHA-256, in
[`hack/verify/fetch-calico.sh`](../verify/fetch-calico.sh).
