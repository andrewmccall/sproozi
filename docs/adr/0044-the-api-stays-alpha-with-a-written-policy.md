# 0044: The API stays alpha, with a written compatibility policy

Status: accepted
Date: 2026-10-21

## Problem and constraints

The release guide promised that version 1.0 would promote the CRDs to
`v1beta1` or `v1` "with conversion and a deprecation period". A review read
that as an unkept promise and a blocker for 1.0. Nothing said what the alpha
API guarantees today, or when it would stop being alpha.

The CRDs still change often. On 2026-10-21 alone, AgentRun gained a name
length rule and a new terminal reason, and task limits were aligned. No
installation holds stored objects: the only release, v0.1.0, was a test
release and has been deleted, so a version change now would need no
conversion.

## Decision

- **Stay on `sproozi.com/v1alpha1`.** One served and stored version, no
  conversion webhook.
- **Write the alpha rules down** in
  [versioning and compatibility](../contributing/releases.md#the-alpha-api):
  a minor release may change the CRDs incompatibly, with a `BREAKING CHANGE:`
  footer naming the field and the operator's steps; a patch release never
  changes them; changes are additive where they can be; and the guide gives
  the cross-version upgrade steps. The existing release guard already refuses a
  release that changes `api/` or `config/` without that footer.
- **Set criteria for beta.** Move to `v1beta1` after two consecutive minor
  releases with no incompatible CRD change, once the controller serves a
  conversion webhook that an upgrade test exercises, and with an ADR that sets
  the beta deprecation period.
- **Drop the 1.0 promise.** Sproozi stays at `0.x` while the API is alpha.

## Alternatives

- **Move to `v1beta1` now.** With no stored objects, the rename would be
  mechanical: the version is in 121 Go import paths and 30 manifests. It was
  rejected because beta forbids removing, renaming or changing a field within a
  version, so every such change would need a new version and a conversion
  webhook. At the current rate of change that cost lands every release.
- **Go straight to `v1`.** The same cost, with a longer deprecation commitment.
- **Keep the promise and add nothing.** Leaves users without rules today and
  keeps an open-ended commitment.

## Consequences

- Operators know what an upgrade may change and what they must do, and the
  release guard enforces the footer.
- Users must pin a release and read its notes before crossing a minor
  version; there is no automatic migration of stored objects.
- Moving to beta later needs a conversion webhook and an upgrade test, which
  do not exist yet.

## Revisit when

- Two consecutive minor releases ship with no incompatible CRD change.
- A user needs in-place upgrades across minor versions without editing stored
  objects.

## Links

- [Versioning and compatibility](../contributing/releases.md#versioning-and-compatibility)
- `hack/release/publish.py` (`require_breaking_record`)
- `api/v1alpha1/`
