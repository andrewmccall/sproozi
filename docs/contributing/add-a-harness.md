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
client without MCP support the run's MCP connections; a **completion
adapter** is a launch file that runs the client and decides the container's
exit status; a **guard test** is a Go test that fails, naming the file to
change, when part of a harness is missing; the **bridge hook** is the guard
test that runs a bridge against a fake MCP server; **managed wiring** is the
per-kind code a managed harness needs outside the registry. Other terms are in
the [glossary](../../GLOSSARY.md).

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
   in a new ADR, using the format and next number in
   [`docs/adr/README.md`](../adr/README.md). Name the exact client package and
   version you pinned and tested; packages move (pi's moved to
   `@earendil-works/pi-coding-agent`).
1. Add a `Harness` entry to the registry in
   [`internal/harness/registry.go`](../../internal/harness/registry.go): the
   selector, its MCP file, field and the function that renders each
   connection (`Entry`), its `Loader`, any launch files, and whether its
   launch reads `instructions.txt` and `request.json`. Put every launch file
   in [`internal/harness/launchfiles`](../../internal/harness/launchfiles) and
   list it with `launchFilesNamed`: that directory is embedded in the binary
   and is the one `.dockerignore` ships to image builds, so a launch file
   anywhere else breaks the manager image build.
2. Add the selector to the `+kubebuilder:validation:Enum` marker on
   `RuntimeClientConfig.Harness` in
   [`api/v1alpha1/agentruntime_types.go`](../../api/v1alpha1/agentruntime_types.go)
   and run `make manifests`.
3. Run `go test ./internal/harness/ ./internal/runtime/kubernetes/` while
   you iterate; `make test` runs everything. The guard tests fail, naming the
   file to change, until the CRD enum, the registry, the rendered `sproozi`
   control connection, the declared loader, the example and the reference row
   agree. A bridge also runs through the bridge hook below; a native client is
   checked only by the verifiers in step 6.
4. Add the client's pinned install under [`hack/clients`](../../hack/clients/README.md).
   Two Dockerfiles install clients: `internal/runtime/docker/image/clients.Dockerfile`
   builds the released native image for managed sessions, so only a managed
   harness belongs there; `hack/verify/orchestration-kind/clients.Dockerfile`
   builds the image the Kind verifiers run AgentRuns in, so add every
   AgentRun harness the verifiers exercise.
5. Add `examples/harnesses/agentruntime-NAME.yaml`, starting from
   [`agentruntime-opencode.yaml`](../../examples/harnesses/agentruntime-opencode.yaml).
   It must be a valid AgentRuntime (the guard decodes it strictly) that sets
   `spec.clientConfig.harness` and the trust bundle ConfigMap, and its
   container must: build the proxy from the gateway token
   (`HTTPS_PROXY` and friends); give the client a placeholder provider key,
   never a real one; read the MCP file, bridge or launch file from
   `/etc/sproozi/contract/`; pass the Sproozi instructions and access skill;
   for a Node client or bridge, set `NODE_USE_ENV_PROXY=1`, which needs
   Node 22.21 or 24; and give a client that waits on an open standard input
   `</dev/null`.
6. Extend the verifiers that load generated configuration in real clients:
   `hack/verify/harness-clients.py` (`make verify-harness-clients`) and
   `hack/verify/harness-process` (`make verify-harness-process`, including a
   `diagnose` call), and `hack/verify/orchestration-kind` if it runs delegated
   work.
7. Document the harness in the [harness reference](../reference/harnesses.md):
   add it to the selector table and describe its launch and completion. Add a
   glossary term only for a new concept. Add it to the harness lists in
   `README.md` and `AGENTS.md` only once it is verified end to end with a
   real client.

## Completion and result

