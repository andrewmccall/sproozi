# 0046: GitHub Copilot CLI is an AgentRun harness in bring-your-own-key mode

Status: accepted
Date: 2026-10-22

## Problem and constraints

People want to run GitHub Copilot CLI as a Sproozi agent. The gateway holds
every provider credential and inspects every model call
([ADR 0001](0001-mcp-capabilities-through-shared-gateway.md)), and every harness
needs an MCP client for the `sproozi` control connection
([ADR 0039](0039-one-harness-registry-and-the-mcp-requirement.md)).

By default Copilot signs in to GitHub with a token and sends model calls to
GitHub's Copilot service. The gateway has no handler for that service, and
putting a GitHub token in the sandbox would break the rule that provider
credentials stay on the gateway.

Pinned and tested: `@github/copilot` 1.0.102 (`hack/clients/copilot`).

## Decision

- **An AgentRun harness, `copilot`, not managed.** The registry entry renders
  `copilot-mcp.json` in Copilot's own `mcpServers` format, each connection
  `{"type":"http","url":...,"tools":["*"]}`, and the stock client loads it
  (`NativeMCP`) through `--additional-mcp-config @FILE`. No bridge is needed.
- **Bring-your-own-key mode against the existing OpenAI Responses route.** The
  example sets `COPILOT_PROVIDER_BASE_URL=https://api.openai.com/v1`,
  `COPILOT_PROVIDER_WIRE_API=responses` and a placeholder
  `COPILOT_PROVIDER_API_KEY`. The gateway supplies the real key, as for
  OpenCode. No GitHub token enters the sandbox.
- **Offline mode.** `COPILOT_OFFLINE=true` stops GitHub sign-in, telemetry, the
  built-in GitHub MCP server and auto-update; `--disable-builtin-mcps` and
  `--no-remote` say the same on the command line. Remote MCP servers still load
  in offline mode.
- **The exit status is the outcome; no completion adapter.** Copilot exits 1
  after a failed or refused model call and 0 after a completed turn, so the
  example `exec`s it, as Claude Code's does.
- **Instructions through `$COPILOT_HOME/copilot-instructions.md`.** Copilot has
  no system prompt flag. It adds that file to its system prompt, so the
  example writes the approved instructions and both integration files there
  and passes `request.json` as the prompt. `--no-custom-instructions` would
  also drop that file, so the example doesn't pass it, and Copilot also loads
  workspace instruction files such as `AGENTS.md`, as Claude Code loads
  `CLAUDE.md`.

## Evidence

With 1.0.102 on macOS arm64:

- `make verify-harness-clients` (`--harness copilot`): `copilot mcp get --json`
  reports the rendered `docs`, `inventory` and `sproozi` connections as
  enabled HTTP servers.
- `make verify-harness-process` (`--harness copilot`) runs the stock client
  through the authenticated TLS gateway with a deterministic Responses
  provider: the success scenario calls `fixture-verify` and returns the
  marker, with 30 model units and 1 tool unit charged; the denied scenario
  exits 1 with no model request; the diagnose scenario's shell `curl` is
  refused and journaled, and `sproozi-diagnose` returns that refusal.
- `make test-e2e` in an owned Kind cluster with Calico runs the example
  runtime from the pinned client image: the success run calls the fixture
  tool through the gateway and ends `Succeeded`; the capability-denied and
  exhausted-budget runs end `Failed` with the gateway's refusals audited.
- A local capture of the provider request showed
  `$COPILOT_HOME/copilot-instructions.md` inside a `<custom_instruction>` block
  of the Responses `instructions`.

Copilot's TLS stack refuses a CA certificate served as a leaf
(`CaUsedAsEndEntity`). The gateway already serves leaves signed by its
inspection CA; the `harness-process` fixture now does the same.

## Alternatives

- **Copilot's own model service with a GitHub token.** Since added by
  [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md), which
  routes Copilot's model hosts and its `api.github.com` account requests. At
  the time, it would use the
  person's Copilot subscription and model catalogue, but would need a new
  gateway handler for GitHub's Copilot endpoints and their token exchange,
  and either a GitHub token in the sandbox or gateway-side token injection
  for a service whose wire contract isn't documented for third parties.
  Rejected for now.
- **Anthropic Messages in bring-your-own-key mode.** Copilot supports
  `COPILOT_PROVIDER_TYPE=anthropic`, and the gateway has that route. The
  example uses OpenAI to match OpenCode's; an administrator can switch the
  three environment variables.
- **A managed harness.** Needs a native state format, a completed-turn
  observer and a checkpoint; there is no request for movement yet.
- **A launch adapter that builds the instructions.** Unneeded: four lines of
  shell write the file, and the exit status is already trustworthy.

## Consequences and limits

- Only the Kind client image installs it, not the released native image,
  because it isn't a managed harness.
- No real-model run is recorded; the evidence uses a deterministic provider.
- A repository's own instruction files reach the agent's system prompt after
  the Sproozi file. They can contradict it but can't remove it.
- Copilot releases often, so the canary will report drift most weeks.

Revisit if the gateway gains a GitHub Copilot model route, if Copilot adds a
system prompt flag or a way to disable workspace instructions alone, or if
someone needs to move Copilot sessions.

Implementation: [registry](../../internal/harness/registry.go),
[runtime example](../../examples/harnesses/agentruntime-copilot.yaml),
[harness reference](../reference/harnesses.md) and
[`harness-process`](../../hack/verify/harness-process/main.go).
