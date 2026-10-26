# 0050: Prebuilt CLI installation and explicit container builds

Status: accepted
Date: 2026-10-26

Partly supersedes [ADR 0024](0024-native-host-platforms-and-cli-releases.md)'s source-only Homebrew choice.

## Problem and constraints

The Homebrew formula compiles the CLI for each user, installing Go even though
Sproozi does not need a Go runtime. Users also need an explicit way to build
both release images from source without first initializing local sessions.

## Decision

Publish tested Homebrew bottles for macOS and Linux on ARM64 and x86-64.
Keep the source formula and `--build-from-source`. The tap builds every bottle
before publishing a new formula, verifies archive hashes, tests reinstalling
the bottles, and records their checksums. Polling the same release preserves
those checksums. Release archives remain another prebuilt installation option.

Keep digest-pinned published images as the default. Add `images build`
using the release Docker Bake recipe for native and control images. Load
locally unless the operator selects `--push` with a repository prefix. The
command builds from `--build DIR` when given, else from release source the
package installed beside the CLI (`PREFIX/share/sproozi`, which the formula
installs), else from the CLI's release source bundle, downloaded, checked
against the release's `checksums.txt`, expanded into a temporary directory and
removed afterwards. Release source must match the CLI version; explicitly
supplied checkouts support custom builds. A development CLI has no release and
needs `--build`. This operation uses the administrator's Docker context and
registry credentials, outside agent execution. It changes no installation or
cluster. The existing setup `--build DIR` continues to build and select the
native image.

Remove Copilot's obsolete `--no-remote` launch argument and pin 1.0.103. The
original offline grouping in ADR 0046 did not establish a separate requirement
for suppressing remote sessions. No replacement setting is introduced. Keep
explicit MCP configuration, disabled built-in MCPs, disabled updates and the
existing sandbox/gateway enforcement. This resolves the removal noted in ADR
0048 without treating `--no-remote` as a network isolation control.

## Alternatives and consequences

A formula downloading raw release executables would avoid Go but lose the
normal Homebrew source-build path. Bottles preserve both. Source-only installs
remain possible on platforms without a compatible bottle.

Building automatically after pull failures would obscure registry problems and
introduce unexpected build dependencies. Only the `images build` command builds;
no pull failure falls back to it. Within that command, finding release source
is automatic: requiring operators to fetch, verify and extract the bundle
themselves added steps without adding trust, because they would check the same
`checksums.txt`. That checksum shares the release with the bundle, so it detects
corruption, not a replaced release. Packaged source is preferred: Homebrew has
already checked it against the formula's SHA-256 and it needs no download.
Embedding the bundle's digest in the CLI would close that gap but requires the
release to produce the bundle before the binaries. Local image builds still
download base images and dependencies, and cluster delivery is a separate step.

Revisit when additional host platforms need bottles, when the release can
embed the source bundle digest in the CLI, or when users need cluster loading
integrated into the CLI.

## References

- [CLI installation](../guides/install.md)
- [Image builds](../reference/local-installation.md#build-container-images-explicitly)
- [Build command](../../cmd/sproozi/images.go)
- [Release recipe](../../hack/release/docker-bake.hcl)
- [Homebrew tap](https://github.com/andrewmccall/homebrew-tap)
