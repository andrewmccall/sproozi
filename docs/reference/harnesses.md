# CLI harnesses

`AgentRuntime.spec.clientConfig.harness` selects generated run-local MCP
configuration. Supported selectors are `codex`, `claude-code` and `opencode`.
An omitted selector leaves client configuration to the administrator's launch
command. Unknown selectors fail runtime validation and contract preparation.

| Selector | Immutable contract file | Native connection format |
| --- | --- | --- |
| `codex` | `codex-mcp.toml` | Codex `mcp_servers` TOML |
| `claude-code` | `claude-mcp.json` | Claude Code `mcpServers`, HTTP transport |
| `opencode` | `opencode-mcp.json` | OpenCode `mcp`, remote transport, OAuth disabled |

Only requested named `mcp.<server>` connections appear. Each points to the
shared gateway's `/mcp/<server>` route. Provider URLs and credentials are absent.
The gateway continues to check the run and live policy on each call; rendered
configuration cannot grant tools. Native `kubernetes.read` MCP configuration
remains an explicit trusted launch setting, as in the [Kubernetes MCP reference](kubernetes-mcp.md).

For Claude Code and OpenCode, contract preparation also writes `instructions.txt`
from the template's trusted instructions and `request.json` from the run's
untrusted task and event context. The original `input.json` remains available.
The examples pass these separately through each client's native input mechanism.
The complete ConfigMap, including native files, must match on retry. Additional
files, binary data, changed selectors and changed instructions fail closed.

## Launch and configuration ownership

The administrator owns the image, CLI version, model and Pod command. Sproozi
renders public connection configuration and owns Pod identity, network access,
deadlines, cancellation and cleanup. It does not replace the CLI's agent loop.
Use the [stock runtime examples](../../examples/harnesses/README.md), supply reviewed
digest-pinned images and select the runtime in `AgentTemplate.spec.runtimeRef`.
The completion container is still named `agent`.

Codex and Claude Code replace the launch shell with the stock process. OpenCode
1.15.1 can exit zero on an API denial and lose terminal JSON stdout events.
Sproozi therefore supplies `opencode-launch.py` in its immutable contract. This
small Python 3 adapter starts the stock CLI, forwards termination signals, and
then reads its native session list/export. It requires fresh session state, one
root session, and a completed final assistant response for the last user request,
with no native error and a matching terminal `step-finish` reason. A missing,
malformed or ambiguous result fails the container. It does not interpret the
answer or decide whether the task was useful. Export inspection is bounded to
8 MiB. This is completion normalization around the existing loop.

Client state belongs in disposable `/home/agent`. Do not mount existing user
homes, authentication stores or sessions. Claude Code's strict MCP flag excludes
other MCP sources, and its settings flags exclude user/project settings and
hooks. OpenCode's custom config normally merges with other settings, so its
example also disables project config and isolates global state. Its `--pure`
mode disables external plugins; the example also disables discovered skills,
sharing, auto-updates, catalog fetching and LSP downloads. These settings reduce
ambient client behavior. Sproozi's gateway and enforcing CNI remain the authority
and network controls.

The native formats follow the official [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference),
[OpenCode configuration](https://opencode.ai/docs/config/) and
[OpenCode MCP reference](https://opencode.ai/docs/mcp-servers/).

## Model providers

Codex and the OpenCode example use the existing OpenAI Responses route.
OpenCode uses a configured bundled OpenAI SDK provider rather than an external
execution server. OpenAI API-key or ChatGPT authentication remains administrator
configuration. ChatGPT compatibility is established for the recorded Codex
path; it has not been established for OpenCode.

Claude Code uses Anthropic's native Messages API through the reserved
`api.anthropic.com:443` route and `model.inference` grant. Set `ANTHROPIC_API_KEY`
on the trusted gateway through the optional `anthropic-api-key` key of the
`model-gateway-credentials` Secret. No key enters the sandbox; the example
supplies only a harmless placeholder. `MODEL_AUTH_MODE` still selects OpenAI
authentication. In API-key mode, at least one OpenAI or Anthropic key must be
configured when model inference is enabled. Anthropic-only operation does not
require an OpenAI key. Claude subscription, Bedrock, Vertex and remote-control
integration are outside this contract.

The gateway certificate must cover `api.anthropic.com` when its handler is
active. The sandbox trust bundle must include the inspection CA. A disabled
Anthropic authority stays reserved, so generic destination egress cannot supply
weaker access to the provider. Workload provider credentials and account headers
do not select the account; the gateway replaces them and refuses redirects.

Messages supports `POST /v1/messages`, optionally with `?beta=true`, JSON and
buffered SSE. Successful SSE must include a valid start, cumulative usage and
terminal message stop before any response reaches the client. Tool-use stop
reasons complete one model response; the stock CLI continues its own loop.
Token counts include ordinary input, cache reads, cache writes and output. Both
provider routes share the same run's `model.inference` account.

Anthropic calls reserve all remaining configured token and monetary capacity.
This serializes capped calls within a run, because `max_tokens` limits output
rather than total input and output. Ambiguous provider consumption forfeits
reserved tokens and dollars. Successful provider usage settles the actual
consumption; known rejected provider responses release the reservation.
OpenAI's output-bound admission behavior remains unchanged and can underestimate
total usage. Neither route provides a hard provider-spending guarantee.

For a monetary cap, configure an exact model row in administrator pricing with
positive input, cached input and output rates. Cache creation also requires
`cacheWrite5mMicrosPerMillionTokens` or `cacheWrite1hMicrosPerMillionTokens` for
any consumed category. Rates are microdollars per million tokens. Missing model
pricing denies Anthropic requests before inference; missing consumed-category
rates forfeit the reservation and withhold output. Explicit one-hour cache
requests, hosted billable tools, nonstandard service tiers and other unpriced
extras are denied under a monetary cap. Capped requests select `standard_only`
explicitly, including when the client omits its service tier, because the
provider defaults to automatic Priority capacity selection
([service tiers](https://platform.claude.com/docs/en/api/service-tiers)). The verifier still accounts for separate
cache-write TTLs when the provider reports them. Consult current provider prices
when creating the administrator table.

## Verification and limits

`make verify-harness-clients` loads the production renderer using installed
stock clients in temporary homes and configuration directories, without a model
request. Locally checked versions are Codex 0.161.0, Claude Code 2.1.27 and
OpenCode 1.15.1. This establishes native configuration loading, not deployed
provider compatibility.

The provider tests exercise shared accounting, credential replacement, terminal
usage, cache rates, unsupported operations and fail-closed malformed responses.
Existing controller tests cover process exit, cancellation, deadline expiry and
sandbox cleanup. `make verify-harness-process` exercises stock model/tool loops with local mocked
providers through the real inspected-TLS proxy, including generated native MCP
connections, tool invocation, budget settlement and capability denials. It also
checks the normalized OpenCode launch outcome. It uses no paid provider calls.
A successful fixture does not establish real Anthropic billing, enforcing CNI
behavior or a deployed Claude/OpenCode run. The recorded Kind proof remains the
[Codex MCP acceptance](../demos/verified-mcp-demo.md).

The [harness and provider ADR](../adr/0002-native-cli-harnesses-and-model-protocols.md)
records the choices, rejected alternatives and revisit conditions.
