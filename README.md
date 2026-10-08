# Sproozi

Sproozi runs bounded agent work in disposable Kubernetes Pods. A trusted shared
gateway checks each run's identity, capabilities and live policy, and supplies
upstream credentials. The sandbox receives no provider credentials.

## Get started

Follow the [getting started guide](docs/guides/getting-started.md) to explore
Sproozi in a disposable Kind cluster. It walks through tool installation,
GitHub repository and App setup, model authentication, running the demo and
inspecting the proposed PR.

For an existing cluster, use [Install on Kubernetes](docs/guides/install-kubernetes.md).
It covers image publishing, gateway certificates, deployment and a first
agent Pod that reads Kubernetes Pods without model or GitHub credentials.

The [manual AgentRun-to-PR guide](docs/guides/manual-pr-demo.md) describes the
demo's acceptance checks and optional integrations.

[Recorded acceptance](docs/demos/verified-sre-demo.md) passed twice on 4 October
2026. A stock agent investigated a deliberately failing workload and proposed
a one-line correction in a real PR. These were dirty-working-tree semantic
endpoint runs. They establish neither deployed recovery nor release readiness.

## MCP capabilities

Sproozi supports MCP through the shared gateway. The native
[Kubernetes MCP tool](docs/reference/kubernetes-mcp.md) lists Pods under the existing
`kubernetes.read` grant and Semantic enforcement. [Configured remote MCP servers](docs/reference/configured-mcp.md)
supply tools under named `mcp.<server>` grants and Protocol enforcement. Register
an approved HTTPS endpoint, select tools and optional argument restrictions in
policy, then request its capability. Sproozi discovers the provider's tool schemas;
adding a compatible server requires configuration, without a provider-specific
Go adapter. Credentials remain behind the gateway.

Codex, Claude Code and OpenCode runtimes receive native broker connections in
the immutable run contract. The [harness reference](docs/reference/harnesses.md)
and [stock runtime examples](examples/harnesses/README.md) describe launch and
provider setup. Claude Code uses Anthropic Messages through the same gateway-owned
credentials and `model.inference` budget. Additional harness evidence is local
and uses mocked providers; the recorded deployed proofs remain Codex-specific. [Recorded MCP acceptance](docs/demos/verified-mcp-demo.md) proves stock
Codex use of the native tool and two configured HTTPS fixture providers, including
denials, request budgets, cancellation and cleanup. Managed stdio and OAuth
consent are future work. The [MCP ADR](docs/adr/0001-mcp-capabilities-through-shared-gateway.md)
records the architectural choice and trade-offs.

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
| Transport | CLI, HTTP and MCP clients use one authenticated inspected-TLS proxy. Native handlers enforce service scope; the configured MCP bridge filters approved tools and validates argument schemas. |
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
