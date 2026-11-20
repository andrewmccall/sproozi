# Add a harness

A harness is an existing agent program Sproozi launches. It can join two
tiers; start with the first:

- **AgentRun harness**: an `AgentRuntime.spec.clientConfig.harness` selector.
  Sproozi renders its MCP configuration and launch files into each run's
  immutable contract. Every harness is one.
- **Managed harness**: also a native kind for `sproozi run`, `stop`, `resume`
  and `move`. Sproozi supervises its terminal, observes completed turns and
  checkpoints its native conversation. Codex, Claude Code and Copilot are
  managed; Copilot ([ADR 0048](../adr/0048-github-copilot-cli-is-a-managed-harness.md))
  is the most recent example to follow.

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

For example, pi has had its own MCP client since 0.99.0. With 1.4.0,
`pi mcp list --json` connected this file, read from
`$PI_CODING_AGENT_DIR/mcp.json`, and listed the server's tools:

```json
{"mcpServers": {"gitmcp": {"type": "http", "url": "https://gitmcp.io/andrewmccall/sproozi", "exposure": "direct"}}}
```

Without `"exposure": "direct"`, pi offers MCP tools only through its
`codemode` tool, so the model may never call `diagnose`. `pi --print` exited 1
when it couldn't reach its provider; whether it does after a call the gateway
refuses mid-turn is untested.

## AgentRun harness

0. Draft the ADR first, with status `proposed`, and fill in its evidence
   before review; a reviewer accepts it. Name it
   `docs/adr/NNNN-short-title.md` with the next free number, copy the headings
   of a recent ADR such as 0048, and add a row to the index.
   Adding a harness is architecturally significant
   (AGENTS.md): record the decision, the MCP route, alternatives and limits
   in a new ADR, using the format and next number in
   [`docs/adr/README.md`](../adr/README.md). Name the exact client package and
   version you pinned and tested; packages move (pi's moved to
   `@earendil-works/pi-coding-agent`). Check the current release for an MCP
   client before you write a bridge: pi gained one in 0.99.0.
1. Add a `Harness` entry to the registry in
   [`internal/harness/registry.go`](../../internal/harness/registry.go): the
   selector, its MCP file, field and the function that renders each
   connection (`Entry`), its `Loader`, any launch files, and whether its
   launch reads `instructions.txt` and `request.json`. Set `RunKeyEnv` only
   when the client reads its key placeholder from the Pod's environment
   rather than from its launch file or command. Put every launch file
   in [`internal/harness/launchfiles`](../../internal/harness/launchfiles) and
   list it with `launchFilesNamed`: that directory is embedded in the binary
   and is the one `.dockerignore` ships to image builds, so a launch file
   anywhere else breaks the manager image build.
2. Add the selector to the `+kubebuilder:validation:Enum` marker on
   `RuntimeClientConfig.Harness` in
   [`api/v1alpha1/agentruntime_types.go`](../../api/v1alpha1/agentruntime_types.go)
   and run `make manifests`.
3. Run `go test ./internal/harness/ ./cmd/native-session/ ./internal/checkpoint/ ./cmd/sproozi/ ./internal/runtime/kubernetes/` while
   you iterate; `make test` runs everything. The guard tests fail, naming the
   file to change, until the CRD enum, the registry, the rendered `sproozi`
   control connection, the declared loader, the example and the reference row
   agree. A harness whose client needs a bridge also runs through the bridge
   hook below; one whose client loads MCP itself is checked only by the
   verifiers in step 6.
4. Add the client's pinned install under [`hack/clients`](../../hack/clients/README.md).
   This step and step 6 install the real client, so they need access to its
   package registry, such as npm.
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
   (`HTTPS_PROXY` and friends); give the client a placeholder provider key
   such as `sproozi-local-placeholder`, never a real one; read the MCP file, bridge or launch file from
   `/etc/sproozi/contract/`; pass the Sproozi instructions and access skill;
   for a Node client or bridge, set `NODE_USE_ENV_PROXY=1`, which needs
   Node 22.21 or 24; and give a client that waits on an open standard input
   `</dev/null`.
6. Extend the verifiers that load generated configuration in real clients:
   `hack/verify/harness-clients.py` (`make verify-harness-clients`) and
   `hack/verify/harness-process` (`make verify-harness-process`, including a
   `diagnose` call), and `hack/verify/orchestration-kind` if it runs delegated
   work. A client's list command may connect to each server and fail when one
   is unreachable, as `pi mcp list` does; read its JSON output instead of its
   exit status.
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

