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

## Semantic bindings require service evidence

The [current enforcement tiers](../capabilities-and-gateways.md) remain Semantic,
Protocol and Destination. MCP delivery does not promote Protocol mediation into
Semantic enforcement. A recognized tool name and an input schema do not establish
read-only behavior or service-resource scope.

A future binding needs a trusted mapping from tool arguments to service authority,
supported by reviewed provider behavior and upstream restrictions. For example,
a Postgres `query` tool needs a restricted database role and bounded target before
claiming read-only access. Otherwise describe the weaker boundary actually proved.

Unsupported semantics or an unavailable server must fail rather than fall back
to broader destination access. Direct credential delegation, if later required,
would be an explicitly weaker mode in which agent code can read the secret. It is
not supplied by the current gateway-isolated credential model.

## Next proof

A managed Postgres example would test separate server identity, credential
isolation, native database restrictions, allowed and denied calls, live
revocation and cleanup after restart. Verify any reuse with two independently
scoped executions and an attempted cross-run request. Keep ordinary CLI/HTTP
delivery available; MCP does not need to replace every tool interface.

An arbitrary approved OCI CLI could separately test integration without a native
connector. Neither proof requires a marketplace or a new per-provider coded
server. Consumption units remain service-specific and inherit the documented
[accounting limits](../capabilities-and-gateways.md#endpoint-interface-and-spending).
