# Publish a release

The release workflow packages the CLI for macOS and Linux on x86-64 and ARM64,
and Windows on x86-64. The target list is
[`hack/release/targets.json`](../../hack/release/targets.json). Every target runs
native host tests and executes its extracted release archive.

## Prepare

1. Merge the intended changes and check that Tests, Lint, E2E, Native platforms
   and the release snapshot jobs pass for that commit.
2. Review changes to commands, configuration and platform limits. Update the
   affected guides and architecture decisions. Run `make verify-docs`.
3. Run the applicable runtime acceptance checks in
   [the testing guide](../../test/acceptance/README.md). Record the tested
   revision and any limits. Packaging tests do not replace a real model,
   Kubernetes or Docker Desktop acceptance run.
4. Choose a new product version. Never replace an existing public release.

Agent clients continue to use upstream defaults. A Sproozi release number does
not select a Codex, Claude Code, OpenCode or Hermes version.

## Publish

Tag the tested commit and push the tag:

```sh
git tag -a v0.1.0 -m 'Sproozi v0.1.0'
git push origin v0.1.0
```

Replace `v0.1.0` with the new version. The **Releases** workflow builds the
archives and a source bundle with vendored Go modules. It verifies checksums,
tests each archive on its native runner, builds and tests the Homebrew source
formula on macOS and Linux, uploads a draft, downloads the assets
again and compares their hashes before publishing. A public release cannot be
overwritten. To retry a failed release, run **Releases** manually and enter the
existing tag in the `tag` field. This checks out that tag and repeats every
release gate. Leave the field empty to test a snapshot without publishing.
An interrupted draft can be retried.

The public [Homebrew tap](https://github.com/andrewmccall/homebrew-tap) checks
stable releases every hour. To update immediately, run its **Update Sproozi**
workflow. That workflow checks the source checksum, builds and tests the
formula, then commits it. Each repository uses its own workflow token. No tap
write token is stored in Sproozi.

After publication, check the download links and run a fresh
`brew install andrewmccall/tap/sproozi`. Confirm `sproozi --version` reports the
intended release.

## What the CLI release contains

The archives contain `sproozi` and its license. They do not install a controller,
worker images, cluster resources or credentials. Follow the
[installation guide](../guides/install.md) for those prerequisites.

Standalone binaries are unsigned. Homebrew builds the source formula locally;
it does not remove macOS quarantine from a downloaded binary. Add signing and
notarization before switching macOS Homebrew installation to prebuilt binaries.
