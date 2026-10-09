# Configured MCP capabilities

Approved remote MCP servers can supply tools through configuration. Register a
server in the trusted gateway, allow selected tools in policy, then request
`mcp.<server>` in an AgentRun. Sproozi discovers the provider's input schemas;
operators add only the argument restrictions they need. No provider-specific Go
adapter or copied tool catalogue is required. [ADR 0001](../adr/0001-mcp-capabilities-through-shared-gateway.md)
records why registration uses the shared gateway and named run grants.

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

Names are DNS labels of at most 63 characters. URLs must be exact HTTPS endpoints,
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
or trust-store configuration. The workload receives only the broker URL and run
identity. Registered provider authorities are reserved against destination-tier
fallback; a separate egress profile cannot turn a denied MCP call into direct
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

`tools` is an explicit default-deny map. `search: {}` allows the discovered tool's
input contract without an additional restriction. `arguments` is an additive
JSON Schema predicate: `const`, `enum`, `pattern`, numeric bounds and object or
array rules can restrict tenants, repositories, namespaces or other tool inputs.
Use `required` when an argument must be present. These predicates constrain the
request; they do not verify a server's interpretation of it.

The gateway validates the discovered schema and policy restriction independently,
so one cannot weaken the other or change its schema dialect. Discovery presents
an `allOf` intersection for client guidance; gateway validation remains the
boundary. Draft 2020-12 and draft-07 are supported. Schemas are bounded to 32 KiB,
256 schema nodes and 4096 expanded evaluation nodes. Acyclic local JSON Pointer
references are supported; external or cyclic references, dynamic references,
anchors and nested resource IDs are rejected. Policy restrictions also reject
unknown keywords, format assertions and vocabulary extensions. Defaults never
supply missing arguments. Duplicate JSON keys and numbers that change decimal
meaning during float64 decoding are rejected.

Every request reloads authorization through the shared proxy. Removing a capability
or its tool, changing scope, stopping the run or cancelling it denies subsequent
requests. Proxy revalidation cancels active requests when run authority ends.
A discovered or cached tool is not a grant. Missing approved tools or unsupported
schemas fail closed, without destination fallback.

## Deliver ordinary Codex configuration

Set `AgentRuntime.spec.clientConfig.harness` to `codex`, `claude-code`, `opencode` or `hermes`
for native run-local configuration. See [harness preparation](harnesses.md) for
the additional launch formats, provider setup and verification limits.

The following Codex example sets `AgentRuntime.spec.clientConfig.harness: codex`.
Set `AgentRuntime.spec.clientConfig.harness: codex`. Provisioning adds
`codex-mcp.toml` to the existing immutable run contract ConfigMap. The trusted
launch command copies it into disposable Codex configuration before `codex exec`:

```sh
export HOME=/home/agent
mkdir -p "$HOME/.codex"
cp /etc/sproozi/contract/codex-mcp.toml "$HOME/.codex/config.toml"
```

The [demo runtime](../../examples/sre-demo/agentruntime.yaml) does this already,
while retaining its command-line model-provider settings. Only requested named
MCP capabilities become connections. A run requesting `mcp.docs` receives:

```toml
[mcp_servers."docs"]
url = "https://sproozi-gateway.sproozi-system.svc:8443/mcp/docs"
required = true
```

Codex uses the existing authenticated HTTPS proxy and trust bundle. MCP connection
configuration contains no provider credentials or upstream URLs. Other clients
can use these broker URLs through the same proxy; automatic configuration
rendering currently supports Codex only. MCP resources and prompts are not exposed.

## Bounds and evidence

Each incoming request creates its own upstream SDK session, discovers selected
tools, executes at most one call, then closes the session. Cookies, upstream session
IDs and workload headers are not shared across runs. This deliberately supports
independent tool operations; servers requiring conversational continuity are
unsupported. Standalone SSE listeners, sampling, elicitation, roots, managed stdio
and OAuth consent flows are outside this implementation. JSON or request-local
SSE responses use the official SDK with redirects and retries disabled.

