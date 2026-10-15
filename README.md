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

Codex, Claude Code, OpenCode and Hermes runtimes receive native broker connections in
the immutable run contract. The [harness reference](docs/reference/harnesses.md)
and [stock runtime examples](examples/harnesses/README.md) describe launch and
provider setup. Claude Code uses Anthropic Messages through the same gateway-owned
credentials and `model.inference` budget. The full Kind E2E launches all four
stock clients with deterministic provider fixtures.
[Recorded MCP acceptance](docs/demos/verified-mcp-demo.md) proves stock
Codex use of the native tool and two configured HTTPS fixture providers, including
denials, request budgets, cancellation and cleanup. Managed stdio and OAuth
consent are future work. The [MCP ADR](docs/adr/0001-mcp-capabilities-through-shared-gateway.md)
records the architectural choice and trade-offs.

## Persistent assistant orchestration

Run stock Hermes in a separate Kubernetes Deployment with native conversation
state and schedules on a PVC. Its native MCP client submits fixed administrator
workflows to Sproozi and reads bounded worker answers retained in AgentRun status
after Pod cleanup. Worker jobs use the existing global queue. Coordinator
inference and channel credentials belong to the trusted assistant deployment.

The [Hermes guide](docs/guides/hermes-orchestration.md) includes full Kind testing,
existing-cluster setup, native API chat and schedules. `make test-e2e` launches
the complete stack in an isolated Kind cluster with an enforcing CNI; no paid
provider credentials or external bot channels are required.

## Core concepts

- `AgentRuntime` fixes the administrator-owned Pod template and gateway endpoint.
- `AgentPolicy` bounds requested capabilities, resource scope, consumption and lifetime.
- `AgentTemplate` selects one runtime and policy and supplies trusted instructions.
- `AgentRun` requests one execution with immutable task, event and capabilities.

The current adapter creates Pods in `sproozi-agents`. Kubernetes Agent Sandbox
integration is planned. The demonstrated runtime uses one completion container
named `agent`; arbitrary container names and long-running sidecars do not have
working completion semantics yet.

## Portable native sessions

Build the public CLI with `go build -o bin/sproozi ./cmd/sproozi`. Use
`sproozi serve` to run the home daemon, then `sproozi start`, `sproozi attach`
and `sproozi move` to work across local Docker and an approved cluster profile.

The [retained-work guide](docs/guides/portable-workflows.md) adds remote-first
Codex tasks through the CLI or authenticated MCP. The remote owner can accept
work while the laptop is offline. Use `sessions`, `pull`, `resume` and `stop` to
continue the genuine native conversation locally under a fresh bounded grant.
The [session command reference](docs/reference/session-commands.md) describes
explicit owner selection, the labelled catalogue and stable session/execution IDs.
The guide includes scoped home-ops setup and one shared external MCP definition.
Home-ops acceptance against personal infrastructure and paid-model engineering
contributions remain unassessed.

The [session guide](docs/guides/session-mobility.md) describes controlled local
Docker launch, native terminal attachment and moves through a fixed remote
Kubernetes profile. The developer needs no kubeconfig. Each visit has fresh
authority within the original deadline and remaining allowance.

Deterministic stock Codex 0.161.0 and Claude Code 2.1.27 fixtures passed the
local-to-remote-to-local roundtrip with the same native UUID, original deadline,
Git history, staged/unstaged changes, untracked files and explicitly selected
ignored files. Both executed real tools and passed local/remote network and
capability denials. Codex also passed held-turn movement, forced cancellation
and conservative accounting replay. [Recorded acceptance](docs/demos/verified-session-mobility.md)
keeps their proof coverage and current limits separate. Existing-session adoption,
GUI/IDE attachment and other managed clients remain future work.

## Boundaries and limitations

| Boundary | Current behavior |
| --- | --- |
| Pod security | Runtime validation requires non-root execution, drop ALL with no added capabilities, RuntimeDefault seccomp and a read-only root. Template volumes and environment sources are explicitly restricted. |
| Identity | Each run has a dedicated ServiceAccount without direct Kubernetes grants. The gateway checks live run identity and policy. |
| Network | Policy grants DNS and gateway egress. An enforcing CNI and actual direct-access probes are required; node-traffic exceptions prevent a blanket isolation guarantee. |
| Transport | Both backends bound connections and sessions per execution, with deadlines for authentication and live authority checks. Native handlers enforce service scope; configured MCP validates approved tools and arguments. |
| Budgets | Kubernetes-backed accounting survives gateway restarts. Model reservations can underestimate total usage, so token and cost limits are not hard provider-spending ceilings. |
| Cleanup | Release recovers deterministic resources after missing status writes and retains containment for unknown Pod state or ownership. Managed writer fencing remains separate; budget ConfigMaps have no automatic retention cleanup. |
| Audit | The gateway logs redacted decisions. Operators supply durable storage and review proposed changes before merging. |

See [security hardening](docs/reference/security-hardening.md) for deployment
controls and known implementation limits. This is a preview for evaluation in
a disposable cluster.

## Documentation

The [documentation index](docs/README.md) links the guides, API references and
architecture explanations. The [home-ops playbook](docs/guides/home-ops-playbook.md)
and [runbook](docs/guides/home-ops-demo-runbook.md) are retained plans for the
next integration; their alert-to-PR and recovery path has not been accepted.

[Future architecture](docs/explanations/future-architecture/README.md) covers
remaining extensions, including more session harnesses, arbitrary-session
adoption, GUI/IDE attachment and managed MCP servers.
