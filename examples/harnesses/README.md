# Stock CLI harness runtimes

These administrator-owned runtime examples launch Claude Code, OpenCode or Hermes as the
`agent` process in a disposable Sproozi Pod. Replace each `example/...@sha256:...`
image with a reviewed, digest-pinned image containing `/bin/sh`, `cat` and the
selected stock CLI. The OpenCode image also needs Python 3. Set `SPROOZI_MODEL` to a provider model available to your
account. Use only model identifiers made of letters, digits, dots, underscores
and hyphens in the OpenCode example because its trusted command builds JSON.

The examples retain the Codex runtime's Pod security, gateway trust and bounded
writable volumes. They create fresh client state under `/home/agent`, load the
immutable contract's selected MCP configuration, pass trusted instructions
separately from request JSON, and replace the shell with the CLI or its narrow completion adapter. Select the
runtime in `AgentTemplate.spec.runtimeRef`; policy and requested capabilities
continue to grant authority. CLI permission flags permit local execution inside
the Pod. They do not grant gateway access.

OpenCode uses OpenAI Responses through `api.openai.com`. Its configured provider
uses the bundled `@ai-sdk/openai` integration. Project configuration, discovered
skills, default plugins, model catalog downloads, sharing and LSP downloads are
disabled. The runtime never attaches to an execution server outside the Pod. The trusted
`opencode-launch.py` asset supervises one stock invocation and checks its native
export before exiting. OpenCode 1.15.1's raw exit code can report zero after an
API error, so the adapter reports failure when terminal session evidence is
missing or contains a native error. It requires fresh state and does not judge
task quality.

Claude Code uses Anthropic Messages through `api.anthropic.com`, with API-key
billing configured on the gateway. Claude subscription authentication, Bedrock,
Vertex, managed loops and remote-control sessions are outside these examples.
`--setting-sources ''` and `--strict-mcp-config` restrict settings and MCP loading;
hooks and slash-command discovery are disabled. No existing sessions are mounted.

These CLIs receive harmless model-key placeholders. Provider keys belong only to
the gateway. The gateway certificate must cover the selected provider authority,
and the mounted trust bundle must contain its inspection CA. Direct network
access remains subject to the existing enforcing CNI requirement.

Run `make verify-harness-clients` to check generated configuration loading with
locally installed stock clients in disposable state. This check performs no model
request. See [harness reference](../../docs/reference/harnesses.md) for configuration,
provider setup and the evidence limits of these examples.


Hermes uses the pinned official 0.21.6 image. The bounded worker launches its
bundled interpreter directly, outside the root startup system, with fresh native
state. Its immutable helper supervises the stock CLI and publishes a bounded
UID-bound final answer. The example enables the native Kubernetes MCP tool;
request `kubernetes.read` and configure its namespace/resource policy to use it.
`make test-e2e` checks the deployed stock loops and persistent coordinator with
deterministic providers. The [Hermes Kubernetes guide](../../docs/guides/hermes-orchestration.md)
provides the persistent deployment and schedule setup.
