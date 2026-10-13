# Kubernetes MCP delivery

**Status:** one native semantic tool is implemented. Stock Codex used it against
real Kubernetes in dedicated Kind with Calico on 6 October 2026. The
[recorded acceptance](../demos/verified-mcp-demo.md) covers allowed tool use,
network and scope denials, cancellation and scoped cleanup. Local integration
tests also use the official MCP client over TLS CONNECT with a fake upstream.

The shared gateway serves Streamable HTTP MCP at
`https://kubernetes.default.svc/mcp`. It exposes `kubernetes_list_pods` with one
required string argument, `namespace`. The tool maps to a fixed Kubernetes GET
for Pods in that namespace with `limit=100`. The result is a JSON PodList in MCP
text content, including Kubernetes pagination metadata when more Pods remain.
This first tool has no continuation, watch, log, mutation or arbitrary URL input.

The tool delivers the existing `kubernetes.read` capability at the Semantic tier.
The run must request that capability, current policy must allow it, and policy
must include both the namespace and `pods` resource. Discovery omits the tool
when Pod reads are unavailable; actual calls still authorize their namespace.
Installing a connection or forging an MCP session ID grants no authority.

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

## Verification

```sh
make verify-mcp
```

The tests exercise discovery, allowed calls, forged arguments and tools,
namespace/resource/capability denials, shared REST/MCP budgeting, separate run
budgets, oversized inputs and outputs, redacted upstream errors, and revocation
of both existing clients and in-flight upstream work. The same gate runs offline
verifier checks for partial creation, ambiguous create timeouts, cleanup failures,
redacted reports and Secret references in every Pod container and volume source.
These checks make no model requests.

For a caller-provisioned, dedicated multi-node Kind cluster, render the bounded
fixture and run the live gate. The fixture renderer requires PyYAML. Supply a
digest-pinned stock Codex image with curl and the selected model-auth mode.
The cluster needs the current controller/gateway, `config/agent-rbac`, Calico,
gateway TLS and controller/sandbox trust ConfigMaps, and valid model credentials
in the trusted namespace. See the [manual setup](../guides/manual-pr-demo.md)
and [model authentication](../guides/model-provider-auth.md) for those inputs.
The gate expects no competing active runs. It does not provision a cluster or
credentials and does not create a GitHub PR.

```sh
python3 hack/verify/render-mcp-fixture.py \
  --codex-image "$CODEX_IMAGE" --model-auth chatgpt > /tmp/sproozi-mcp-proof.yaml
kubectl --kubeconfig "$MCP_KUBECONFIG" apply -f /tmp/sproozi-mcp-proof.yaml
python3 hack/verify/mcp-live.py \
  --kubeconfig "$MCP_KUBECONFIG" --cluster "$MCP_KIND_CLUSTER" \
  --report .local/verification/mcp-live.json
```

The fixture grants only `kubernetes.read` and `model.inference` and scopes Pod
reads to `sproozi-demo`. Its stock CLI prints JSON tool events and an exit marker,
then holds the Pod for verifier probes. The gate requires successful CLI exit
and a completed tool result containing the real demo Pod. It then verifies
namespace and unknown-tool denials, direct service/node API timeouts, cancellation,
stale-token denial from an independently isolated witness and removal of
run-labelled invocation resources. It deletes its own Run and witness and
writes a redacted report. The fixture resources and cluster remain caller-owned.

This proves normal provisioned-run cancellation, not recovery from lost
provisioning status or automatic deletion of durable budget state. Observed
provider usage does not establish hard spending ceilings. Historical SRE
acceptance predates this MCP route. Configured remote MCP has separate
[deployed fixture evidence](../demos/verified-mcp-demo.md#configured-remote-mcp-acceptance).
Managed stdio servers and Docker execution remain unverified. Additional stock
harnesses have a separate [full Kind gate](../guides/hermes-orchestration.md).
