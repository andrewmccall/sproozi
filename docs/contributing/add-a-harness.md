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

Terms used below: the **run contract** is the immutable ConfigMap Sproozi
writes for each run and mounts read-only at `/etc/sproozi/contract`; a
**launch file** is a file the registry adds to that contract for the
harness's launch command; an **MCP bridge** is a launch file that gives a
client without MCP support the run's MCP connections.

## The rule: an MCP client

Every harness must load MCP servers over Streamable HTTP, because every run
receives the `sproozi` control connection and its `diagnose` tool
([ADR 0025](../adr/0025-managed-harness-integration-and-diagnose.md)). A
program without an MCP client can't be registered until it has one: add MCP
support upstream, or ship an MCP bridge as one of its launch files
([ADR 0039](../adr/0039-one-harness-registry-and-the-mcp-requirement.md)).
Each registry entry declares which in `Loader`: `NativeMCP` when the stock
client reads its MCP file, or `Bridge` naming the launch file that does.

## AgentRun harness

0. Write the ADR first. Adding a harness is architecturally significant
   (AGENTS.md): record the decision, the MCP route, alternatives and limits
   in a new ADR under `docs/adr/`, and update the docs in the same change.
1. Add a `Harness` entry to the registry in
   [`internal/harness/registry.go`](../../internal/harness/registry.go): the
   selector, its MCP file, field and the function that renders each
   connection (`Entry`), its `Loader`, any launch files (embedded with
   `go:embed` in `internal/harness/launch.go`), and whether its launch reads
   `instructions.txt` and `request.json`.
2. Add the selector to the `+kubebuilder:validation:Enum` marker on
   `RuntimeClientConfig.Harness` in
   [`api/v1alpha1/agentruntime_types.go`](../../api/v1alpha1/agentruntime_types.go)
   and run `make manifests`.
3. Run `go test ./internal/harness/`. Its guard tests fail, naming the file
   to change, until the CRD enum, the registry, the rendered `sproozi`
   control connection, the declared loader, the example and the reference row
   agree. A bridge also runs through the test hook below; a native client is
   checked only by the verifiers in step 6.
4. Add the client's pinned install under [`hack/clients`](../../hack/clients/README.md).
   Two Dockerfiles install clients: `internal/runtime/docker/image/clients.Dockerfile`
   builds the released native image for managed sessions, so only a managed
   harness belongs there; `hack/verify/orchestration-kind/clients.Dockerfile`
   builds the image the Kind verifiers run AgentRuns in, so add every
   AgentRun harness the verifiers exercise.
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

## What the guards check, and what they don't

`go test ./internal/harness/ ./cmd/native-session/ ./internal/checkpoint/`
fails, naming the file to change, unless:

- the registry, the `+kubebuilder:validation:Enum` marker and the generated
  CRD list the same selectors, and an unregistered selector is refused;
- every harness's run contract has its MCP file with the requested servers
  and the `sproozi` connection, and the Sproozi integration files;
- every harness declares exactly one loader; a bridge is a non-empty embedded
  launch file with a runtime, and every launch file has contents;
- every bridge passes the test hook below, when its runtime is on `PATH`;
- every harness has a stock runtime example that selects it and a row in the
  reference's selector table with the right Managed value;
- every managed kind has its own native directory and a protocol, is accepted
  by `nativeconfig`, and has a supervisor adapter and a checkpoint format.

They don't prove that a stock client loads its MCP file or that a launch
completes. Review those, and run the verifiers:
`make verify-harness-clients` loads generated configuration in installed
Codex, Claude Code and OpenCode (not Hermes, and not bridges);
`make verify-harness-process` runs Claude Code and OpenCode through the
gateway, including `diagnose`. To verify a bridge-based harness, run the hook,
then load the bridge in the real client with configuration rendered by
`go run ./hack/verify/mcp-config --harness NAME`: check that its tools appear
as `<connection>__<tool>` and that it refuses to start without `sproozi`, and
record that evidence in the ADR. Client lockfiles, Dockerfiles, verifier
lists and documentation prose are reviewed by hand.

## Writing an MCP bridge

A bridge is a launch file: the registry embeds it, Sproozi writes it into the
run contract next to the rendered MCP file, and the runtime's launch command
loads it into the client, for example as an extension or plugin. Both land
in `/etc/sproozi/contract`. The bridge must:

- read the rendered MCP file, such as `/etc/sproozi/contract/NAME-mcp.json`,
  and refuse to start when it has no `sproozi` connection;
- reach each URL through the sandbox's environment: the runtime's
  `HTTPS_PROXY`, and the injected trust bundle (`NODE_EXTRA_CA_CERTS`,
  `SSL_CERT_FILE`). Node's `fetch` uses the proxy only with
  `NODE_USE_ENV_PROXY=1`;
- open one MCP session per call over Streamable HTTP: `initialize`, then
  `notifications/initialized`, then the request with the returned
  `Mcp-Session-Id`, accepting a JSON or event-stream reply;
- expose each tool as `<connection>__<tool>`, such as `sproozi__diagnose`;
- support `BRIDGE --sproozi-check CONFIG`, which loads every connection, prints
  `{"tools":["<connection>__<tool>", ...]}` and exits, or exits nonzero
  without `sproozi`.

[`internal/harness/testdata/example-mcp-bridge.mjs`](../../internal/harness/testdata/example-mcp-bridge.mjs)
shows each part. The test hook in `internal/harness/bridge_test.go` renders
the harness's MCP file against a fake MCP server, runs
`Loader.Runtime BRIDGE --sproozi-check CONFIG`, and requires the tools of
every connection and the refusal without `sproozi`. It skips, and says so,
when the runtime, such as `node`, is not on `PATH`; install it to run the
hook.

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
`make verify-generated` regenerates code and manifests and fails if that
changes your working tree, so it works before you commit; when it fails, the
regenerated files are already in place to review.