Tool work and upstream HTTP operations have 30-second timeouts. Cancellation
notification delivery can take up to five additional seconds before session
teardown. Request bodies are limited to 64 KiB and upstream responses to 8 MiB. Discovery is bounded to eight pages and 256 tools; policy selects
at most 128 tools per server and the registry holds at most 32 servers. Session
DELETE is attempted on close with the bounded HTTP timeout; provider-side
cleanup still depends on the remote server honouring deletion. Cancellation cannot
undo work already committed by a provider. SDK v1.7.0 is pinned because it sends
bounded cancellation notifications before retiring a call. Its session reader
stays alive while individual calls observe revocation. The v1.8.0 asynchronous
notification behaviour raced session teardown in repeated revocation tests.

A configured budget spends one unit before each approved tool invocation, keyed
by Run UID and capability. Failed upstream attempts remain charged. Reconnecting
or changing policy does not reset consumption. Omitted budgets are uncapped;
monetary ceilings are rejected because MCP supplies no trusted pricing. Discovery
and initialization have fixed per-request bounds but do not consume tool-call
units. Audit records the run, named capability, Protocol tier, approved tool and
fixed outcome, without argument values, results or provider diagnostics. Provider
failures return fixed messages; successful tool output remains provider content.

[Configuration examples](../../examples/mcp/agentpolicy.yaml) include two providers
with different tool and scope shapes. Their hosts are placeholders, not deployed
integrations. Run the repeatable local checks:

```sh
make verify-mcp
make verify-mcp-client  # installed stock Codex; no model request
make test              # includes real API-server validation through envtest
```

The proxy integration tests use two official SDK HTTPS servers and ordinary SDK
clients, covering discovery, scoped calls, reconnect budgets, distinct runs,
credential snapshots, malformed requests, revocation, session deletion and schema
failure cases. The client check exercises the actual TOML renderer in a temporary
`CODEX_HOME`. These local checks complement the
[recorded Kind acceptance](../demos/verified-mcp-demo.md#configured-remote-mcp-acceptance)
with stock Codex and two deployed fixture providers. That report proves the
configured HTTPS path, not every ecosystem server.

## Deployed verification

`make verify-mcp-kind` creates and removes its own two-node Kind cluster. Supply
Docker, Kind, kubectl, OpenSSL, PyYAML, the checksum-pinned Calico v3.30.4 manifest,
a digest-pinned stock Codex tools image and a dedicated single-owner ChatGPT
session file. The check uses real model requests. Do not share that rotating
session with another running gateway; the script returns its newest replacement
to the supplied file before deleting the cluster.

```sh
docker build -t controller:mcp-proof .
docker build -f hack/verify/mcp-provider/Dockerfile -t mcp-provider:proof .
export MCP_PROOF_IMAGE=controller:mcp-proof
export MCP_PROVIDER_IMAGE=mcp-provider:proof
export CODEX_IMAGE="<stock-codex-image>@sha256:<digest>"
export CHATGPT_SESSION_FILE="<private-single-owner-session.json>"
export SPROOZI_CNI_MANIFEST="<pinned-calico-v3.30.4.yaml>"
export MCP_PROOF_REPORT=.local/verification/configured-mcp-live.json
make verify-mcp-kind
```

The retained local-registry stock image can use `hack/demo/load-image.sh`;
other images must already be available to Docker. The gate deploys two fixture
providers with different schemas and gateway-only bearer credentials. It checks
actual Codex tool events, scope/schema/tool/server denials, cross-connection
budget consumption, direct network denial, cancellation, correlated audit and
controller cleanup. Exact provider invocation counts establish that denied calls
never execute tools. An independently contained witness tests stale authority
after the original Pod stops. The setup enforces immutable sandbox trust and
uses server-side apply for generated CRDs.

Normal exit removes the owned cluster and private fixture material. If rotating
session recovery fails, the script retains its named cluster and private directory
for recovery instead of losing the newest credential.
