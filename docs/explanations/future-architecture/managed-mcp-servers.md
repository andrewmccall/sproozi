# Managed MCP servers and further semantic bindings

**Status:** future direction. Native Kubernetes and configured remote HTTPS MCP
are complete. See the [current MCP references](../../reference/configured-mcp.md)
and [ADR 0001](../../adr/0001-mcp-capabilities-through-shared-gateway.md).
This document covers only extensions not implemented by those paths.

## Managed servers need separate authority and lifecycle

A managed MCP server could expose a database or internal service while Sproozi
owns its preparation, upstream credentials, network access and lifetime. A
credential-bearing server needs isolation from the untrusted agent. Putting it
in the same Pod as an ordinary sidecar does not establish separate network
authority; the current [AgentRuntime](../../reference/agentruntime.md) shares
run authority across its containers.

Before adding a managed server, establish:

- Which execution owns its resources and when creation, cancellation and
  cleanup occur, including recovery after partial provisioning.
- How provider credentials remain inaccessible to agent code, including with
  stdio delivery. A process under the agent's identity cannot safely hold broader
  upstream authority.
- Whether a server can be reused between executions without sharing sessions,
  credentials or scope, and how live revocation closes active connections.
- How transport adapters retain run attribution and accounting. Existing
  request-owned upstream sessions do not promise conversation continuity.

Reuse existing server-management or gateway implementations if they preserve
these boundaries. Choose one concrete deployment before designing a general
server scheduler. OAuth consent, token storage and renewal require an explicit
trusted ownership design; they are outside current remote HTTPS configuration.

## Service-specific permission checks

The [current enforcement tiers](../capabilities-and-gateways.md) remain
Semantic, Protocol and Destination. MCP transport alone does not provide
service-specific permission checks. A recognized tool name and an input schema
do not establish read-only behavior or service-resource scope.

A future binding must map tool arguments to permitted service actions and
resources. Provider review and upstream restrictions must support that mapping.
For example, a Postgres `query` tool needs a restricted database role and
bounded target before claiming read-only access. Otherwise describe the weaker
boundary actually proved.

Unsupported semantics or an unavailable server must fail rather than fall back
to broader destination access. Direct credential delegation, if later required,
would be an explicitly weaker mode in which agent code can read the secret. It is
not supplied by the current gateway-isolated credential model.

## Testing requirements

See [managed MCP server requirements](../../../test/acceptance/README.md#managed-mcp-servers)
for credential isolation, service restrictions, revocation and cleanup checks.
