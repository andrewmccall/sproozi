# System architecture

**Audience:** operators, contributors

Sproozi separates untrusted agent execution from trusted policy and credential
handling. CLI, HTTP and MCP clients use one shared authenticated gateway. Its
native service handlers enforce Semantic scope, the configured MCP bridge
provides Protocol mediation, and the destination handler checks exact network
destinations. These tiers describe the broker's understanding of an operation.

```mermaid
graph LR
    subgraph Cluster
        subgraph System[sproozi-system]
            C[controller-manager]
            GW[shared authenticated gateway]
            M[semantic model module]
            G[semantic service modules]
            MCP[configured MCP bridge, Protocol]
            E[destination module]
            W[webhook]
            CR[(Trusted Secrets and configuration)]
        end

        subgraph Agents[sproozi-agents]
            S[Sandbox with stock agent]
            Contract[Immutable run contract and native MCP configuration]
        end

        subgraph Target[Approved target namespace]
            D[Cluster resources]
        end

        R[AgentRun]
        T[AgentTemplate]
        P[AgentPolicy]
        RT[AgentRuntime]
    end

    A[Operator or future event producer] --> R
    A -. optional signed webhook .-> W
    W -. optional .-> R
    R --> C
    T --> C
    P --> C
    RT --> C
    C --> S
    C --> Contract
    Contract -. mounted configuration .-> S
    S -->|CLI, HTTP and MCP through inspected TLS| GW
    GW --> M
    GW --> G
    GW --> MCP
    GW --> E
    G -. native Kubernetes REST and MCP reads .-> D
    MCP --> Providers[Approved remote HTTPS MCP servers]
    CR --> M
    CR --> G
    CR --> MCP
    CR --> E
    CR --> W
```

## Trust and namespace boundaries

- `sproozi-system` holds the trusted controller, webhook and shared gateway,
  including Semantic, Protocol and Destination handlers and provider credentials.
- `sproozi-agents` holds run-scoped identities, network policies, immutable
  contracts and untrusted sandbox workloads.
- Approved target namespaces such as `sproozi-demo` hold resources the run may inspect.
- Task text, event payloads and provider output are untrusted. Tool discovery
  describes an interface; trusted registration and policy determine authority.

## External-call invariant

With an enforcing CNI and the generated NetworkPolicy, external calls from a
workload, including Kubernetes API and MCP calls, go through the shared gateway.
The generated policy grants only DNS and gateway egress. Operators must verify
direct API/provider denials in their environment; NetworkPolicy alone has
node-traffic exceptions. Routing selects a handler. The handler authorizes the
actual operation against immutable requested capabilities and current
lifecycle/policy state.

Each run gets one dedicated ServiceAccount. Its gateway-audience token identifies
the run but does not grant capabilities. The workload Pod has no Sproozi proxy
sidecar or local proxy process. Provider credentials remain in the trusted gateway.

## MCP is a supported capability interface

| Path | Run grant | Trusted configuration | Enforcement |
| --- | --- | --- | --- |
| Native Kubernetes Pod-list tool at `https://kubernetes.default.svc/mcp` | `kubernetes.read` | Existing Kubernetes namespace/resource policy | Semantic, through the same handler as native REST reads |
| Registered remote tools at the gateway's `/mcp/<server>` path | `mcp.<server>` | Gateway registry plus `AgentPolicy.spec.mcpServers` tool rules and optional argument restrictions | Protocol, using discovered input schemas and independent policy validation |

Adding a compatible remote HTTPS server requires registration and policy, without
a provider-specific Go adapter. The gateway snapshots exact provider URLs and
optional bearer credential files at startup. Registration or credential changes
require a gateway restart; policy remains live. Provider authorities are reserved
against weaker destination routing.

For each incoming remote MCP request, the bridge initializes a private upstream
SDK session, discovers and filters approved tools, executes at most one call and
closes the session. Budgets charge attempted tool invocations under the Run UID
and named capability; reconnecting does not reset consumption. Cached discovery
and connection configuration grant no authority. The proxy's live revalidation
revokes requests when run or policy authority ends.

The controller renders native Codex, Claude Code, OpenCode or Hermes MCP configuration
in the immutable run contract. The administrator-owned launch loads it into
fresh client state, with no upstream URLs or credentials. Claude Code's native
Anthropic model route shares the existing `model.inference` account with OpenAI;
the trusted gateway selects its protocol and credentials. The [harness reference](../reference/harnesses.md)
and [harness ADR](../adr/0002-native-cli-harnesses-and-model-protocols.md)
describe launch, completion, accounting and verification limits.

The [MCP ADR](../adr/0001-mcp-capabilities-through-shared-gateway.md) records why
registration, grants and session ownership have this shape. See
[configured MCP capabilities](../reference/configured-mcp.md),
[native Kubernetes MCP delivery](../reference/kubernetes-mcp.md) and
[the agent contract](../reference/agent-contract.md) for current configuration and
limits. The configured bridge supports independent HTTPS tool operations;
managed stdio, OAuth consent and conversation continuity remain future work.

## Persistent assistant delegation