`go test ./internal/harness/ ./cmd/native-session/ ./internal/checkpoint/ ./cmd/sproozi/ ./internal/runtime/kubernetes/`
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
  AgentRuntime selecting it, sets the trust bundle ConfigMap, sets
  `HTTPS_PROXY` and mentions its MCP, bridge or launch file under
  `/etc/sproozi/contract/` (the guard can't tell whether the command really
  reads it), and a row in the reference's selector table with the right
  Managed value;
- every managed kind has its own native directory and a protocol, is accepted
  by `nativeconfig`, and has a supervisor adapter and a checkpoint format;
- every managed kind supplies every managed fact listed in the managed
  harness steps (an AgentRun-only harness supplies none), exactly one is the
  default, each sign-in choice is worded and names a mechanism the harness
  has, its supervisor adapter launches the registry's `Executable`, and
  the file names a Python launch file repeats equal the registry's
  (`TestManagedKindsSupplyEveryFact` and the tests beside it in
  `internal/harness/registry_test.go`);
- a tripwire for the common form of a fact kept outside the registry
  (`TestNoHarnessFactOutsideTheRegistry` in `internal/harness/sites_test.go`)
  holds: non-test Go code under `internal`, `cmd`, `api` and `test` compares
  nothing with a `NativeKind` constant (`==` or `!=`), lists none in a
  `case`, a `[]NativeKind` literal or as a composite literal key or map
  index, and converts no string literal to a `NativeKind`, except in the
  behaviour scopes (file and function) listed in `behaviourSites`, where a
  new harness adds its cases without editing the list; and no string in
  `internal/harness` or `cmd/native-session` outside the registry repeats a
  registered credential variable, token placeholder, instructions file,
  executable, sign-in file or MCP file name. It does not catch a comparison
  with a converted string such as `string(kind) == "copilot"`, a local
  constant or variable aliasing a kind, a map keyed or indexed by kind
  without a kind constant, a switch on another registry value such as
  `DisplayName`, prefix checks, values repeated in other packages, or
  anything in `_test.go` files, `zz_generated*` files or outside those
  directories, such as verifiers under `hack/`; review those. Read a new
  fact from the registry entry, adding a field when it has none;
- the [configured MCP reference](../reference/configured-mcp.md) names every
  managed harness in its local session and remote executor rows, and the
  `RemoteExecutor` harnesses as those an executor starts tasks and
  interactive sessions for;
- what each fact's readers return, for every registered selector and an
  unregistered kind, is unchanged: `internal/harness/testdata/registry-outcomes.json`
  and `cmd/sproozi/testdata/wizard-outcomes.json` record it. A new harness
  changes both: regenerate them with `SPROOZI_UPDATE_OUTCOMES=1` and review
  the difference. Expect the new harness's own rows; the top-level
  `selectors`, `managedKinds` and `managedKindList` (and `taskKindList` or
  `default` when it takes tasks or is the default); its refused flag
  spellings and reserved variables, recorded for every kind; and the
  empty-kind wizard rows, whose agent question lists every managed harness.
  Any other change is an outcome the new harness altered for another.

They don't prove that a stock client loads its MCP file, that it shows the
MCP tools to the model (pi, for example, hides them behind its `codemode`
tool unless the file sets `exposure: direct`), or that a launch completes. Review those, and run the verifiers:
`make verify-harness-clients` loads generated configuration in installed
Codex, Claude Code, OpenCode and Copilot (not Hermes, and not bridges);
`make verify-harness-process` runs Claude Code, OpenCode and Copilot through the
gateway, including `diagnose`. To verify a bridge-based harness, run the hook,
then load the bridge in the real client with configuration rendered by
a local fake MCP server:

```sh
go run ./hack/verify/fake-mcp --harness NAME --dir /tmp/sproozi-mcp-check
```

