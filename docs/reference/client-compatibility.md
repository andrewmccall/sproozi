# Standard client compatibility

Agents can use ordinary command-line tools. The administrator-owned Pod template
supplies proxy settings, trust material and client configuration for `kubectl`,
`git`, `gh` and the approved package client. Every one of these clients,
including `kubectl`, must use the same shared authenticated gateway. No
Sproozi-specific agent command or MCP server is required, and no local
proxy/sidecar is injected.

The optional [Kubernetes MCP connection](kubernetes-mcp.md) exposes one Pod-list
tool through the same gateway and permission checks. [Configured remote MCP
servers](configured-mcp.md) use named `mcp.<server>` capabilities.

## Supported client operations

The approved Codex demo image must contain Codex CLI, kubectl,
git, gh and curl. Supported operations include Kubernetes discovery, Git smart HTTP clone/fetch/push and GitHub
REST PR creation through `gh api --method POST repos/OWNER/REPO/pulls`. The
gateway also implements a narrow GraphQL subset, but `gh pr
create` can use fragments and parent-repository selections outside that
subset and is denied. The demo deliberately uses the REST operation rather than
expanding the GraphQL grammar or weakening repository authorization. PyPI
support is limited to exact, SHA-256-locked wheels when the independent
`packages.install` capability is granted. Unsupported clients or operations must
be denied by the trusted gateway, rather than silently treated as an allowed
host connection.

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
events are returned. The client receives the expected protocol, but events
arrive only after completion. Truncated, failed or ambiguous streams fail
closed. Chat Completions streaming and WebSocket upgrades are unsupported.
Responses token accounting uses the provider's `input_tokens`, `output_tokens`
and `input_tokens_details.cached_tokens` fields, as documented in the [Responses
API](https://developers.openai.com/api/reference/cli/resources/responses/methods/retrieve)
and [streaming
guide](https://developers.openai.com/api/docs/guides/streaming-responses).

A runtime need not contain Codex or the demo's CLI tools. Its external clients
must use the authenticated proxy and trust configuration. Per-run bypass is
unsupported.

## Additional CLI harnesses

Native configuration and trusted launch examples also exist for Claude Code,
OpenCode and Hermes. Claude Code uses the reserved Anthropic Messages model
route; OpenCode
uses the existing OpenAI route with completion normalization for its native
session result. The [harness reference](harnesses.md) describes launch settings,
provider configuration and managed-session compatibility. Anthropic Messages streaming is buffered and usage-checked
before delivery, like the OpenAI Responses path. Bedrock, Vertex, Claude
subscription auth, and Claude Code/OpenCode persistent services and managed
loops are outside the supported configuration.