Stock Hermes runs separately with native sessions, memory and schedules on a PVC.
Its native MCP client calls a private task service. That service owns narrowly
scoped Kubernetes credentials and maps workflow names to fixed templates and
capability subsets. The assistant supplies task intent, bounded replay identity
and UID-bound observation/cancellation; it holds no Kubernetes token.

The controller admits and executes each delegated AgentRun through the existing
worker queue and shared gateway. It retains optional explicit final text with
terminal status before deleting the worker Pod. Conversations, schedules and
coordinator inference belong to Hermes; worker identity and budgets belong to
Sproozi. The coordinator does not occupy the single worker slot. See
[the task interface](../reference/orchestration.md),
[the Kubernetes guide](../guides/hermes-orchestration.md) and
[ADR 0003](../adr/0003-persistent-assistants-and-retained-results.md).

## Execution and evidence

The execution adapter creates Pods, not external Agent Sandbox custom resources.
A run receives an immutable input contract, one projected gateway-audience token,
a ServiceAccount without direct Kubernetes grants, and a run-scoped NetworkPolicy.
The token identifies the run; requested capabilities and live policy determine
its authority. New gateway requests recheck that authority, and open sessions
close through bounded revalidation.

[Recorded SRE acceptance](../demos/verified-sre-demo.md) demonstrated the Semantic
Kubernetes, model and GitHub paths in Kind on 4 October 2026.
[Recorded MCP acceptance](../demos/verified-mcp-demo.md) demonstrated stock Codex
use of native Kubernetes MCP and two configured HTTPS fixture providers on
6 October 2026, including denials, budgets, cancellation and normal cleanup.
These dirty-tree runs establish neither universal provider compatibility nor
release reproducibility or home-ops integration. Current model admission,
interrupted cleanup and container-completion limitations are listed in
[security hardening](../reference/security-hardening.md).

## Managed native session continuity

The home daemon starts controlled Docker sessions without Kubernetes credentials.
Its coordinator owns one bounded execution's native UUID, selected checkpoints,
current location, original absolute deadline and remaining escrow. Each visit
has a fresh allocation and exact execution incarnation. A fixed TLS profile
owns remote admission and Kubernetes credentials; each remote visit creates a
new AgentRun under live controller policy.

For [remote-first work](../guides/portable-workflows.md), the profile additionally
hosts a task catalogue independent of the laptop. It admits an approved Codex
task, observes genuine completion, fences/seals the worker and retains stopped
native/Git state under a stable product session ID. Authenticated MCP lets Hermes
submit and inspect work. Pull commits a one-way claim to the home; later resume
starts a separately authorized execution with the same native UUID, preserving
old receipts. Product operations bind to their original execution so delayed
retries cannot affect a newer continuation. See
[ADR 0012](../adr/0012-retained-work-and-fresh-session-continuations.md).

```mermaid
flowchart LR
    CLI[Native terminal client] --> Home[Home daemon and session journal]
    Home --> Docker[Controlled Docker placement]
    Home --> Profile[Authenticated fixed remote profile]
    Profile --> Run[Fresh Kubernetes AgentRun]
    Docker --> LocalGateway[Local model gateway and accounts]
    Run --> Gateway[Shared gateway and managed accounts]
    Docker --> Checkpoint[Selected native conversation and Git workspace]
    Run --> Checkpoint
    Checkpoint --> Home
```

Checkpoint contents supply untrusted continuity, never spending authority.
Placements regenerate configuration and access independently. A graceful move
gates input, waits for the native turn and tools, revokes authority, observes the
entire source writer stopped, seals accounts and captures state. Only then may a
new destination activate. Forced cancellation revokes first and needs no fresh
checkpoint. Unknown stop or accounting retains escrow and blocks activation.

Deterministic stock Codex 0.161.0 and Claude Code 2.1.27 proofs completed native
terminal roundtrips across Docker, Kubernetes and Docker with unchanged UUID,
Git state and original deadline. Both executed real tools and passed denied
capability/direct-HTTP probes on both placements. Codex also passed graceful
held-turn movement and forced cancellation with conservative receipt replay.
[Recorded acceptance](../demos/verified-session-mobility.md) separates those checks
from remaining native-history, platform and provider compatibility limits.
The [session guide](../guides/session-mobility.md) describes the narrow evaluated
contract. [ADR 0010](../adr/0010-managed-session-authority-and-admission.md) records
authority, stop and recovery tradeoffs.

Runtime implementations live in `internal/runtime/kubernetes` and
`internal/runtime/docker`. Shared harness, checkpoint and workspace code stays
independent of these backends. Kubernetes budget persistence lives in
`internal/budget/kubernetes`; shared accounting rules live in `internal/budget`.
Policy, gateway authentication, model credential storage and task services place
their Kubernetes implementations in `kubernetes` children too. Controller/API
scaffold paths remain unchanged. See
[ADR 0009](../adr/0009-runtime-backends-and-sealed-accounting.md).

## Future architecture

The [future architecture overview](future-architecture/README.md) retains managed
MCP servers, further native sessions, GUI/IDE clients, arbitrary-session adoption
and enterprise profile extensions. Those directions require separate proofs.
