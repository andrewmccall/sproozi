# Kubernetes MCP delivery

The shared gateway serves Streamable HTTP MCP at
`https://kubernetes.default.svc/mcp`. It exposes `kubernetes_list_pods` with one
required string argument, `namespace`. The tool maps to a fixed Kubernetes GET
for Pods in that namespace with `limit=100`. The result is a JSON PodList in MCP
text content. It includes Kubernetes pagination metadata when more Pods remain,
but this tool cannot request another page. It accepts no pagination token. It
also supports no watches, logs, mutations or arbitrary URLs.

The tool delivers the existing `kubernetes.read` capability at the Semantic tier.
The run must request that capability, current policy must allow it, and policy
must include both the namespace and `pods` resource. Discovery omits the tool
when Pod reads are unavailable; actual calls still authorize their namespace.
Opening a connection or forging an MCP session ID grants no permission.

## Identity and lifetime

Clients use the same authenticated HTTPS forward proxy and mounted trust bundle
as `kubectl`. The projected run token authenticates CONNECT. No provider token
or Kubernetes credential belongs in MCP configuration or workload headers. The
gateway's Kubernetes transport supplies upstream credentials.

The endpoint uses the official [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk)
with stateless JSON responses. It keeps no shared MCP sessions and provides no
server-initiated requests or resumable SSE stream. GET and DELETE return 405.
Browser Origin headers are rejected. The trusted proxy binds CONNECT, SNI and
Host to the Kubernetes authority and revalidates the run and policy on requests
and during open sessions. Tool work retains the original HTTP context so
revocation and session deadlines cancel in-flight upstream calls, including
older MCP protocol clients.

MCP request bodies are limited to 64 KiB and Pod responses to 8 MiB. Namespace
arguments must be Kubernetes DNS labels. Upstream failures return fixed messages
without upstream error bodies. Returned Pod content remains untrusted evidence.
The tool uses the existing Kubernetes semantic audit record, attributed to Run
UID, capability, namespace/resource and tier. It spends one Kubernetes request
unit before forwarding and shares the run's `kubernetes.read` budget with REST.
MCP initialization and discovery do not spend upstream request units.

## Trusted Codex launch configuration

The [Codex MCP configuration](https://developers.openai.com/codex/mcp) supports a
Streamable HTTP URL and a required connection. In an administrator-owned launch
command, after configuring the ordinary proxy and trust environment, pass:

```sh
codex exec \
  -c 'mcp_servers.sproozi-kubernetes.url="https://kubernetes.default.svc/mcp"' \
  -c 'mcp_servers.sproozi-kubernetes.enabled_tools=["kubernetes_list_pods"]' \
  -c 'mcp_servers.sproozi-kubernetes.required=true' \
  ...
```

The [manual demo](../guides/manual-pr-demo.md) renderer has an opt-in variant:

```sh
python3 hack/demo/render.py \
  --codex-image "$CODEX_IMAGE" \
  --repository "$SPROOZI_DEMO_REPOSITORY" \
  --model-auth api_key --mcp > /tmp/sproozi-mcp-demo.yaml
```

This renders the connection into the trusted runtime command and asks Codex to
start its investigation with the Pod-list tool. It requires the demo's existing
`kubernetes.read` grant. Review and apply the rendered manifest only in the
isolated demo cluster. `required=true` makes a failed MCP startup fail the launch.
The default demo renderer remains unchanged.
