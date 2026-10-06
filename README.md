# Sproozi

Sproozi runs bounded agent work in disposable Kubernetes Pods. A trusted shared
gateway checks each run's identity, capabilities and live policy, and supplies
upstream credentials. The sandbox receives no provider credentials.

## Run the manual demo

Start with the [manual AgentRun-to-PR guide](docs/guides/manual-pr-demo.md).
It configures a disposable Kind cluster, a digest-pinned Codex image and real
provider credentials, then verifies a proposed PR, explicit denials and cleanup.

[Recorded acceptance](docs/demos/verified-sre-demo.md) passed twice on 4 October
2026. A stock agent investigated a deliberately failing workload and proposed
a one-line correction in a real PR. These were dirty-working-tree semantic
endpoint runs. They establish neither deployed recovery nor release readiness.

## Core concepts

- `AgentRuntime` fixes the administrator-owned Pod template and gateway endpoint.
- `AgentPolicy` bounds requested capabilities, resource scope, consumption and lifetime.
- `AgentTemplate` selects one runtime and policy and supplies trusted instructions.
- `AgentRun` requests one execution with immutable task, event and capabilities.

The current adapter creates Pods in `sproozi-agents`. Kubernetes Agent Sandbox
integration is planned. The demonstrated runtime uses one completion container
named `agent`; arbitrary container names and long-running sidecars do not have
working completion semantics yet.

## Boundaries and limitations

| Boundary | Current behavior |
| --- | --- |
| Pod security | Runtime validation requires non-root execution, dropped capabilities, RuntimeDefault seccomp, a read-only root and bounded writable volumes. |
| Identity | Each run has a dedicated ServiceAccount without direct Kubernetes grants. The gateway checks live run identity and policy. |
| Network | Policy grants DNS and gateway egress. An enforcing CNI and actual direct-access probes are required; node-traffic exceptions prevent a blanket isolation guarantee. |
| Transport | Standard clients use one authenticated inspected-TLS proxy. Semantic handlers check Kubernetes resources and GitHub repository/branch scope. |
| Budgets | Kubernetes-backed accounting survives gateway restarts. Model reservations can underestimate total usage, so token and cost limits are not hard provider-spending ceilings. |
| Cleanup | Recorded runs removed their sandbox resources. Interrupted provisioning can leave resources when status writes are lost; budget ConfigMaps have no automatic retention cleanup. |
| Audit | The gateway logs redacted decisions. Operators supply durable storage and review proposed changes before merging. |

See [security hardening](docs/reference/security-hardening.md) for deployment
controls and known implementation limits. This is a preview for evaluation in
a disposable cluster.

## Documentation

The [documentation index](docs/README.md) links the guides, API references and
architecture explanations. The [home-ops playbook](docs/guides/home-ops-playbook.md)
and [runbook](docs/guides/home-ops-demo-runbook.md) are retained plans for the
next integration; their alert-to-PR and recovery path has not been accepted.
