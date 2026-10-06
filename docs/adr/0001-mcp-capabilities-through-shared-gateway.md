---
status: accepted
date: 2026-10-06
---

# Use MCP through the shared capability gateway

MCP already supplies tool discovery, descriptions and input schemas. Adding each
provider should require trusted configuration and policy, without a Sproozi
adapter for every server. Deliver native semantic tools and configured remote
MCP tools through the existing authenticated gateway and run capability model.

## Decision

Native MCP delivery reuses the existing service handler and capability. The
Kubernetes Pod-list tool uses `kubernetes.read` and retains Semantic enforcement.
Configured HTTPS MCP servers use named `mcp.<server>` capabilities and Protocol
enforcement. An argument schema constrains the request; tool descriptions and
annotations do not establish service semantics.

The gateway owns a startup registry of exact provider URLs and optional bearer
credential files. It snapshots registration and credential bytes together.
`AgentRun.spec.capabilities` requests named authority;
`AgentPolicy.spec.mcpServers` explicitly selects tools and optional JSON Schema
argument restrictions. The bridge discovers provider schemas, filters permitted
tools and validates provider and policy schemas independently. Registering a
server, loading client configuration or caching discovery grants no authority.

Keep authentication, live policy revalidation, audit and durable Run UID/capability
accounting in the existing trusted boundary. Reserve provider authorities against
weaker destination routing. Workloads receive broker URLs and projected run
identity; provider credentials stay behind the gateway.

The downstream MCP endpoint is stateless. Each incoming HTTP request owns one
upstream SDK session, bounded discovery, at most one tool invocation and teardown.
A CONNECT connection is a transport lifetime, not an MCP conversation. The
trusted Codex launch receives its public connection fragment in the existing
immutable run contract, derived from requested names and the runtime's gateway.

## Boundary and ownership choices

Remote connections use `/mcp/<server>` on the gateway's existing authority.
Provider URLs stay inside the registry, so adding a server does not add workload
destinations, inspection certificate names or controller routing configuration.
The MCP handler selects the named budget account after validating the path;
the shared dispatcher cannot assign one fixed capability to this mixed route.
Controller admission checks requested names and policy scope, without reading
the registry or contacting providers. A valid but unregistered name therefore
fails at the gateway, after admission.

Registration may read bearer files only within a dedicated MCP credential mount,
including symlinks contained within it. Otherwise a registry edit would acquire
authority to read unrelated gateway credentials. The upstream client does not
inherit workload headers, cookies, session IDs or ambient proxy configuration.
Redirects and tool retries are disabled. A failure or uncertain result must not
silently send the same operation again or move it to another destination.

Use the official MCP SDK and a JSON Schema library behind one `http.Handler`,
with shared policy compilation for admission and call enforcement. Provider and
policy schemas retain their original dialects and validate the same parsed
arguments independently. A merged schema can change draft-07 interpretation
under a newer dialect and lose restrictions. The advertised `allOf` is client
guidance only. Reject unsupported or unbounded schema forms and ambiguous JSON,
rather than fetching external references, applying defaults or building a custom
validator. Missing approved tools fail preparation; newly discovered tools gain
no authority.

Keep client delivery in the existing immutable contract and a concrete Codex
renderer. The trusted launch installs the fragment while retaining its approved
model command options. Avoid another contract resource, global client settings,
a custom tool manifest or a general harness plugin registry. The workload may
change its client configuration; gateway authorization still controls each call.
Native Kubernetes MCP remains a separate trusted launch opt-in because it
delivers an existing Semantic capability, not a registered remote server.

## Alternatives considered

- A Go adapter and copied tool catalogue for every provider would preserve native
  service knowledge but make routine onboarding an implementation project. Keep
  native adapters where their semantic guarantees are needed; use the configured
  bridge for tools that fit Protocol enforcement.
- An `AgentMCPServer` resource and a separate run server list would support
  Kubernetes registration workflows, namespace RBAC and live resolution, but
  introduce another API and authority vocabulary, Secret/revision reads and
  registration-aware revocation. Read-only resolution does not require a provider
  controller. The registry reuses the existing capability list at the cost of
  deployment coordination for changes. Revisit a resource when dynamic
  registration, namespace ownership or registration status requires it.
- A unified `AgentCapability` resource could represent native and MCP
  implementations together. It would also migrate native names, scopes, budgets,
  admission and clients. Named MCP extensions meet the current requirement while
  preserving native meanings; a broader capability API needs its own migration
  requirement and decision.
- Routing on each provider authority would reuse the existing authority map, but
  couple provider addresses, certificate names and shared-host paths to workload
  delivery. Gateway-owned named paths keep that coordination inside the trusted
  registry. Direct provider routes remain reserved against Destination fallback.
- Persistent upstream sessions would support conversations, but need explicit
  run/server ownership, expiry and recovery. No selected provider required that
  state. Per-request ownership avoids sharing sessions across runs; revisit it
  when a concrete provider requires continuity.
- Keying budgets by registration UID would allow replacing a registration to
  restore the same run's allowance. Run UID plus capability name preserves the
  bound across reconnects and registration replacement. If registrations become
  resources later, their identity must not replenish the account.
- An approved input-schema digest would detect contract changes across requests,
  but add an approval/versioning workflow without proving unchanged provider
  semantics. Fresh discovery plus unchanged policy predicates enforce the current
  Protocol contract. Revisit pinning when an actual integration requires reviewed
  contract versions.
- Promoting a provider to Semantic through tool annotations or a `tier` setting
  would turn descriptive metadata into an authorization guarantee. Keep generic
  mediation Protocol. Stronger declarative bindings need a reviewed service
  contract and upstream restrictions that establish resource/action meaning.

## Consequences

Registration and credential changes require a gateway restart; policy remains
live. Each request repeats initialization and discovery. Servers requiring
conversation continuity are unsupported. Managed stdio, remote OAuth consent,
resources, prompts and server-initiated authority need separate decisions.

One configured MCP budget unit is one attempted upstream tool invocation. Failed
attempts remain charged, and reconnecting retains the Run UID/capability account.
Discovery is bounded separately. Omitted budgets are uncapped; generic MCP has no
trusted monetary pricing. Provider restrictions and reviewed behaviour remain
necessary for service-semantic guarantees.

Request cancellation revokes tool work, while the session reader remains alive
long enough to deliver bounded cancellation and teardown. Closing both together
can discard the upstream cancellation notification. Remote deletion is
best-effort and cannot undo committed work or guarantee cleanup after a gateway
crash. The current SDK workaround and supported schema/transport limits belong
in the reference. Expand them when a concrete provider needs the feature, with
an explicit owner for any new lifecycle or authority.

## Implementation and evidence

- [System architecture](../explanations/system-architecture.md) and
  [capabilities and gateways](../explanations/capabilities-and-gateways.md)
- [Configured MCP reference](../reference/configured-mcp.md),
  [native Kubernetes MCP reference](../reference/kubernetes-mcp.md) and
  [agent contract](../reference/agent-contract.md)
- Registry and bridge: `internal/endpoints/mcp`; policy validation:
  `internal/mcppolicy`; client fragment: `internal/harness`; immutable contract:
  `internal/kubernetes/contract.go`; gateway wiring: `cmd/gateway/main.go`
- [Recorded acceptance](../demos/verified-mcp-demo.md) covers stock Codex use of
  native Kubernetes and two configured HTTPS fixture providers, denials, budgets,
  cancellation and normal cleanup. This is bounded dirty-tree evidence, not
  universal MCP compatibility or a release claim.
