# Kind quickstart verification map

This map covers the user paths in
[Getting started](../../../../docs/guides/getting-started.md). It deliberately
excludes installation on an existing cluster.

## Baseline preconditions

- Use the guide's host tools and a kubectl version compatible with Kubernetes 1.33.1.
- Run commands from the repository root and pass a configured local environment
  explicitly for the full demo.
- Use the helper's owned cluster and kubeconfig. The operator's default context
  is never a verification target.
- Read `result.json` before interpreting a run. Dependency proof alone leaves
  provider-backed execution and exploration unassessed.

## Features

- [Cluster dependencies](dependencies.md), controller deployment, CRDs and actual NetworkPolicy enforcement.
- [AgentRun to pull request](demo.md), the configured Codex run, real PR, denial probes and sandbox cleanup.
- [Exploration and cancellation](exploration.md), guide commands against a retained demo cluster and a second manual run.

The map lists coverage to maintain, not a claim that all entries have passed.
Proof includes the action, resulting state and side effects. Keep evidence after
cleanup and identify failed or unreachable paths explicitly.
