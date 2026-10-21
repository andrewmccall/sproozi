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

A Sproozi release publishes worker images built from the client lockfiles in
[`hack/clients`](../../hack/clients/README.md) at the tagged commit. The release's
`images.json` records those client versions. Kubernetes runtimes and the stock
examples can still select other images.

## Publish

Tag the tested commit and push the tag:

```sh
git tag -a v0.1.0 -m 'Sproozi v0.1.0'
git push origin v0.1.0
```

Replace `v0.1.0` with the new version. The **Releases** workflow first builds the
native and control images on native amd64 and arm64 runners with
[`hack/release/docker-bake.hcl`](../../hack/release/docker-bake.hcl). It pushes
them by digest to `ghcr.io/OWNER/sproozi-native` and `ghcr.io/OWNER/sproozi`
with provenance and SBOM attestations, merges each into one multi-platform
index, and records `images.json`. It then builds the archives, embedding the
native image digest in the CLI, and a source bundle with vendored Go modules
and the same digest. Finally it renders `sproozi_VERSION_kubernetes.tar.gz` with
the release digests. It verifies checksums,
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

A failed release can leave pushed images behind. The CLI pins digests, so a
retry with the same tag is safe; the version tag then names the retried images.

The first push creates the GHCR packages. GitHub may create them as private;
make both packages public once in the package settings, or `sproozi init
--local` and cluster nodes cannot pull them. Publication requires `images.json`,
and each native smoke test checks that its archive embeds the published native
image.

## What a release contains

| Asset | Contents |
| --- | --- |
| CLI archives | `sproozi` and its license. `sproozi version` prints the embedded native image. |
| Source bundle | The tagged source with vendored modules and the embedded native image, for Homebrew. |
| `images.json` | Native and control image references, per-platform digests, client versions, helper contract and recipe hash. |
| `sproozi_VERSION_kubernetes.tar.gz` | CRDs, the digest-pinned install bundle, the optional certificate controller, quickstart overlays and the first check. |
| `checksums.txt` | SHA-256 of every other asset. |

The archives install nothing by themselves. `sproozi init` pulls the
native image; the [Kubernetes guide](../guides/install-kubernetes.md) applies
the Kubernetes assets.

Standalone binaries are unsigned. Homebrew builds the source formula locally;
it does not remove macOS quarantine from a downloaded binary. Add signing and
notarization before switching macOS Homebrew installation to prebuilt binaries.