The run's outcome is the `agent` container's exit status: zero is
`Succeeded`, anything else `Failed` ([run contract](../reference/agent-contract.md)).
If the stock client exits nonzero whenever its task fails, the example can
`exec` it and trust that status, as Codex and Claude Code do. If it can exit
zero after an API error or lose its final state, ship a completion adapter as
a launch file, as `opencode-launch.py` and `hermes-launch.py` do: it runs the
client, checks its native result and sets the exit status, without
interpreting the answer. To return a final answer, the adapter writes the
run's termination-file envelope, a small JSON result bound to the run's UID
described in [retained final answers](../reference/agent-contract.md#retained-final-answers).

## What the guards check, and what they don't

`go test ./internal/harness/ ./cmd/native-session/ ./internal/checkpoint/`
fails, naming the file to change, unless:

- the registry, the `+kubebuilder:validation:Enum` marker and the generated
  CRD list the same selectors, and an unregistered selector is refused;
- every harness's run contract has its MCP file with the requested servers
  and the `sproozi` connection, and the Sproozi integration files;
- every harness declares exactly one loader; a bridge is one of its launch
  files with a runtime; every launch file is a non-empty file in
  `internal/harness/launchfiles`, which `.dockerignore` re-includes;
- every bridge passes the bridge hook below, when its runtime is on `PATH`;
- every harness has a stock runtime example that strictly decodes as an
  AgentRuntime selecting it and whose container reads its MCP, bridge or
  launch file from the contract, and a row in the reference's selector table
  with the right Managed value;
- every managed kind has its own native directory and a protocol, is accepted
  by `nativeconfig`, and has a supervisor adapter and a checkpoint format.

They don't prove that a stock client loads its MCP file or that a launch
completes. Review those, and run the verifiers:
`make verify-harness-clients` loads generated configuration in installed
Codex, Claude Code and OpenCode (not Hermes, and not bridges);
`make verify-harness-process` runs Claude Code and OpenCode through the
gateway, including `diagnose`. To verify a bridge-based harness, run the hook,
then load the bridge in the real client with configuration rendered by
a local fake MCP server:

```sh
go run ./hack/verify/fake-mcp --harness NAME --dir /tmp/sproozi-mcp-check
```

It writes `/tmp/sproozi-mcp-check/NAME-mcp.json` with a `github` and a
`sproozi` connection on `127.0.0.1`, prints every request it receives, and runs
until Ctrl-C. Start the real client with that file in place of the contract's,
check that it lists `github__list_issues` and `sproozi__diagnose`, ask it to
call `sproozi__diagnose`, and check that it refuses to start once you delete
the `sproozi` entry. Record that evidence in the ADR. The same command checks
a native client by hand; `hack/verify/mcp-config` renders the in-cluster
gateway addresses instead, which only a sandbox can reach. Client lockfiles, Dockerfiles, verifier
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
- support `BRIDGE --sproozi-check CONFIG`, which loads every connection, calls
  `sproozi__diagnose` through `tools/call`, prints
  `{"tools":["<connection>__<tool>", ...],"diagnose":"<its text>"}` and exits,
  or exits nonzero without `sproozi`.

One file serves both modes: the client imports it as a plugin and calls its
exported functions, and `node BRIDGE --sproozi-check CONFIG` runs it as a
command. Tell them apart by the `--sproozi-check` argument, not by comparing
the module's path with `process.argv[1]`, which symbolic links such as macOS
`/tmp` break silently.

[`internal/harness/testdata/example-mcp-bridge.mjs`](../../internal/harness/testdata/example-mcp-bridge.mjs)
shows each part. The bridge hook in `internal/harness/bridge_test.go`
renders the harness's MCP file against the fake MCP server in
`internal/harness/mcpfake`, runs `Loader.Runtime BRIDGE --sproozi-check CONFIG`,
and requires every connection's tools, a `diagnose` round trip and the
refusal without `sproozi`; failures include the bridge's standard error. When
the runtime, such as `node`, is not on `PATH` it prints a notice and skips,
and fails instead when `CI` is set.

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
