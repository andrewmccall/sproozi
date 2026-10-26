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
formula on Apple silicon macOS and Linux (ARM64 and x86-64), bottles each build, tests each
bottle after reinstalling it, publishes the archives in the tap repository,
then commits the formula with the bottle checksums. A failed matrix leaves the
previous formula in place. Repeated polling preserves existing bottle metadata. Each repository uses its own workflow token. No tap
write token is stored in Sproozi.

After publication, check the download links and run a fresh
`brew install andrewmccall/tap/sproozi`. Confirm `sproozi --version` reports the
intended release.

A failed release can leave pushed images behind. The CLI pins digests, so a
retry with the same tag is safe; the version tag then names the retried images.

The first push creates the GHCR packages. GitHub may create them as private;
make both packages public once in the package settings, or `sproozi init`
and cluster nodes cannot pull them. Publication requires `images.json`,
and each native smoke test checks that its archive embeds the published native
image.

## Versioning and compatibility

Sproozi is pre-1.0 and versions follow `v0.MINOR.PATCH`.

- **Any minor release may break compatibility.** That covers the
  `sproozi.com/v1alpha1` CRDs, the local `config.json`, session journals,
  checkpoints and the gateway's stored state. There is one API version and no
  conversion webhook, so an upgrade does not convert stored objects.
- **A patch release does not break compatibility.** It fixes bugs and security
  issues only.
- **Release notes name every break.** Give a breaking commit a
  `BREAKING CHANGE:` footer that says what operators must do. The publish step
  lists those commits under "Breaking changes" in the GitHub release, with the
  full text of each footer. If a release changes anything under `api/`,
  `config/` or `examples/` and no commit carries a footer, publishing is
  refused until the tag's message says `Breaking-Change: none`, which records
  that someone checked. A `type!:` subject alone is not enough, because it
  gives operators no steps. The check also runs when publishing resumes an
  existing draft, and rewrites that draft's notes. One footer anywhere in the
  release satisfies the guard for every changed path, so make it cover every
  change operators must act on. Release from an annotated tag (`git tag -a`):
  publishing refuses a lightweight tag. The first release, with no earlier
  `v*` tag in its history, needs no record. The guard needs full history: in a
  shallow checkout, or when git cannot tell whether an earlier `v*` tag
  exists, publishing refuses rather than treating the release as the first. The release body is
  the changelog; there is no separate file.
- **Upgrade in place only within a minor version.** Across minor versions, stop
  running sessions and runs first, then follow the notes, which name any object
  a release no longer ships and `kubectl apply` leaves behind. See the
  [Kubernetes upgrade steps](../guides/install-kubernetes.md#upgrade).
- **Only the latest release is supported.** See [SECURITY.md](../../SECURITY.md).

### The alpha API

The CRDs are `sproozi.com/v1alpha1`, and they stay alpha until the criteria
below are met ([ADR 0044](../adr/0044-the-api-stays-alpha-with-a-written-policy.md)).
Sproozi stays at `0.x` while the API is alpha. Pin the release you install.

- **One version, no conversion.** The controller serves and stores only
  `v1alpha1`. There is no conversion webhook, so an upgrade never rewrites
  stored objects.
- **A minor release may change the CRDs incompatibly.** It may add, remove or
  rename a field, change what a field means, or tighten validation. Each such
  change carries a `BREAKING CHANGE:` footer that names the field and says what
  operators must change.
- **A patch release never changes the CRDs.**
- **Changes are additive where they can be.** A new field is optional, with a
  default that keeps existing objects valid. A field is removed or renamed only
  when keeping it would mislead.
- **Upgrading across a minor version.** First delete finished AgentRuns and
  stop running ones, while the old controller can still remove their
  finalizers. Then save your AgentRuntimes, AgentPolicies and AgentTemplates
  with `kubectl get ... -o yaml`. Apply the new CRDs and `install.yaml`, then
  reapply any object the release notes say to change.

The API moves to `v1beta1` when all of these hold:

- No minor release has changed the CRDs incompatibly for two consecutive
  minor releases.
- The controller serves a conversion webhook, and an upgrade test converts
  stored objects from the previous version.
- An ADR records the move and the beta deprecation period.

From then on, a field is not removed or renamed, and its meaning does not
change, within a version. An incompatible change needs a new version, served
beside the old one with conversion, and the old version stays served for the
deprecation period that ADR sets.

## What a release contains

| Asset | Contents |
| --- | --- |
| CLI archives | `sproozi` and its license. `sproozi version` prints the embedded native image. |
| Source bundle | The tagged source with vendored modules and the embedded native image, for Homebrew and `sproozi images build`. |
| `images.json` | Native and control image references, per-platform digests, client versions, helper contract and recipe hash. |
| `sproozi_VERSION_kubernetes.tar.gz` | Extracts to `sproozi-release/`: `crds.yaml`, the digest-pinned `install/`, the optional certificate controller in `self-managed-ca/`, the `quickstart-self-managed-ca/` and `quickstart-external-ca/` overlays, `first-check/`, the CLI's `cli-access/`, a `kind/` cluster configuration and a `README.md` that links the [Kubernetes guide](../guides/install-kubernetes.md) at the release's tag. |
| `metadata.json` | GoReleaser's record of the version, commit and date the assets were built from. |
| `checksums.txt` | SHA-256 of the archives, the source bundle, `images.json` and the Kubernetes assets. |

The archives install nothing by themselves. `sproozi init` pulls the
native image; the [Kubernetes guide](../guides/install-kubernetes.md) applies
the Kubernetes assets.

Standalone binaries are unsigned, and a browser download on macOS is
quarantined. Homebrew pours bottles without quarantine. Add signing and
notarization before distributing macOS binaries by other means.
