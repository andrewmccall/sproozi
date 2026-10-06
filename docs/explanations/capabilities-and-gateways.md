# Capabilities and gateways

**Audience:** operators, contributors

Sproozi does not treat permissions as a single allowlist. A capability becomes usable only when multiple layers agree.

```mermaid
flowchart LR
    Run[AgentRun requested capabilities]
    Policy[AgentPolicy allowed capabilities and bounds]
    Template[AgentTemplate selected egress profiles ]
    Runtime[AgentRuntime shared gateway endpoint and Pod template]
    Gateways[Shared gateway and semantic capability modules]

    Run --> Gateways
    Policy --> Gateways
    Template --> Gateways
    Runtime --> Gateways
```

## Mental model

The broker can enforce three levels of understanding, selected per operation:

| Tier | Enforced boundary | Example |
| --- | --- | --- |
| Semantic | Meaningful service resources and actions | Read pods in one namespace; create a PR from a run-owned branch |
| Protocol | A bounded service interface and request shape | Permit a service's fixed read-only HTTP endpoint |
| Destination | Exact network destinations and protocols | Permit HTTPS to one approved host |

CLI, HTTP proxy and MCP are delivery interfaces. The demo's `kubectl`, `git` and
`gh` calls are semantic requests because the broker checks their resources and
actions. Installing a tool grants no external authority. One run may use
different tiers, always within its statically declared capability envelope.

The [recorded demo](../demos/verified-sre-demo.md) exercised semantic paths. It
did not exercise protocol-only integrations, generic destination access, package
installation or MCP mediation.

- `AgentRun` asks for an explicit capability subset
- `AgentPolicy` says whether that subset is legal
- `AgentTemplate` adds trusted context such as selected egress profiles and instructions
- `AgentRuntime` tells the sandbox where the shared trusted gateway lives
- the gateway authenticates the run, then semantic modules do capability enforcement

## How egress, model, and capabilities interact

### `model.inference`

`model.inference` requires live capability authorization. An optional
`AgentPolicy.spec.budgets[model.inference]` entry adds reservation and settlement
accounting. Model admission can underestimate total consumption; this accounting
is not a hard provider-spending ceiling.

### `network.egress`

`network.egress` only becomes useful when:

1. the run requests `network.egress`
2. the policy lists the profile name
3. the template also selects the same profile
4. the shared gateway destination module approves the exact destination tuple

### `github.pull_request`

The sandbox never gets GitHub credentials. It asks the shared gateway's GitHub
module to publish a PR, and that module validates the run, repository scope, and
revision contract first.

## Why this matters

This split keeps the agent flexible without making prompts the source of truth for authorization. Policy, RBAC, and gateways remain authoritative even if the sandbox is compromised or the task/event text is hostile.

The production story still depends on operator hardening around inspected TLS,
namespace isolation, GitHub-side protections, and durable gateway state. The
recorded semantic runs establish scoped historical acceptance, not production
readiness. See [Security hardening](../reference/security-hardening.md).

## Implementation map

The tiers describe how much a module understands about the permitted operation.
They are not transports and are not increasing permission levels.

| Tier | Module | Status |
| --- | --- | --- |
| Semantic | `internal/endpoints/kubernetes`, `internal/endpoints/github`, `internal/endpoints/model`, `internal/endpoints/packages` | Implemented |
| Protocol | No standalone bounded-request integration | Planned; protocol parsing inside semantic modules does not constitute a separate integration |
| Destination | `internal/endpoints/destination` | Inspected HTTPS to exact profile hosts and ports |

`internal/proxytransport` provides authentication, TLS inspection and session
revocation for all routed tiers. `Dispatcher` in `internal/gateway/dispatch.go` selects an exact
authority and records the route's capability and enforcement tier in its static
definition. Each handler still authorizes the actual operation against the run
and live policy and writes its tier in audit output.

Destination routes are derived from configured egress profiles. The inspection
certificate must cover every configured host. Semantic service addresses are
reserved even when their module is disabled, so generic egress cannot provide a
weaker fallback. Raw CONNECT tunnels, plaintext HTTP and protocol upgrades are
unsupported. Model responses are deliberately buffered for usage verification;
the shared dispatcher itself preserves streaming response behaviour.

## Endpoint interface and spending

The gateway is the shared service. Each implementation lives beneath
`internal/endpoints` in service-specific subpackages and implements
`http.Handler`. There is no plugin lifecycle, custom forwarding interface, or
per-service executable. The dispatcher supplies the authenticated run and an
optional `budget.Meter` in request context; handlers authorize the operation.

`internal/budget` owns the shared accounting contract and its memory and Kubernetes
storage adapters. `Meter.Spend` charges known usage before forwarding;
`Meter.Reserve` returns a reservation for variable usage, which the endpoint
settles or releases. Models translate provider-reported usage and administrator pricing into units
and cost. The current admission estimate can omit input tokens, and rejection
after an over-bound settlement does not undo upstream consumption. Other
endpoints spend one unit
per authorized upstream request, including each GitHub read-back request.

## Future interoperability

[Capabilities and MCP interoperability](future-architecture/capabilities-and-mcp.md)
relates the Known/Proxy/Generic design vocabulary to these enforcement tiers
and describes policy-controlled MCP delivery. It is a future direction; the
current CLI and HTTP paths remain the implemented capability interfaces.
