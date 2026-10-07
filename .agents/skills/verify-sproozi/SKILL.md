---
name: verify-sproozi
description: Verify Sproozi's Kind getting-started guide, cluster dependencies, AgentRun-to-PR demo, exploration and cleanup through the documented CLI paths.
---

# Verify the Sproozi Kind quickstart

Scope is [Getting started](../../../docs/guides/getting-started.md). Read the
[feature map](features/README.md), then choose the requested path. Existing-cluster
installation is outside this skill. Use the repository helper from its root.
Use the dependency path by default. Choose the provider-backed demo when that
acceptance is requested and its disposable repository and credentials are configured.

## Launch

Run the dependency path without provider credentials:

```sh
python3 hack/verify/quickstart.py doctor
python3 hack/verify/quickstart.py dependencies
```

For the full demo, finish the guide's repository, GitHub App and credential
wizard steps first. Use a configured environment with absolute credential-file
paths. The following creates a real GitHub branch and PR and consumes model usage:

```sh
python3 hack/verify/quickstart.py doctor --env-file "$PWD/.local/demo.env"
python3 hack/verify/quickstart.py demo --env-file "$PWD/.local/demo.env"
```

Use `--keep-cluster` on `demo` when exploration or cancellation is in scope.
A successful dependency gate proves controller deployment and NetworkPolicy
enforcement. A full demo passes only with a passing live report containing the
PR, denial probes and run cleanup. Model and GitHub failures remain failures.

The helper copies the current public sources into an isolated Git checkout.
It uses unique cluster names, images and kubeconfigs. Its `driver.lock` serialises
the existing gate's shared Docker image tag and the demo's fixed local registry.
Retained demo clusters keep that lock until cleanup. Run other demo commands
after this verification finishes.

ChatGPT sessions have one refresh owner. Use a newly authorised session for this
cluster; establish that it is unused by any existing gateway before launching.
The skill never performs browser consent or retrieves credentials from other clusters.

## Doctor

`doctor` reads host tool availability, Docker health and versions. With
`--env-file`, it also checks credential presence and host gh authentication.
It does not run setup, inference or credential rotation. kubectl must support the
guide's pinned Kubernetes 1.33.1 node image.

For a retained cluster, set `SPROOZI_QUICKSTART_EVIDENCE` to the exact directory
printed by launch and check its identity and API:

```sh
python3 hack/verify/quickstart.py doctor --evidence "$SPROOZI_QUICKSTART_EVIDENCE"
```

## Drive

Use the recipes in [dependencies](features/dependencies.md),
[demo](features/demo.md) and [exploration](features/exploration.md).
The helper runs the repository's existing `make verify-kind` and `make demo`
paths. Interactive GitHub App creation and model consent remain human setup
steps; the helper reuses configured inputs. Inspect the guide whenever its
commands or prerequisites change. Do not substitute the existing-cluster guide
or an internal fake-client test for a mapped Kind feature.

## Evidence

Each invocation prints its evidence directory under
`.local/verification/quickstart/`. Retain:

- `commands.log`, with the commands, output and failures from the real gates.
- `result.json`, with feature coverage, source revision, guide digest, tool
  versions and cleanup status. `not_run` is not a passing observation.
- `source-files.json`, with hashes of the current sources actually copied.
- `crds-observed.json`, with CRDs seen in the owned cluster during the gate.
- `live.json` for the full provider-backed demo, including PR and denial evidence.

The dependency proof must observe Sproozi's four CRDs and Calico's CRDs, with no
external Agent Sandbox, cert-manager or monitoring CRDs. Sproozi creates ordinary
Pods. No external Sandbox controller, gVisor or Kata runtime is installed.

The existing-cluster path, interactive setup and later exploration are unassessed
unless actually driven. Add an `exploration.log` and `exploration.json` for the
extra recipe, recording actions and observed end states. Private kubeconfigs and
transcripts stay under `.local`; provider environment files and TLS private keys
remain in scratch and are removed during cleanup.

## Cleanup

The helper cleans up on success, failure and normal interruption. When a full
demo was retained for exploration, or recovery is needed, use its exact directory:

```sh
python3 hack/verify/quickstart.py cleanup "$SPROOZI_QUICKSTART_EVIDENCE"
```

Cleanup checks the recorded owner, removes only that cluster and scratch
checkout, then records `clusterRemoved` and `scratchRemoved`. Confirm the evidence
files still exist afterwards. It leaves the GitHub PR, branch, host registry and
operator-owned credential files for review. Close or delete GitHub artefacts only
when that action is requested. An uncleanable cluster makes the verification fail.

## Helpers

`hack/verify/quickstart.py` is executable and supports `doctor`, `dependencies`,
`demo` and `cleanup`; run `python3 hack/verify/quickstart.py --help` for arguments.
Use `/maintain-verification-skill` when guide or feature changes need reconciliation
with this map.
