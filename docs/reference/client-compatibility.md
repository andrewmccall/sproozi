# Standard client compatibility

The sandbox interface is ordinary command-line tooling. The administrator-owned
Pod template exposes
standard proxy, trust, and client configuration to `kubectl`, `git`, `gh`, and
the approved package client. Every one of these clients, including `kubectl`,
must use the same shared authenticated gateway. No Sproozi-specific agent
command or MCP server is required, and no local proxy/sidecar is injected.

The optional [Kubernetes MCP connection](kubernetes-mcp.md) delivers one Pod-list
tool through the same proxy and semantic grant. Its [recorded Kind acceptance](../demos/verified-mcp-demo.md)
used stock Codex 0.154.0 and verified network denials, cancellation and scoped cleanup.
[Configured remote MCP tools](configured-mcp.md) use the same proxy through named
`mcp.<server>` capabilities. Local official-SDK tests cover two providers; the
stock Codex configuration check verifies generated connection loading without
a model request. A separate Kind acceptance run used stock Codex 0.154.0 with
two configured HTTPS fixture providers, including denials, budgets and cancellation.

## Verified demo client path

The selected digest-pinned Codex demo image must contain Codex CLI, kubectl, git,
gh and curl (the latter is used by the acceptance probes). The compatibility surface is limited to Kubernetes discovery, Git
smart HTTP clone/fetch/push, and bounded GitHub REST PR creation through
`gh api --method POST repos/OWNER/REPO/pulls`. The gateway also implements a
narrow GraphQL subset, but the pinned gh 2.97.0 `gh pr create` command uses
fragments and parent-repository selections outside that subset and is denied.
The demo deliberately uses the REST operation rather than expanding the
GraphQL grammar or weakening repository authorization. PyPI support is limited to exact,
SHA-256-locked wheels when the independent `packages.install` capability is
granted. Unsupported clients or operations must be denied by the trusted
gateway, rather than silently treated as an allowed host connection.

The model path accepts JSON responses and Responses API SSE. SSE is buffered
within the gateway's body and time limits until a single successful
`response.completed` event supplies usage; the budget is settled before any
events are returned. This preserves the client protocol but delays the events
until completion. Truncated, failed or ambiguous streams fail closed. Chat
Completions streaming and WebSocket upgrades are unsupported. Responses token
accounting uses the provider's `input_tokens`, `output_tokens` and
`input_tokens_details.cached_tokens` fields, as documented in the
[Responses API](https://developers.openai.com/api/reference/cli/resources/responses/methods/retrieve)
and [streaming guide](https://developers.openai.com/api/docs/guides/streaming-responses).

[Recorded acceptance](../demos/verified-sre-demo.md) exercised this semantic
client path on 4 October 2026 from a dirty working tree. Other images, client
versions and configurations require their own verification. A generic runtime
need not contain Codex or the demo's CLI suite, but its external clients must use
the authenticated proxy and trust configuration. No per-run bypass is supported.
