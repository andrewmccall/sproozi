# Add a harness

A harness is an existing agent program Sproozi launches. It can join two
tiers; start with the first:

- **AgentRun harness**: an `AgentRuntime.spec.clientConfig.harness` selector.
  Sproozi renders its MCP configuration and launch files into each run's
  immutable contract. Every harness is one.
- **Managed harness**: also a native kind for `sproozi run`, `stop`, `resume`
  and `move`. Sproozi supervises its terminal, observes completed turns and
  checkpoints its native conversation. Codex and Claude Code are the only ones.

The current selectors are listed in the
[harness reference](../reference/harnesses.md#harness-selectors).

Adding a harness is architecturally significant (AGENTS.md): record it in a
new ADR with its alternatives and limits, and update the docs in the same
change.

## The rule: an MCP client

Every harness must load MCP servers over Streamable HTTP, because every run
receives the `sproozi` control connection and its `diagnose` tool
([ADR 0025](../adr/0025-managed-harness-integration-and-diagnose.md)). A
program without an MCP client can't be registered until it has one: add MCP
support upstream, or ship an MCP bridge as one of its launch files
([ADR 0039](../adr/0039-one-harness-registry-and-the-mcp-requirement.md)).

## AgentRun harness

1. Add a `Harness` entry to the registry in
   [`internal/harness/registry.go`](../../internal/harness/registry.go): the
   selector, its MCP file, field and `Entry` renderer, any launch files, and
   whether its launch reads `instructions.txt` and `request.json`.
2. Add the selector to the `+kubebuilder:validation:Enum` marker on
   `RuntimeClientConfig.Harness` in
   [`api/v1alpha1/agentruntime_types.go`](../../api/v1alpha1/agentruntime_types.go)
   and run `make manifests`.
3. Run `go test ./internal/harness/`. Its guard tests fail, naming the file
   to change, until the CRD enum, the registry and the rendered `sproozi`
   control connection agree.
4. Add the client's pinned install under [`hack/clients`](../../hack/clients/README.md)
   and to `internal/runtime/docker/image/clients.Dockerfile` if the release
   images should carry it.
5. Add a stock runtime example under
   [`examples/harnesses`](../../examples/harnesses/README.md).
6. Extend the verifiers that load generated configuration in real clients:
   `hack/verify/harness-clients.py` (`make verify-harness-clients`) and
   `hack/verify/harness-process` (`make verify-harness-process`, including a
   `diagnose` call), and `hack/verify/orchestration-kind` if it runs delegated
   work.
7. Document the harness in the [harness reference](../reference/harnesses.md):
   add it to the selector table and describe its launch and completion. Add a
   glossary term only for a new concept.

## Managed harness

Do the AgentRun steps first, then:

1. In the registry entry, set `Native`, `NativeDirectory` and `Protocol`, the
   model API it calls through the gateway.
2. Accept it in `nativeconfig.ValidateHarness`
   ([`internal/nativeconfig/claude.go`](../../internal/nativeconfig/claude.go)).
3. Add a supervisor adapter to `nativeAdapters` in
   [`cmd/native-session/launch.go`](../../cmd/native-session/launch.go): launch
   arguments, completed-turn observation, controlled stop and refused
   [harness arguments](../../internal/harness/native_args.go).
4. Add its native state capture, validation and restore in
   [`internal/checkpoint/artifact.go`](../../internal/checkpoint/artifact.go)
   and `internal/harness`, with captured transcript fixtures.
5. If its protocol is new, add the gateway model handler and the provider in
   `internal/localsetup`.
6. Run `go test ./internal/harness/ ./cmd/native-session/ ./internal/checkpoint/`;
   the guards name each missing piece. Then prove a real round trip with
   `make verify-local-onboarding` and the session verifiers in
   [which command proves what](../../test/acceptance/running.md#which-command-proves-what).
7. Document its managed behaviour in the harness reference's managed section.

## Gates

Run `make lint-fix`, `make test`, `make verify-generated` and
`make verify-docs`, plus the verifiers above for the tier you added.
