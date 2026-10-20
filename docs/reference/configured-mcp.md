# Configured MCP capabilities

Approved remote MCP servers can supply tools through configuration. Register a
server in the trusted gateway, allow selected tools in policy, then request
`mcp.<server>` in an AgentRun. Sproozi discovers the provider's input schemas.
Operators add any required argument restrictions. A compatible server needs no
provider-specific Go adapter or copied tool catalogue. [ADR
0001](../adr/0001-mcp-capabilities-through-shared-gateway.md) records why
registration uses the shared gateway and named run grants.

This path enforces the **Protocol** tier. Tool names, descriptions and schemas
cannot establish read-only behaviour or service semantics. A reviewed server,
restricted provider credential and upstream-native controls must support claims
about the actual operation. The native [Kubernetes MCP tool](kubernetes-mcp.md)
continues to enforce the **Semantic** tier through its existing handler.
The [MCP tools specification](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
also treats tool annotations as untrusted unless the server is trusted.

## Register the provider

The gateway reads JSON from `MCP_SERVERS_PATH` at startup. The installed
`mcp-servers` ConfigMap starts with an empty registry.
`SPROOZI_ENABLED_CAPABILITIES` selects native modules; remote registrations
become available through this registry, subject to run and policy grants.
For example:

```json
{
  "servers": {
    "docs": {
      "url": "https://docs.example.com/mcp",
      "bearerTokenFile": "/var/run/secrets/sproozi/mcp/docs/token"
    }
  }
}
```

Names are DNS labels of at most 63 characters. `sproozi` is reserved for the
[control connection](sproozi-integration.md) and is rejected. URLs must be exact HTTPS endpoints,
without user info, query parameters or fragments. DNS names, IDNA spelling and
ports are canonicalized before reserving provider authorities. Authentication is
optional; when needed, mount a Secret file under `/var/run/secrets/sproozi/mcp`
**only in the gateway** and reference it with `bearerTokenFile`. Files and symlinks
must remain within that dedicated mount; registry entries cannot read other
gateway credentials. The registration does not support arbitrary
headers.

For deployment, replace the example hosts with your approved providers, update
the gateway ConfigMap's `servers.json`, and add the corresponding Secret volumes
and mounts to the gateway Deployment. Restart the gateway after registration or
credential changes: it snapshots both together. Policy remains live. The remote
provider uses ordinary system TLS trust; private CAs need trusted gateway image
or trust-store configuration. The workload receives only the gateway URL and run
identity. Registered provider authorities are reserved against destination-tier
fallback; a separate egress profile cannot bypass a denied MCP call with direct
provider access.

## Select tools and add scope

The same capability name appears in policy and the immutable run request:

```yaml
# AgentPolicy.spec fragment
allowedCapabilities: [mcp.docs]
mcpServers:
  docs:
    tools:
      search:
        arguments:
          type: object
          required: [collection]
          properties:
            collection:
              const: operations
budgets:
  mcp.docs:
    maxUnits: 10
```

```yaml
# AgentRun.spec fragment
capabilities: [mcp.docs]
```

`tools` is an explicit default-deny map. `search: {}` allows the discovered
tool's input contract without an additional restriction. `arguments` adds JSON
Schema restrictions: `const`, `enum`, `pattern`, numeric bounds and object or
array rules can restrict tenants, repositories, namespaces or other tool inputs.
Use `required` when an argument must be present. These predicates constrain the
request; they do not verify a server's interpretation of it.

Discovery preserves the provider's input schema and describes additional policy
restrictions in the tool description. Before forwarding a call, the gateway
validates both independently. A policy restriction cannot weaken the provider
schema or change its dialect. Draft 2020-12 and draft-07 are supported. Schemas are bounded to 32 KiB, 256 schema nodes and 4096 expanded
evaluation nodes. Acyclic local JSON Pointer references are supported; external
or cyclic references, dynamic references, anchors and nested resource IDs are
rejected. Policy restrictions also reject unknown keywords, format assertions
and vocabulary extensions. Defaults never supply missing arguments. Duplicate
JSON keys and numbers that change decimal meaning during float64 decoding are
rejected.

Every request reloads authorization through the shared proxy. Removing a capability
or its tool, changing scope, stopping the run or cancelling it denies subsequent
requests. When the run stops or loses permission, the gateway cancels its active requests.
A discovered or cached tool is not a grant. Missing approved tools or unsupported
schemas fail closed, without destination fallback.

## Deliver ordinary Codex configuration

Set `AgentRuntime.spec.clientConfig.harness` to `codex`, `claude-code`, `opencode` or `hermes`
for native run-local configuration. See [harness preparation](harnesses.md) for
the additional launch formats, provider setup and supported configurations.

For Codex, set `AgentRuntime.spec.clientConfig.harness: codex`. The controller
adds `codex-mcp.toml` to the immutable run contract ConfigMap. The trusted
launch command copies it into disposable Codex configuration before `codex
exec`:

```sh
export HOME=/home/agent
mkdir -p "$HOME/.codex/skills/sproozi-access"
cp /etc/sproozi/contract/codex-mcp.toml "$HOME/.codex/config.toml"
cp /etc/sproozi/contract/sproozi-instructions.md "$HOME/.codex/AGENTS.md"
cp /etc/sproozi/contract/sproozi-access.md "$HOME/.codex/skills/sproozi-access/SKILL.md"
```

The [demo runtime](../../examples/sre-demo/agentruntime.yaml) does this already,
while retaining its command-line model-provider settings. Requested named MCP
capabilities become connections, followed by the `sproozi`
[control connection](sproozi-integration.md). A run requesting `mcp.docs`
receives:

```toml
[mcp_servers."docs"]
url = "https://sproozi-gateway.sproozi-system.svc:8443/mcp/docs"
required = true

[mcp_servers."sproozi"]
url = "https://sproozi-gateway.sproozi-system.svc:8443/mcp/sproozi"
required = true
```

Codex uses the existing authenticated HTTPS proxy and trust bundle. MCP
connection configuration contains no provider credentials or upstream URLs.
Other clients can use these gateway URLs through the same proxy. Sproozi
generates configuration for the four harness selectors listed above. MCP
resources and prompts are not exposed.

## Protocol limits

Each incoming request creates its own upstream SDK session, discovers selected
tools, executes at most one call, then closes the session. Cookies, upstream session
IDs and workload headers are not shared across runs. This deliberately supports
independent tool operations; servers requiring conversational continuity are
unsupported. Standalone SSE listeners, sampling, elicitation, roots, managed stdio
and OAuth consent flows are outside this implementation. JSON or request-local
SSE responses use the official SDK with redirects and retries disabled.

Before upstream setup, the SDK decodes the JSON-RPC envelope. The bridge admits
`server/discover`, `initialize`, `tools/list`, `tools/call` and `ping` calls plus
initialization and cancellation notifications. Unsupported methods and retained
`subscriptions/listen` requests are rejected before upstream setup.

One overall 30-second request context covers protocol negotiation, legacy
initialization fallback, discovery and tool work. Upstream HTTP exchanges also
have a 30-second timeout. The SDK keeps its connection reader independent of
individual call cancellation so bounded cancellation notifications can be sent.
Notification delivery can take up to five additional seconds, and detached SDK
cleanup DELETE can use its separate HTTP timeout. The overall handler wall-clock
time can therefore exceed 30 seconds during cleanup. Request bodies are limited
to 64 KiB and upstream responses to 8 MiB. Discovery is bounded to eight pages
and 256 tools; policy selects
at most 128 tools per server and the registry holds at most 32 servers. Session
DELETE is attempted on close with the bounded HTTP timeout; provider-side
cleanup still depends on the remote server honouring deletion. Cancellation cannot
undo work already committed by a provider. The gateway uses MCP SDK v1.7.0. It sends bounded cancellation notifications
before closing a call and keeps the session reader active during revocation.

A configured budget spends one unit before each approved tool invocation, keyed
by Run UID and capability. Failed upstream attempts remain charged. Reconnecting
or changing policy does not reset consumption. Omitted budgets are uncapped;
monetary ceilings are rejected because MCP supplies no trusted pricing. Discovery
and initialization have fixed per-request bounds but do not consume tool-call
units. The shared proxy also admits at most 128 connections and eight inspected
sessions per execution by default; see [transport limits](security-hardening.md#protect-transport-and-identities).
Audit records the run, named capability, Protocol tier, approved tool and
fixed outcome, without argument values, results or provider diagnostics. Provider
failures return fixed messages; successful tool output remains provider content.

The [configuration examples](../../examples/mcp/agentpolicy.yaml) show two tool
and scope configurations. Their hosts are placeholders. Replace them with your
approved providers.
