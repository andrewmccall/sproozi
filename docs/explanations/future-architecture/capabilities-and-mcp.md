# Capabilities and MCP interoperability

**Status:** future direction. See the [overview](README.md) for the current
foundation and shared invariants.

MCP should make an approved capability usable by an existing harness. Sproozi
still owns the execution that receives it, its policy, identity and lifetime.
An MCP endpoint alone does not describe which workload may use it, what scope
it has or when its authority ends.

The useful demonstration is an agent receiving a task and a bounded capability,
then finding the corresponding tools in its disposable environment. Sproozi
prepares the connection and enforces the grant. The user should not need to copy
provider credentials into the agent or maintain a separate global MCP setup.

## Preserve Known, Proxy and Generic

The earlier design vocabulary describes how much Sproozi understands about an
operation. The public docs use Semantic, Protocol and Destination for the
implemented enforcement model. Preserve the relationship without introducing a
second set of API capability names.

| Design vocabulary | Meaning | Relationship to current docs |
| --- | --- | --- |
| Known | Enforce service resources and actions, such as observing one Kubernetes namespace or opening a PR in one repository. | Semantic. Native handlers already provide selected integrations. An MCP-backed implementation could provide another delivery path. |
| Proxy | Mediate a bounded command or protocol request while understanding less about the target service. | Protocol. A standalone bounded-request integration is planned; protocol parsing within a semantic handler does not itself establish this tier. |
| Generic | Grant explicitly bounded access where richer mediation is unavailable, with the reduced guarantee visible. | Destination is the current network form. Broader filesystem, volume or direct credential delegation would need a separate design and explicit policy. |

These are enforcement tiers, not transports, increasing permission levels or
three separate execution architectures. One run can combine them within its
declared envelope. `kubectl` and `gh` are CLI clients but the current gateway
understands their permitted operations. Conversely, an MCP call can still be
only a protocol-level grant.

Use [capabilities and gateways](../capabilities-and-gateways.md) for the current
module map and [permissions and capabilities](../../reference/permissions-and-capabilities.md)
for current capability names and scope. This direction does not add arbitrary
MCP capability strings to the existing API.

## MCP delivers capabilities; it does not establish semantics

A recognised tool name and JSON argument schema are useful inputs to mediation.
They do not establish that an operation is safe, read-only or confined to the
claimed resource. A Known grant needs a trusted mapping from the tool and its
arguments to enforceable service scope, backed by the provider's behaviour and
upstream restrictions. An unfamiliar MCP server does not become Known merely
because its tools have descriptive names.

For example, a Postgres MCP tool named `query` does not establish read-only
access. A restricted database role, bounded target and reviewed server behaviour
would need to support that claim. Otherwise the grant should describe the
weaker request boundary that Sproozi can actually enforce. The same reasoning
applies to a generic tool that accepts arbitrary shell commands or URLs.

New integrations may begin with less semantic understanding and later become
Known. That is an explicit change to an approved grant, not automatic fallback.
A missing semantic adapter, unsupported operation or failed MCP connection must
not silently become generic access to the target. The current gateway already
reserves semantic service addresses against weaker destination fallback.

## Directional preparation flow

1. Resolve the run's requested capabilities against trusted policy and scope.
2. Select an approved implementation and record the enforcement tier it provides.
3. Prepare the service connection or managed server, its identity and permitted
   upstream access outside the agent's authority.
4. Render only the approved connections into the selected harness's run-local
   configuration, then launch the workload.
5. Authenticate and authorise actual requests, account for configured consumption,
   and record redacted decisions.
6. Revoke access and release run-owned configuration and server resources when
   execution ends or policy removes the grant.

The generated configuration is delivery information. The trusted enforcement
point must reject a forged or out-of-scope call even if the agent modifies its
configuration or invokes the endpoint directly. Filtering discovery is useful
for usability; it is not the authorisation boundary. Revocation must apply to
active sessions as well as future connections.

## Reuse providers without building an integration catalogue

There are several useful deployment choices, each subordinate to the same
execution policy.

- A Sproozi capability can expose an existing native semantic operation through
  MCP while retaining ordinary CLI/HTTP delivery.
- An approved remote MCP service can provide operations. Its credentials and
  service policy remain behind a trusted boundary; Sproozi must establish how
  run scope is enforced before presenting a Known guarantee.
- A managed MCP server can provide a database or internal integration. Sproozi
  controls its preparation, credentials, network access and lifetime as a
  separate workload.
- An existing MCP gateway can supply transport or server-management machinery
  if it preserves run identity, policy checks, revocation and audit attribution.

A credential-bearing managed server is a separate trust boundary. Placing it in
the agent's Pod as an ordinary sidecar does not give it separate network
isolation or authority. The current [AgentRuntime](../../reference/agentruntime.md)
shares run authority across Pod containers. Server reuse is possible only with
explicit isolation between runs and clear ownership of sessions and cleanup.
Sproozi should not start by designing a general server scheduler.

MCP offers integration breadth. OCI-packaged CLIs and SDKs should remain useful
for internal or proprietary tooling, and selected native adapters should add
semantics where they earn their maintenance cost. Installing any of these tools
grants no external authority. A direct secret grant, if ever supported as a
Generic escape hatch, must state that agent code can read it. It is a weaker,
explicitly authorised mode, not today's credential-isolated default.

## Keep harness configuration at the edge

MCP connection configuration belongs in the [harness adapter](harness-portability.md).
Do not require every harness to read one Sproozi tool manifest. Keep an internal
description of approved connections and render the selected harness's supported
configuration at launch. Tool discovery and execution then use MCP.

The eventual adapter may need remote HTTP, local stdio or a small bridge. Choose
the transport for an actual harness and deployment. A stdio process running
under the agent's identity cannot safely hold broader upstream credentials.
Run authentication material may be visible to the workload, just as today's
gateway token is; that token must grant only the active run's approved authority.
Provider secrets should not appear in generated harness configuration.

Preserve ordinary clients and the [current client contract](../../reference/client-compatibility.md).
MCP should widen the ways an execution can use a capability, not require every
shell, filesystem or network action to become an MCP tool call.

## First proof and open decisions

Start with one integration in the existing Kubernetes workload. Verify that the
agent discovers and uses the allowed operation, an unlisted or out-of-scope call
fails, direct upstream access fails, and cancellation removes authority. Record
which tier was actually enforced. A second harness should consume the capability
through its own configuration without changing the grant.

A managed Postgres example would be a useful later test of credential separation,
server lifecycle and upstream-native restrictions. An arbitrary OCI CLI would
test the escape hatch without requiring a native connector for every service.
Neither needs to be part of the first MCP proof.

The implementation should settle where enforcement lives, how remote sessions
carry run identity, which server behaviour supports Known mappings, and who owns
managed-server cleanup. Consumption units will differ between services; MCP does
not supply one universal budget model. Existing
[endpoint accounting](../capabilities-and-gateways.md#endpoint-interface-and-spending)
is a starting point, with its documented limits.

Success means a bounded execution can use an ecosystem capability and explain
its actual guarantee. It does not require a server marketplace, dozens of native
connectors or a standalone MCP broker product.
