# 0071: A Pod gets a key placeholder only for a client that reads it

Status: accepted
Date: 2026-11-20

## Problem and constraints

When the gateway holds a model credential, the harness sends a public
placeholder key and the gateway replaces it
([ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md)). The
controller gave every AgentRun Pod's workload containers
`OPENAI_API_KEY=sproozi-local-placeholder`, unless the runtime named its own
sign-in, whatever client the Pod ran.

Only Codex reads that variable from the Pod, through its provider's
`env_key`. The other AgentRun harnesses supply their own placeholder:
`claude-launch.py` sets Claude Code's `ANTHROPIC_API_KEY`, `hermes-launch.py`
sets Hermes's `OPENAI_API_KEY`, and the OpenCode and Copilot examples set the
key in their launch command (`apiKey`, `COPILOT_PROVIDER_API_KEY`). A managed
session's supervisor launches its harness with an explicit environment from
`harness.NativeBaseEnvironment` and ignores the Pod's. The owner's words, on
2026-11-20: "that key isn't needed for all agents".

Per-harness facts belong in the registry, not in a branch on the harness in
the sandbox code
([ADR 0039](0039-one-harness-registry-and-the-mcp-requirement.md)).

## Decision

- **The Pod gets a key placeholder only for a harness whose client reads it
  from the Pod.** The registry's `RunKeyEnv` names that variable; only Codex
  sets it, to `OPENAI_API_KEY`. `harness.RunKeyEnvironment` returns it, and
  `BuildPodSpec` sets it with the placeholder in every workload container.
- **Otherwise the harness supplies its own placeholder,** in its launch file
  or its runtime's command. The registry guard (`exampleProblem` in
  `internal/harness/registry_test.go`) fails for a harness with no
  `RunKeyEnv` whose example and launch files never set the placeholder.
- **A runtime that names no harness keeps `OPENAI_API_KEY`.** Its command is
  the administrator's own, which Sproozi can't inspect, such as the Codex
  runtime in `config/samples`.
- **A runtime with its own sign-in gets no key placeholder,** as before.

## Alternatives

- **A placeholder under each harness's own key variable** (`KeyEnv`). It would
  give Claude Code's and Copilot's Pods `ANTHROPIC_API_KEY` and
  `COPILOT_PROVIDER_API_KEY`, which they don't get today and which their
  launch helper or command already sets. Rejected: no client needs it.
- **A boolean that reuses `KeyEnv`.** Smaller in the registry, but `KeyEnv` is
  a managed session's variable and is empty for OpenCode and Hermes. A
  separate variable name says which variable the Pod's client reads.
- **Removing the placeholder from runtimes that name no harness too.** It
  would break an administrator's own Codex runtime with no selector, and
  Sproozi can't tell what such a runtime runs.

## Tradeoffs and consequences

- An administrator's own `claude-code`, `opencode`, `copilot` or `hermes`
  runtime whose command relied on the injected `OPENAI_API_KEY` loses it, and
  must set its placeholder itself as the examples do.
- A run's Pod created by the previous controller, whose `SandboxName` status
  write was lost, is refused when the new controller adopts it, as `existing
  sandbox ... has unsafe Pod settings: containers`. Every change to
  `BuildPodSpec` does this (`ensureSandbox` in
  `internal/controller/execution.go`, `validateExistingSandbox` in
  `internal/runtime/kubernetes/sandbox.go`). A run whose `SandboxName` was
  recorded never compares its Pod again.

## Revisit conditions

- `OPENAI_BASE_URL` is still set in every Pod, whatever its harness; only
  Codex and runtimes that name no harness may read it.
- A second harness whose client reads its key from the Pod.

## Links

- [AgentRuntime reference](../reference/agentruntime.md)
- [Harness reference: model providers](../reference/harnesses.md#model-providers)
- `internal/harness/registry.go` (`RunKeyEnv`), `internal/harness/launch.go`
  (`RunKeyEnvironment`), `internal/runtime/kubernetes/sandbox.go`
  (`BuildPodSpec`), `internal/runtime/kubernetes/sandbox_credentials_test.go`
