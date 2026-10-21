# Configured MCP capabilities

Approved remote MCP servers can supply tools through configuration, with no
Go code: register the server in the gateway, allow selected tools in policy,
and grant `mcp.<server>` to a run. Sproozi discovers the server's input
schemas; you add any argument restrictions. [ADR
0001](../adr/0001-mcp-capabilities-through-shared-gateway.md) records why
registration uses the shared gateway and named run grants.

| Where | Harnesses that get the servers | Configured in |
| --- | --- | --- |
| Kubernetes AgentRuns | Every [harness selector](harnesses.md#harness-selectors) | The gateway's registry ConfigMap, the AgentPolicy and the AgentRun. |
| Local sessions (`sproozi run`) | Codex. Claude Code sessions get no named MCP servers. | `config.json`; see [local sessions](#local-sessions). |
| Sessions on a remote executor | Codex executors | The executor's allowances and the cluster's policy. |

This path enforces the [Protocol tier](../../GLOSSARY.md#protocol-tier).
Tool names, descriptions and schemas cannot establish read-only behaviour or
service semantics. A reviewed server, restricted provider credential and
upstream-native controls must support claims about the actual operation. The
native [Kubernetes MCP tool](kubernetes-mcp.md) enforces the
[Semantic tier](../../GLOSSARY.md#semantic-tier) through its own handler. The
[MCP tools specification](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
also treats tool annotations as untrusted unless the server is trusted.

[`examples/mcp`](../../examples/mcp/agentpolicy.yaml) is a complete working
example for the public DeepWiki server (no credential): its registry, policy,
template, a Codex runtime with a neutral prompt, and a run.

"Adding" a server means one of three things. A contribution adds an example
directory under `examples/` for review; `make verify-mcp` checks it, and it is
never installed. An installation registers the server in its own gateway
registry, the `sproozi-mcp-servers` ConfigMap, which ships empty
(`config/manager/mcp-servers.yaml`) and is never edited in this repository.
This machine registers it in its own `config.json`; see
[local sessions](#local-sessions).

## Find a server's tools

Policy names tools exactly, so start from what the server offers. From a
machine that may reach the server:

```sh
python3 hack/mcp-tools.py https://mcp.deepwiki.com/mcp
```

It prints each tool and its arguments (`?` marks optional ones);
`--bearer-token-file FILE` adds a credential and `--json` prints the schemas.
The listing grants nothing. DeepWiki offers `read_wiki_structure(repoName)`,
`read_wiki_contents(repoName)` and `ask_wiki_question(repoName, question)`.

Use the URL the provider publishes for MCP. The gateway needs one that speaks
the Streamable HTTP transport; a URL that serves only the older HTTP+SSE
transport passes every offline check but fails at the gateway.
`mcp-tools.py` tells you which one a URL speaks.

Two live checks catch what the offline checks cannot. `--policy` compares your
policy's tools and argument names with the server's, reports unknown tools,
unknown arguments and required arguments the policy forbids, and exits 1 on
any mismatch. The Go test lists the tools with the gateway's own MCP client and
checks that the gateway accepts each tool's input schema:

```sh
python3 hack/mcp-tools.py https://mcp.deepwiki.com/mcp --policy examples/mcp/agentpolicy.yaml
SPROOZI_MCP_LIVE_URL=https://mcp.deepwiki.com/mcp \
  go test ./internal/endpoints/mcp -run TestLiveServerToolsCompile -v
```

Approve only tools whose effect you would accept from any run. Leave out tools
that write, and tools that fetch an arbitrary URL, such as GitMCP's
`fetch_generic_url_content`: the provider fetches it, so the URL is outside
Sproozi's [Destination tier](../../GLOSSARY.md#destination-tier) policy.

## Register the provider

The gateway reads `servers.json` from its `sproozi-mcp-servers` ConfigMap in
`sproozi-system` at startup. The source tree calls it `mcp-servers`: the
installation adds `sproozi-` to the start of every object name it creates. The installed registry is empty. A
registry naming two servers, one with a credential, looks like this:

```json
{
  "servers": {
    "deepwiki": {
      "url": "https://mcp.deepwiki.com/mcp"
    },
    "inventory": {
      "url": "https://inventory.example.com/mcp",
      "bearerTokenFile": "/var/run/secrets/sproozi/mcp/inventory/token"
    }
  }
}
```

Each server takes `url` and optional `bearerTokenFile`, and nothing else.
Names are DNS labels: 1 to 63 lowercase letters, digits and hyphens.
`sproozi` is reserved for the [control connection](sproozi-integration.md).
URLs must be exact HTTPS endpoints, without user info, query parameters or
fragments. Host names are lower-cased, internationalized names converted to
their ASCII form and the port made explicit before the gateway reserves the
server's [provider authority](../../GLOSSARY.md#provider-authority). When a server needs a credential, mount a
Secret file under `/var/run/secrets/sproozi/mcp` **only in the gateway** and
name it with `bearerTokenFile`. Files and symlinks must stay within that
mount; the registry cannot read other gateway credentials or set headers.

A run also needs a model. The release quickstart's gateway enables only
`kubernetes.read`, so first give it the model module and its credential, and
cover the model's host in its certificate:

```sh
kubectl -n sproozi-system create secret generic model-gateway-credentials \
  --from-file=openai-api-key=/path/to/openai-api-key
kubectl -n sproozi-system patch deployment sproozi-certificates --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--dns=api.openai.com"}]'
kubectl -n sproozi-system set env deployment/sproozi-gateway \
  SPROOZI_ENABLED_CAPABILITIES=kubernetes.read,model.inference
```

[Required secrets and config](required-secrets-and-config.md) lists the model
credentials and modes, and [gateway certificates](gateway-certificates.md#self-managed-ca)
the certificate names. Then replace the registry and restart the gateway,
which reads the registry and credentials only at startup:

```sh
kubectl -n sproozi-system create configmap sproozi-mcp-servers \
  --from-file=servers.json=examples/mcp/servers.json --dry-run=client -o yaml |
  kubectl apply --server-side --force-conflicts --field-manager=sproozi-admin -f -
kubectl -n sproozi-system rollout restart deployment/sproozi-gateway
kubectl -n sproozi-system rollout status deployment/sproozi-gateway
```

If you deploy from files kept in Git ([GitOps](../../GLOSSARY.md#gitops)),
change the ConfigMap in your own copy of the install files instead, for example
in an [overlay](../../GLOSSARY.md#overlay), and restart the gateway the same
way. For a credential, also add the Secret volume and mount to
the `sproozi-gateway` Deployment. Policy changes need no restart. The provider
uses ordinary system TLS trust; a private CA needs a gateway image or trust
store that includes it. The workload receives only the gateway URL and its run
identity. Registered provider authorities are reserved against
[Destination-tier fallback](../../GLOSSARY.md#destination-tier), so an egress
profile cannot bypass a denied MCP call by reaching the provider directly.

## Select tools and add scope

The same capability name appears in policy and the immutable run request:

```yaml
# AgentPolicy.spec fragment
allowedCapabilities: [mcp.deepwiki]
mcpServers:
  deepwiki:
    tools:
      ask_wiki_question:
        arguments:
          type: object
          required: [repoName, question]
          properties:
            repoName:
              const: modelcontextprotocol/go-sdk
budgets:
  mcp.deepwiki:
    maxUnits: 10
```

```yaml
# AgentRun.spec fragment
capabilities: [mcp.deepwiki]
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
A discovered or cached tool is not a grant. Unsupported schemas fail closed,
without destination fallback.

Every tool the policy names must exist on the server. If one does not, every
call to that server fails, so a misspelt or renamed tool shows up on the first
call instead of silently missing from the agent's list. The error names the
missing tool and the tools the server offers, for example: `MCP server
"deepwiki" tools unavailable or unsupported: the policy approves
"ask_question", which the server does not offer; it offers
"ask_wiki_question", "read_wiki_contents", "read_wiki_structure"`. The agent
sees it, and the gateway's audit log records it as the deny reason.

| Message | Fix |
| --- | --- |
| `MCP registration "docs" has unknown key "headers"; a server takes "url" and optional "bearerTokenFile"` | Remove the key; credentials go in `bearerTokenFile`. |
| `MCP registration name "DeepWiki" is not a valid name …` / `… "sproozi" is reserved …` | Use a lowercase DNS label other than `sproozi`. |
| `capability "mcp.x" has no tool scope; add mcpServers.x.tools to the policy` | Add the tool scope (admission). |
| `MCP tool scope mcpServers.x is invalid: tool "t" arguments: unsupported MCP schema restriction "format"` | Remove the named keyword from the argument schema. |
| `MCP server "x" is not registered in the gateway's MCP registry` | Add it to `servers.json` and restart the gateway. |

## Check your configuration offline

`make verify-mcp` loads every `examples/*/servers.json` with the gateway's own
loader and checks the directory's `agentpolicy.yaml`, `agenttemplate.yaml` and
`agentrun.yaml`:

- every granted `mcp.<name>` is registered, scoped and has a finite budget;
- the scopes compile, and the run asks only for what the policy allows;
- admission accepts the run, and the runtime's harness gets a connection for
  each server;
- no two examples define a policy, template or run with the same name in one
  namespace.

To check your own configuration, put those four files in a directory under
`examples/` and rename the copied resources. Leave out `agentruntime.yaml` to
use the `mcp-research` runtime from `examples/mcp`, which only tells the agent
to use the tools Sproozi configured; add one only if your example needs a
different runtime, with its own name. Tool and argument names can only be
checked against the live server, with `hack/mcp-tools.py --policy`.

GitHub Actions runs the same checks on every pull request. `make test` runs
the Go tests, without the race detector, but not the Python ones.

## Deliver ordinary Codex configuration

Set `AgentRuntime.spec.clientConfig.harness` to one of the [harness selectors](harnesses.md#harness-selectors)
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

The [example runtime](../../examples/mcp/agentruntime.yaml) does this already,
while retaining its command-line model-provider settings. Requested named MCP
capabilities become connections, followed by the `sproozi`
[control connection](sproozi-integration.md). A run requesting `mcp.deepwiki`
receives:

```toml
[mcp_servers."deepwiki"]
url = "https://sproozi-gateway.sproozi-system.svc:8443/mcp/deepwiki"
required = true

[mcp_servers."sproozi"]
url = "https://sproozi-gateway.sproozi-system.svc:8443/mcp/sproozi"
required = true
```

Codex uses the existing authenticated HTTPS proxy and trust bundle. MCP
connection configuration contains no provider credentials or upstream URLs.
Other clients can use these gateway URLs through the same proxy. Sproozi
generates configuration for every [harness selector](harnesses.md#harness-selectors). MCP
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
or changing policy does not reset consumption. The gateway treats an omitted
budget as uncapped, but the example check requires one, so reviewed examples
always cap their calls;
monetary ceilings are rejected because MCP supplies no trusted pricing. Discovery
and initialization have fixed per-request bounds but do not consume tool-call
units. The shared proxy also allows at most 2048 connections, shared by all runs, and
16 inspected connections per execution by default, set by
`SPROOZI_GATEWAY_MAX_CONNECTIONS` and
`SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION`; see [transport limits](security-hardening.md#protect-transport-and-identities).
Audit records the run, named capability, Protocol tier, approved tool and
fixed outcome, without argument values, results or provider diagnostics. Provider
failures return fixed messages; successful tool output remains provider content.

## Local sessions

A local Codex session can use the same registered servers through this
machine's gateway. Add to the `gateway` object in `config.json`:

| Key | Value |
| --- | --- |
| `mcpAuthority` | The exact DNS name and port 443 workers use for the gateway's MCP endpoint. `sproozi init` sets `mcp.sproozi.internal:443` and covers it in the gateway certificate; a hand-written configuration's certificate must cover it. |
| `mcpRegistryFile` | Absolute path of a private `servers.json` in the format above. |
| `mcpCredentialsDirectory` | Absolute private directory holding the files `bearerTokenFile` names, when a server needs a credential. |
| `mcpPolicy` | The `mcpServers` map of an AgentPolicy: server name to approved `tools`. |

Add a unit allowance for each server, such as
`"mcp.deepwiki": {"units": {"bounded": true, "value": 10}}`, then restart the
service with `sproozi service stop` and `sproozi service start`. Monetary MCP
allowances are rejected because MCP has no trusted pricing. Sproozi generates
the Codex client configuration for each session from these allowances.

Claude Code sessions on the same machine get no named MCP servers: Sproozi
generates named-server configuration only for Codex sessions, and says so when
the service starts (`claude-code sessions get no configured MCP servers
(mcp.deepwiki): …`). Start the session with `sproozi run --harness codex` to
use them.
