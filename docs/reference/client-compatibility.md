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

## Bounded GraphQL reads

The GraphQL route accepts the named `RepositoryInfo` and `PullRequestByNumber`
queries with exact declared variables and repository/PR argument bindings. It
validates the selected fields and forwards a reconstructed document. Fields may
be reordered or omitted within these allowed selections:

| Operation and declared variables | Allowed fields |
| --- | --- |
| `RepositoryInfo($owner: String!, $name: String!)` | `id`, `name`, `nameWithOwner`, `owner { login }`, `defaultBranchRef { name }` |
| `PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!)` | `id`, `number`, `url`, `title`, `body`, `state`, `headRefName`, `headRefOid`, `baseRefName`, `repository { nameWithOwner }` |

Aliases, directives, fragments, additional variables, argument rebinding, extra
fields and trailing operations are rejected. This is a finite client contract;
registering an allowed repository does not authorize arbitrary GraphQL. The
existing narrow PR mutation path has its own validation and repository read-back.
Use the bounded REST `gh api` workflow above for the pinned gh client.

## Model responses

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

## Additional CLI harnesses

Native configuration and trusted launch examples also exist for Claude Code,
OpenCode and Hermes. Claude Code uses the reserved Anthropic Messages model
route; OpenCode
uses the existing OpenAI route with completion normalization for its native
session result. [Current harness support](harnesses.md) records configuration,
provider setup and local mocked-provider evidence. Full isolated Kind acceptance
launches all four stock loops and native persistent Hermes delegation with
deterministic provider fixtures. The earlier SRE acceptance remains
Codex-specific. Anthropic Messages streaming is buffered and usage-checked
before delivery, like the OpenAI Responses path. Bedrock, Vertex, Claude
subscription auth, and Claude Code/OpenCode persistent services and managed
loops remain unverified.
