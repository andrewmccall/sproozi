# CLI harnesses

`AgentRuntime.spec.clientConfig.harness` selects generated run-local MCP
configuration. Supported selectors are `codex`, `claude-code`, `opencode` and `hermes`.
An omitted selector leaves client configuration to the administrator's launch
command. Unknown selectors fail runtime validation and contract preparation.
These four selectors describe ordinary `AgentRun` launches. Managed interactive
sessions have the narrower explicit compatibility contract below.

| Selector | Immutable contract file | Native connection format |
| --- | --- | --- |
| `codex` | `codex-mcp.toml` | Codex `mcp_servers` TOML |
| `claude-code` | `claude-mcp.json` | Claude Code `mcpServers`, HTTP transport |
| `opencode` | `opencode-mcp.json` | OpenCode `mcp`, remote transport, OAuth disabled |
| `hermes` | `hermes-mcp.json` | Hermes `mcp_servers`, Streamable HTTP |

Only requested named `mcp.<server>` connections appear. Each points to the
shared gateway's `/mcp/<server>` route. Provider URLs and credentials are absent.
The gateway continues to check the run and live policy on each call; rendered
configuration cannot grant tools. Native `kubernetes.read` MCP configuration
remains an explicit trusted launch setting, as in the [Kubernetes MCP reference](kubernetes-mcp.md).

For Claude Code, OpenCode and Hermes, contract preparation also writes `instructions.txt`
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

Hermes 0.21.6 uses `hermes-launch.py` around one native `chat --oneshot`
invocation. The helper loads fresh native configuration, passes trusted template
instructions as an ephemeral system prompt, selects only configured MCP toolsets through reserved `sproozi-mcp-<server>`
aliases. This avoids native built-in toolset name collisions. It disables
discovered rules. It validates a single successful native terminal
result, preserves process failure and signals, and publishes its final text in
the UID-bound termination-file envelope. It truncates by encoded UTF-8 envelope
size and marks incomplete text explicitly. Missing or malformed native completion
fails the container; output text does not determine task quality. The helper
runs as PID 1, forwards signals to the native process group and reaps children.
The official image's root/s6 startup is bypassed for bounded non-root workers.
The separate persistent coordinator uses stock image startup and native state.

The Hermes runtime example opts into the gateway's native Kubernetes Pod-list
MCP connection with `--kubernetes-mcp`. This is trusted configuration; calls
still require the requested `kubernetes.read` grant and live namespace/resource
policy. Removing the flag disables the connection.

For ordinary runs, client state belongs in disposable `/home/agent`. Do not mount existing user
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

## Managed interactive sessions

The [session service](../../cmd/sproozi/README.md) preserves a selected stock TUI
while moving its native conversation and Git workspace from Docker to a fixed
Kubernetes profile and back. Managed launches require an explicit immutable
`codex` or `claude-code` selector. Both placements, the profile and the checkpoint
must agree; a move cannot switch clients. OpenCode and Hermes ordinary launches
do not establish managed session mobility support.

The [retained-work workflow](../guides/portable-workflows.md) supports remote-first
positional tasks only for managed Codex. Its completion observer requires the
actual submitted user text and a new completed native turn, then the ordinary
writer-fence/accounting/checkpoint path. Later local resume uses a fresh grant.
Managed Codex MCP connections and placeholder Kubernetes configuration are
regenerated for each launch; portable history cannot select upstream credentials
or server registrations. Managed Claude tasks and MCP remain gated pending their
own client/tool completion evidence.

| Managed selector | Observed executable pin | Selected native state | Completed-turn observation |
| --- | --- | --- | --- |
| `codex` | 0.161.0 | One same-UUID rollout | Native completed turn, resolved supported tools and initial developer instruction digest |
| `claude-code` | 2.1.27 | One same-UUID primary project transcript | Fresh prompt/tool/Stop hooks plus a settled transcript with paired foreground Bash tools |

The fixed supervisor verifies the executable version before activation and owns
the PTY across attachment disconnects. The selected state and workspace are
published together under `/workspace/session`; personal homes, caches, provider
credentials and previous launch settings are excluded. Every visit supplies
fresh trusted configuration, gateway access and the same approved instructions.
Claude's transcript does not itself prove the approved system instructions or
permissions. Codex's retained initial instruction block is checked against the
approved digest. The destination still requires fresh admission.

