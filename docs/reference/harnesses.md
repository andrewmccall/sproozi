# CLI harnesses

A harness runs the agent loop, conversation and tools. Sproozi supplies its
container environment and controls external service access. This page describes
supported clients, launch settings and completion checks.

Harnesses come in two tiers:

- An [AgentRun harness](../../GLOSSARY.md#agentrun-harness) is any value of
  `AgentRuntime.spec.clientConfig.harness`. Sproozi renders its MCP
  configuration into each run's immutable contract.
- A [managed harness](../../GLOSSARY.md#managed-harness) is also a native kind
  for `sproozi run`, `stop`, `resume` and `move`, described under
  [managed interactive sessions](#managed-interactive-sessions).

To add one, follow [Add a harness](../contributing/add-a-harness.md).

## Harness selectors

This table is the reference list; other pages link here. It matches the
registry in [`internal/harness/registry.go`](../../internal/harness/registry.go).
If `harness` is omitted, the administrator's launch command prepares client
configuration. Unknown values fail validation and contract preparation.

| Selector | Managed | Immutable contract file | Native connection format |
| --- | --- | --- | --- |
| `codex` | yes | `codex-mcp.toml` | Codex `mcp_servers` TOML |
| `claude-code` | yes | `claude-mcp.json` | Claude Code `mcpServers`, HTTP transport |
| `opencode` | no | `opencode-mcp.json` | OpenCode `mcp`, remote transport, OAuth disabled |
| `hermes` | no | `hermes-mcp.json` | Hermes `mcp_servers`, Streamable HTTP |
| `copilot` | no | `copilot-mcp.json` | GitHub Copilot CLI `mcpServers`, HTTP transport, all tools |

Every harness needs an MCP client: there is no harness without the `sproozi`
control connection ([ADR 0039](../adr/0039-one-harness-registry-and-the-mcp-requirement.md)).

Requested named `mcp.<server>` connections appear, followed by the `sproozi`
control connection, which every selected harness receives. Each points to the
shared gateway's `/mcp/<server>` route. Provider URLs and credentials are absent.
The gateway continues to check the run and live policy on each call; rendered
configuration cannot grant tools. Native `kubernetes.read` MCP configuration
remains an explicit trusted launch setting, as in the [Kubernetes MCP reference](kubernetes-mcp.md).

For Claude Code, OpenCode, Hermes and Copilot, contract preparation also writes `instructions.txt`
from the template's trusted instructions and `request.json` from the run's
untrusted task and event context. The original `input.json` remains available.
The examples pass these separately through each harness's native input mechanism.
Every selected harness also receives `sproozi-instructions.md` and
`sproozi-access.md`; see [Sproozi integration and diagnose](sproozi-integration.md)
for where each harness places them.
The complete ConfigMap, including native files, must match on retry. Additional
files, binary data, changed selectors and changed instructions fail closed.

## Launch and configuration ownership

The administrator owns the image, model and Pod command. Sproozi does not
require a fixed client release. The repository's client images install the
releases pinned in [`hack/clients`](../../hack/clients/README.md); build with
other releases by changing those lockfiles. Sproozi
renders public connection configuration and owns Pod identity, network access,
deadlines, cancellation and cleanup. It does not replace the CLI's agent loop.
Use the [stock runtime examples](../../examples/harnesses/README.md), supply reviewed
digest-pinned images and select the runtime in `AgentTemplate.spec.runtimeRef`.
The completion container is still named `agent`.

Codex and Claude Code replace the launch shell with the stock process. OpenCode
can exit zero on an API denial and lose terminal JSON stdout events.
Sproozi therefore supplies `opencode-launch.py` in its immutable contract. This
small Python 3 adapter starts the stock CLI, forwards termination signals, and
then reads the harness conversation through OpenCode's session list and
export. It requires fresh session state, one root session, and a completed final assistant response for the last user request,
with no native error and a matching terminal `step-finish` reason. A missing,
malformed or ambiguous result fails the container. It does not interpret the
answer or decide whether the task was useful. Export inspection is bounded to
8 MiB. The adapter checks completion without replacing the agent loop.

GitHub Copilot CLI replaces the launch shell with one stock `copilot --prompt`
invocation. Its exit status is trustworthy: it exits nonzero when the model
call fails or the gateway refuses it, and zero only after its final `result`
record. With `--output-format json` its standard output is one JSON event per
line. Copilot has no system prompt flag, so the example writes the trusted
instructions and both integration files to `$COPILOT_HOME/copilot-instructions.md`,
which Copilot adds to its system prompt, and passes `request.json` as the
prompt. Copilot also loads `AGENTS.md` and similar instruction files from the
workspace; `--no-custom-instructions` would disable the Sproozi file too.
Copilot names MCP tools `<connection>-<tool>`, such as `sproozi-diagnose`.

Hermes uses `hermes-launch.py` around one native `chat --oneshot`
invocation. The helper loads fresh native configuration, passes trusted template
instructions as a temporary system prompt, and selects only configured MCP
toolsets through reserved `sproozi-mcp-<server>` aliases. This avoids native built-in toolset name collisions. It disables
discovered rules. It validates a single successful native terminal
result, preserves process failure and signals, and publishes its final text in
the UID-bound termination-file envelope. It truncates by encoded UTF-8 envelope
size and marks incomplete text explicitly. Missing or malformed native completion
fails the container; output text does not determine task quality. The helper
runs as PID 1, forwards signals to the native process group and reaps children.
The official image's root/s6 startup is bypassed in bounded non-root sandboxes.
The separate persistent coordinator uses stock image startup and native state.

The Hermes runtime example opts into the gateway's native Kubernetes Pod-list
MCP connection with `--kubernetes-mcp`. This is trusted configuration; calls
still require the requested `kubernetes.read` grant and live namespace/resource
policy. Removing the flag disables the connection.

For ordinary runs, client state belongs in disposable `/home/agent`. Do not
mount existing user homes, authentication stores or sessions. Claude Code's
strict MCP flag excludes other MCP sources, and its settings flags exclude
user/project settings and hooks. OpenCode's custom config normally merges with
other settings, so its example also disables project config and isolates global
state. Its `--pure` mode disables external plugins; the example also disables
discovered skills, sharing, auto-updates, catalog fetching and LSP downloads.
Copilot's example loads the contract's MCP file with `--additional-mcp-config`,
disables its built-in GitHub MCP server, sets `COPILOT_OFFLINE=true` so it makes
no GitHub sign-in, telemetry or update requests, and disables remote control.
These settings limit automatically loaded client features. The gateway and
enforcing CNI remain responsible for permission checks and network restrictions.

The native formats follow the official [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference),
[OpenCode configuration](https://opencode.ai/docs/config/) and
[OpenCode MCP reference](https://opencode.ai/docs/mcp-servers/), and
`copilot help environment`, `copilot help providers` and `copilot mcp --help`
for the GitHub Copilot CLI.

## Managed interactive sessions

The [session service](../../cmd/sproozi/README.md) preserves a selected stock
TUI while moving its harness conversation and Git workspace from Docker to a
fixed Kubernetes profile and back. Managed launches require an explicit
immutable `codex` or `claude-code` selector. Both placements, the profile and
the checkpoint must use the same selected harness. A move cannot switch harnesses.
OpenCode, Hermes and Copilot runs can't be moved.

The workflow in [Run a task remotely, then resume it locally](../guides/portable-workflows.md) supports remote tasks only for managed Codex. Its completion observer requires the
submitted task text and a new completed turn. It then follows the usual stop,
finalized usage and checkpoint steps. A later local resume gets fresh
permissions.
Managed launches also receive the user's [profile](profile.md): Codex in
`$CODEX_HOME/AGENTS.md` and `$CODEX_HOME/skills`, and Claude Code as a section
appended to its system prompt and a `profile` plugin, because its isolated
settings sources exclude user memory. The profile stays outside the checkpoint
and never replaces the approved developer instructions.
Managed Codex MCP connections and placeholder Kubernetes configuration are
regenerated for each launch; portable history cannot select upstream credentials
or server registrations. Managed Claude tasks and MCP are not supported.

| Managed selector | Selected native state | Completed-turn observation |
| --- | --- | --- |
| `codex` | One same-UUID rollout | Native completed turn, resolved supported tools and initial developer instruction digest |
| `claude-code` | One same-UUID primary project transcript | Fresh prompt/tool/Stop hooks plus a settled transcript with paired foreground Bash tools |

The supervisor owns the PTY across attachment disconnects. It uses the installed
client without an exact-version check. The
[recorded round trips](../../test/acceptance/reports/verified-session-mobility.md)
used Codex 0.161.0 and Claude Code 2.1.27; movement hasn't been re-recorded with
the currently pinned Codex 0.163.0 and Claude Code 2.1.302. The selected state and workspace are
published together under `/workspace/session`; personal homes, caches, provider
credentials and previous launch settings are excluded. Every visit supplies
fresh trusted configuration, gateway access and the same approved instructions.
Claude's transcript can include context and prompt snapshots. These remain
history and do not establish approved instructions or permissions. Codex's retained initial instruction block is checked against the
approved digest. The destination still requires fresh admission.

A move blocks new input and waits for the current turn and its tools to finish.
It then revokes access and confirms that the container stopped before capturing
the checkpoint.
Claude uses controlled native process shutdown after its Stop observation; Codex
uses native quit after its completed-turn observation. Hooks and journals run
inside the sandbox, so they are only hints. They can't grant gateway permission,
record finalized usage or prove that the container stopped. Forced
cancellation revokes first. See [ADR
0011](../adr/0011-native-session-compatibility-and-completion.md).

The managed Codex launch uses `--sandbox danger-full-access` to run tools within the outer container boundary. The restricted sandbox
kernel does not support the inner bubblewrap sandbox. Claude uses fixed `bypassPermissions` and foreground Bash. Both
rely on the outer non-root, read-only, capability-restricted container and
enforced gateway network boundary. Portable history cannot select these settings.

A Claude transcript that records a rate-limit (429) retry is supported once a
later successful reply follows it. Other error records, background or parallel
tasks, subagents, compaction and imported personal settings are not supported.
New input after a completed Claude turn means that turn's completion no longer
counts. [ADR 0011](../adr/0011-native-session-compatibility-and-completion.md)
records the transcript edge cases.

## Model providers

Every harness can authenticate in any of three ways: a gateway-held API key, a
gateway-held sign-in (a stored token, or OpenAI's ChatGPT plan mode), or its
own sign-in, which the gateway forwards while still inspecting and counting
each call. Provider hosts and sign-in requests come from one provider table;
see [ADR 0047](../adr/0047-harnesses-sign-in-to-their-own-model-providers.md),
[local setup](../guides/local-sessions.md#choose-how-an-agent-signs-in) and
[AgentRuntime sign-in](agentruntime.md#harness-sign-in). The rest of this
section describes gateway-held credentials.

Codex, Hermes and the OpenCode and Copilot examples use the existing OpenAI
Responses route. Copilot runs in its bring-your-own-key mode
(`COPILOT_PROVIDER_BASE_URL`, `COPILOT_PROVIDER_WIRE_API=responses`), so it
needs no GitHub account or Copilot subscription. With a Copilot plan instead,
leave out the `COPILOT_PROVIDER_*` variables and give Copilot a GitHub token
as `COPILOT_GITHUB_TOKEN`, through
[`clientConfig.signIn`](agentruntime.md#harness-sign-in) (its own sign-in) or
the gateway's `copilot-sign-in-token`. The gateway then serves Copilot's model
API (`/responses`, `/v1/messages`, `/chat/completions`, `/models` and `/auto`)
and its `/copilot_internal/` account requests on `api.github.com`, and refuses
GitHub's built-in MCP server, agents, experiments and telemetry. Copilot loads
MCP servers, including Sproozi's, only if its account allows them
(`is_mcp_enabled`). Models that stream chat completions, such as Gemini, are
refused.
OpenCode uses a configured bundled OpenAI SDK provider rather than an external
execution server. OpenAI API-key or ChatGPT authentication remains administrator
configuration. ChatGPT compatibility is established for the recorded Codex
path; it has not been established for OpenCode. With an API key the gateway
accepts `GET /v1/models`, `POST /v1/responses` (JSON or SSE) and
`POST /v1/chat/completions` (JSON only; a streaming request gets a 403 with
code `streaming_not_supported`). ChatGPT mode accepts only `GET /v1/models` and
`POST /v1/responses` with `store:false` and `stream:true`. See
[protocol and budgets](../guides/model-provider-auth.md#protocol-and-budgets).

Claude Code uses Anthropic's native Messages API through the reserved
`api.anthropic.com:443` route and `model.inference` grant. Set `ANTHROPIC_API_KEY`
on the trusted gateway through the optional `anthropic-api-key` key of the
`model-gateway-credentials` Secret, or a gateway-held sign-in token through
`anthropic-sign-in-token`. No key enters the sandbox; the example supplies only
a harmless placeholder. A Claude subscription works either as that token or as
Claude Code's own sign-in, described above. In API-key mode, at least one
OpenAI or Anthropic key or token must be configured when model inference is
enabled. Anthropic-only operation does not require an OpenAI key. Bedrock,
Vertex and remote-control integration are outside this contract.

The gateway certificate must cover `api.anthropic.com` when its handler is
active. The sandbox trust bundle must include the inspection CA. A disabled
Anthropic authority stays reserved, so generic destination egress cannot supply
weaker access to the provider. With a gateway-held credential, workload provider
credentials and account headers don't select the account: the gateway replaces
them and refuses redirects.

Messages supports `POST /v1/messages`, optionally with `?beta=true`, JSON and
SSE. The gateway streams SSE to the client as it arrives and settles the call
from the usage in `message_start` and `message_delta` once `message_stop`
arrives. Tool-use stop
reasons complete one model response; the stock CLI continues its own loop.
Token counts include ordinary input, cache reads, cache writes and output. Both
provider routes share the same run's `model.inference` account.

A self-contained Anthropic call makes a
[bounded reservation](../../GLOSSARY.md#bounded-reservation) from its request
bytes and `max_tokens`, so Claude Code's calls within a run can proceed in
parallel while their reservations fit. A call that enables a provider-run tool
or references input its bytes don't contain reserves all remaining capacity and
runs alone, as does every Codex and ChatGPT-mode call, which carry no output
bound. A call interrupted after the provider received the whole request, such
as by Esc, is charged an estimate, even before the response headers arrive;
see [calls that aren't charged](../guides/model-provider-auth.md#calls-that-arent-charged)
for the free cases. See
[interrupted calls](../guides/model-provider-auth.md#interrupted-calls). The
whole reservation is charged when no estimate is possible, or at a budget cut;
see [unverifiable calls](../guides/model-provider-auth.md#unverifiable-calls).
Successful provider usage settles the actual consumption; known rejected
provider responses release the reservation. On both routes, a request with
`background` other than omitted, `null` or `false`, or `n` or `best_of` other
than omitted, `null` or 1, is refused with a 400 before inference, because the provider would do work the gateway can't
see or bound; see
[protocol and budgets](../guides/model-provider-auth.md#protocol-and-budgets). Cost-bounded
API-key inference now requires an exact positive input/cached/output price row
before provider work and selects `service_tier: default`, refusing explicit
nondefault tiers. Workload account/project selector headers are removed in both
OpenAI authentication modes. The API-key route still lacks a complete billable-field
allowlist and pricing for all hosted/non-token work. Harness-executed tool
declarations are preserved. Neither route provides a hard provider-spending
guarantee. See [model admission](../guides/model-provider-auth.md#protocol-and-budgets).

For a monetary cap, configure an exact model row in administrator pricing with
positive input, cached input and output rates. Cache creation also requires
`cacheWrite5mMicrosPerMillionTokens` or `cacheWrite1hMicrosPerMillionTokens` for
any consumed category. Rates are microdollars per million tokens. Missing model
pricing denies Anthropic and OpenAI requests before inference, with a 403 and
nothing charged; a consumed category without a rate charges the whole
reservation, after the output has reached the client. Explicit one-hour cache
requests, hosted billable tools, nonstandard service tiers, `context-1m`
long-context betas in `anthropic-beta` and other unpriced extras are denied
under a monetary cap. The pricing table has no long-context premium rate.
Other `anthropic-beta` values, such as those the stock client sends, are
forwarded. Capped requests select `standard_only`
explicitly, including when the client omits its service tier, because the
provider defaults to automatic Priority capacity selection
([service tiers](https://platform.claude.com/docs/en/api/service-tiers)). The verifier still accounts for separate
cache-write TTLs when the provider reports them. Consult current provider prices
when creating the administrator table.

Codex keeps its original developer instructions on `exec resume`. Restoring with
different approved instructions is not supported. See
[ADR 0008](../adr/0008-selected-native-and-git-checkpoints.md).