It writes `/tmp/sproozi-mcp-check/NAME-mcp.json` with a `github` and a
`sproozi` connection on `127.0.0.1`, prints every request it receives, and runs
until Ctrl-C. Start the real client with that file in place of the contract's,
check that it lists the `list_issues` and `diagnose` tools under its own
naming (for example `github__list_issues`, or pi's `mcp__github__list_issues`)
and ask it to call `diagnose`. For a bridge, also check that it refuses to
start once you delete the `sproozi` entry; a client that loads MCP itself may
skip a missing server, so record what it does instead. Record that evidence in the ADR. The same command checks
a harness whose client loads MCP itself by hand; `hack/verify/mcp-config` renders the in-cluster
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
   model API it calls through the gateway, then every managed fact. The guard
   test fails on a missing one, so a new harness never takes another's value:
   - `DisplayName` and `Product`, the names messages and the setup wizard show;
   - `Executable`, the stock client's path in the managed sandbox image;
   - `InstructionsFile`, its user-level instructions file name;
   - `KeyEnv`, the variable that carries the placeholder credential, and
     `TokenEnv` with `TokenPlaceholder` when a gateway-held sign-in token uses
     another; `SignInYieldsGitHubToken` when it prefers `GH_TOKEN` to its own
     sign-in;
   - `RefusedFlags`, the command-line options Sproozi controls (the tables in
     [`internal/harness/native_args.go`](../../internal/harness/native_args.go));
   - `ReservedEnv`, the variables its supervisor sets and a launch may not;
   - `TakesTask`, `RemoteExecutor` and `Default`, when each applies;
   - `KeyOptions` and `SignInOptions`, any native options a local installation
     gives it; and `SignInFile` and `SignInProvider` when it signs in itself;
   - `SignInChoices`, the answers the setup wizard offers for how it reaches
     its provider, most common first, each with its wording and its
     `SignInAnswer`: a key the gateway holds, a ChatGPT plan the gateway
     holds (OpenAI harnesses only), a sign-in token the gateway holds, or its
     own sign-in (which needs `SignInFile`).

   Every reader of these facts takes them from the registry, so this entry
   is the only place to set them; `TestNoHarnessFactOutsideTheRegistry`
   trips on the common forms of code elsewhere that branches on a kind
   instead (see the guard list below for what it misses).
2. Accept it in `nativeconfig.ValidateHarness`
   ([`internal/nativeconfig/claude.go`](../../internal/nativeconfig/claude.go)).
3. Add a supervisor adapter to `nativeAdapters` in
   [`cmd/native-session/launch.go`](../../cmd/native-session/launch.go): launch
   arguments, completed-turn observation and controlled stop. Its `executable`
   is the registry's `Executable`; add a variable for it and for its native
   home to `cmd/native-session/homes.go`, and add its graceful-stop wait text
   to `waitingFor` in `cmd/native-session/main.go`: it has branches for
   Copilot and Claude Code only, and any other kind gets Codex's, which checks
   for Codex's journal.
   If the client reports turns through hooks that run a `native-session`
   command, as Claude Code's `observe-claude` does, that command must report
   once its payload is whole, not wait for the end of its input: the client
   may close the hook's input late, and a hook its timeout kills reports
   nothing, which leaves the turn running. Test it with input that stays
   open, as `TestClaudeHookReportsACompletePayloadWithoutEndOfInput` does.
4. Add its native state capture, validation and restore in
   [`internal/checkpoint/artifact.go`](../../internal/checkpoint/artifact.go)
   and `internal/harness`, with captured transcript fixtures, and read its
   final answer from that state in `FinalText`
   ([`internal/checkpoint/result.go`](../../internal/checkpoint/result.go)),
   which otherwise returns no result for it.
5. If its protocol is new, add the gateway model handler and the provider in
   `internal/localsetup`.
6. Run `go test ./internal/harness/ ./cmd/native-session/ ./internal/checkpoint/ ./cmd/sproozi/`;
   the guards name each missing piece. Then prove a real round trip with
   `make verify-local-onboarding` and the session verifiers in
   [which command proves what](../../test/acceptance/running.md#which-command-proves-what).
7. Document its managed behaviour in the harness reference's managed section.
   Add its `DisplayName` to the local session and remote executor rows of the
   [configured MCP reference](../reference/configured-mcp.md), and, when it
   sets `RemoteExecutor`, to the sentence after them that names the
   harnesses an executor starts tasks and interactive sessions for;
   `TestConfiguredMCPReferenceMatchesTheRegistry` fails until they agree.

## Gates

Run `make lint-fix`, `make test`, `make verify-generated` and
`make verify-docs`, plus the verifiers above for the tier you added.
`make verify-generated` regenerates code and manifests and fails if that
changes your working tree, so it works before you commit; when it fails, the
regenerated files are already in place to review.