Move gates input, waits for the active supported turn/tools, then revokes access
and independently confirms the actual worker container stopped before capture.
Claude uses controlled native process shutdown after its Stop observation;
Codex uses native quit after its completed-turn observation. Hooks and journals
are workload-accessible hints, not gateway authority, sealed accounting or a
whole-container writer fence. Forced cancellation revokes first. See
[ADR 0011](../adr/0011-native-session-compatibility-and-completion.md).

The managed Codex launch uses `--sandbox danger-full-access` because the actual
tool fixture could not start the inner bubblewrap sandbox in the restricted
worker kernel. Claude uses fixed `bypassPermissions` and foreground Bash. Both
rely on the outer non-root, read-only, capability-restricted container and
enforced gateway network boundary. Portable history cannot select these settings.

The pinned Claude parser supports the observed bounded 429 retry record followed
by a genuine successful assistant response. Its error message remains opaque;
it does not supply usage or authority. A retry invalidates the previous assistant
leaf, so unresolved retries cannot reuse earlier completion. Other error-record
shapes, background/parallel tasks, subagents, compaction and imported personal
settings are outside the managed compatibility contract. Input accepted after a
completed Claude turn also prevents reuse of its earlier Stop observation.

## Model providers

Codex, Hermes and the OpenCode example use the existing OpenAI Responses route.
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
OpenAI's output-bound admission can underestimate total usage. Cost-bounded
API-key inference now requires an exact positive input/cached/output price row
before provider work and selects `service_tier: default`, refusing explicit
nondefault tiers. Workload account/project selector headers are removed in both
OpenAI authentication modes. The API-key route still lacks a complete billable-field
allowlist and pricing for all hosted/non-token work. Native client-executed tool
declarations are preserved. Neither route provides a hard provider-spending
guarantee. See [model admission](../guides/model-provider-auth.md#protocol-and-budgets).

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

`make verify-native-session` is a separate native checkpoint prerequisite. It
uses stock Codex 0.161.0 and deterministic loopback providers to resume one UUID
through three fresh homes, transferring a single rollout through the production
capture/restore helper. It checks cumulative conversation and fresh provider
bindings. It excludes native configuration and authentication. This does not
provide a user-facing move command or establish Docker/Kubernetes, terminal,
long-history or tool-state portability. Original developer instructions persist
on `exec resume`; changing them is an incompatible restore until proved. See
[ADR 0008](../adr/0008-selected-native-and-git-checkpoints.md).

The separate [production mobility fixture](../../hack/verify/session-mobility/README.md)
has passed real Docker-to-Kubernetes-to-Docker round trips for both managed pins.
Codex's extended gate verifies a real command tool effect, Git and selected
ignored files, same native UUID/deadline, held-turn graceful movement, forced
held-turn cancellation, accounting replay and administrator network/mount denial
probes. Claude's full round trip verifies three genuine TUI turns, foreground
Bash and its file effect, Git/ignored-file continuity, four stopped visits and
sealed usage from actual Messages requests. These are deterministic providers
through production gateways and placements, not paid-provider or broad
long-history proofs. Claude's round trip alone does not establish Codex's
extended failure and containment coverage. See the
[setup guide](../guides/session-mobility.md) for the public CLI path.

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
A successful local process fixture does not establish deployed CNI behavior.

`make test-e2e` builds pinned Codex 0.161.0, Claude Code 2.1.27, OpenCode 1.15.1
and the official Hermes 0.21.6 image and runs them against the deployed TLS
gateway in isolated Kind with Calico. It also launches the native persistent
Hermes API and scheduler through the task MCP service, checks PVC restart,
retained answers, replay, cancellation, deadlines and direct network denial.
Image provenance and JSON evidence remain in
`.local/verification/orchestration-kind/`. This uses deterministic provider
fixtures, without paid inference or real external channel delivery. See the
[full-stack setup and test guide](../guides/hermes-orchestration.md).

The [harness and provider ADR](../adr/0002-native-cli-harnesses-and-model-protocols.md)
records the choices, rejected alternatives and revisit conditions.

[ADR 0003](../adr/0003-persistent-assistants-and-retained-results.md) records
persistent assistant ownership and retained result publication.
