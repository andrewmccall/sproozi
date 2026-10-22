# Hand-written service configuration

For local Docker sessions, run `sproozi init` and follow the
[local sessions guide](../../docs/guides/local-sessions.md): it writes this
configuration for you. This page is for administrators who write or adjust it
by hand, for example for [session mobility](../../docs/guides/session-mobility.md)
or on [Windows](../../docs/guides/windows.md).

The [service](../../GLOSSARY.md#service) is the `sproozi service run` process
on your machine. It owns a local Docker placement for each configured harness,
with its private gateway, the accounting, the session journal, and HTTPS
connections to administrator-selected
[remote executors](../../GLOSSARY.md#remote-executor). Commands reach it over a
private Unix socket or Windows named pipe. A terminal can detach while the
service and the agent keep running.

## Use a hand-written configuration

Every command reads the installation's `config.json` (`--root`,
`SPROOZI_ROOT`, the recorded root or `~/.sproozi`). For a configuration
elsewhere, pass the hidden `--config FILE` flag; a relative path names a file
from the working directory. `--socket PATH` names the control socket directly,
and with it alone no configuration is read. On macOS and Linux a `--socket`
path too long to bind, or one naming a directory or regular file, fails before
any request.

Run the service in the foreground with:

```sh
sproozi service run --config /absolute/private/config.json
```

`service start` and the session commands start an installation's service for
you; a hand-written configuration is left to you. The
[command reference](../../docs/reference/session-commands.md) lists every
command, and the [remote-work guide](../../docs/guides/portable-workflows.md)
sets up remote executors.

A missing, malformed or unsafe configuration fails with its path and the next
step, for example `no configuration file at PATH`, or `chmod 600 PATH` (`700`
for a directory) for a file that others can read. A file that loads but holds
malformed JSON or an invalid setting is reported by key, such as `local.name:
required`. A file must hold exactly one JSON object: anything after its closing
`}` fails with `configuration PATH: unexpected content after the closing "}"
that ends at byte N; the file must hold exactly one JSON object; ...`. The
configuration must be a regular file, not a directory or symbolic link.
Printed commands quote paths that contain spaces, so you can paste them.

`model` is optional: without it, each harness uses its own default.

On this machine, `move --local` brings a session back to the configured
`local.name` placement, whatever it is called, and output calls it `local`.

## Prepare Docker and private files

Prepare the selected native/helper/anchor images and the administrator-created bridge
network used by `internal/runtime/docker`. The gateway IPv4 must be reachable from
that network, including across Docker Desktop's VM boundary. The anchor permits
worker traffic only to that fixed IPv4 and port. Image IDs are immutable Docker
`sha256:` content IDs, not tags. With a hand-written configuration, the service
neither pulls images nor creates a network; `init` does both.

Keep configuration, provider keys, kubeconfig files and TLS keys outside the worker
workspace. Directories must be owned and mode `0700`; files must be owned, regular,
mode `0600` (or stricter), and selected by absolute path. `dockerConfig` is a fresh
private Docker CLI directory, not a personal Docker configuration. The engine uses
only the configured local Unix socket or Windows named pipe and executable.
The Docker engine must use Linux containers. Windows private files use account
ownership and restrictive ACLs rather than Unix mode bits.

## Configuration reference

The following JSON shows the supported configuration shape. Replace the example
image IDs, paths, instruction digest, model and network address with approved
values. Compute `instructionDigest` as SHA256 of the exact UTF-8 `instructions`
bytes, written as 64 lowercase hex characters without a `sha256:` prefix;
The service reports any other value as `local.instructionDigest (SHA-256 of
local.instructions as 64 lowercase hex characters, without a sha256:
prefix)`. In `allowances`, `bounded` says whether a dimension has a limit at all:
`bounded:true` with a `value` is a limit, and `bounded:false` with `value:0`
is none. It is unrelated to a model call's
[bounded reservation](../../GLOSSARY.md#bounded-reservation) or an
[unbounded call](../../GLOSSARY.md#unbounded-call), which describe how one
request reserves. Unit ceilings must be finite and positive. A
cost limit with `bounded:false` and `value:0` explicitly has no monetary cap;
a bounded cost limit must be positive and requires administrator pricing. A value of the wrong JSON type is reported
by its key, for example `local.gatewayPort: number -1 is not a whole number from
0 to 65535`.

```json
{
  "directory": "/absolute/private/home",
  "dockerBinary": "/absolute/path/to/docker",
  "dockerSocket": "/absolute/path/to/docker.sock",
  "dockerConfig": "/absolute/private/docker-cli",
  "local": {
    "harness": "codex",
    "name": "local",
    "nativeImageID": "sha256:REPLACE_WITH_64_LOWERCASE_HEX",
    "helperImageID": "sha256:REPLACE_WITH_64_LOWERCASE_HEX",
    "anchorImageID": "sha256:REPLACE_WITH_64_LOWERCASE_HEX",
    "networkName": "sproozi-sessions",
    "gatewayIPv4": "192.168.65.254",
    "gatewayPort": 18443,
    "instructions": "Administrator-approved instructions",
    "instructionDigest": "REPLACE_WITH_64_LOWERCASE_HEX",
    "agentLimits": {"MemoryBytes": 1073741824, "CPUMilli": 1000, "PIDs": 256},
    "helperLimits": {"MemoryBytes": 536870912, "CPUMilli": 500, "PIDs": 128},
    "anchorLimits": {"MemoryBytes": 67108864, "CPUMilli": 100, "PIDs": 32},
    "volumeBytes": 536870912,
    "maxCheckpointBytes": 8388608,
    "retainedBytes": 67108864
  },
  "allowances": {
    "model.inference": {
      "units": {"bounded": true, "value": 200000},
      "costMicros": {"bounded": false, "value": 0}
    }
  },
  "maxDuration": "30m",
  "nativeOptions": [],
  "gateway": {
    "listen": "0.0.0.0:18443",
    "certificateFile": "/absolute/private/gateway.crt",
    "keyFile": "/absolute/private/gateway.key",
    "trustBundleFile": "/absolute/private/ca.pem",
    "providerURL": "https://api.openai.com",
    "providerTokenFile": "/absolute/private/provider-token",
    "maxModelCallSeconds": 1200
  }
}
```

Remote executors are not configured here. Remote commands reach a cluster
through a kubeconfig context: `--context NAME` or `--context FILE`, by default
the context named `sproozi`, and `--namespace`. The service records each
[remote executor](../../GLOSSARY.md#remote-executor) it reaches, under the
executor's own name, in `executors.json` in its directory, so `move` and `pull`
keep working after the command exits. See
[remote executor access](../../docs/reference/remote-executors.md). A
configuration that still has a `remote` or `workSources` key fails with
`unknown key "remote": remote executors are no longer configured here`.

`local.name` is 1 to 63 lowercase letters, digits and hyphens, starting and
ending with a letter or digit, like a DNS label, and cannot be `docker` or
`home`; either fails with, for example, `"docker" is reserved: docker and home
are reserved names for this machine's service, and local.name names its
placement`. An unknown key is reported with the key that holds it, such as
`local.agentLimits: unknown key "bogus"`.

### Fields for home-managed certificates, harnesses and shared instructions

These optional fields are what `init` writes:

- `gateway.pkiDirectory` replaces `certificateFile`, `keyFile` and
  `trustBundleFile` with a home-owned certificate authority store. The service
  renews the serving leaf, rotates the authority when every running allocation
  has the new trust or has stopped, and reloads the certificate without
  restarting. Setup creates the store; the service never creates an authority.
  Do not combine it with the three file fields.
- `harnesses` lists further native harnesses. Each entry has `harness`, a
  distinct placement `name` and `gatewayPort`, `model`, optional
  `nativeOptions`, `providerURL`, `providerTokenFile` and optional
  `providerCAFile` and `pricingFile`. It shares the `local` placement's images,
  network, gateway address, instructions, limits, allowances and certificate
  source. Each harness has its own grants and placement directories.
  `run --harness` selects one; `move --local` returns a session to its own
  harness's placement.
- The [profile](../../docs/reference/profile.md) is the `profile` folder
  beside the configuration file; it is not a setting.
- `gateway.listen` may name a loopback address when the engine forwards its host
  address to host loopback, as Docker Desktop does.

## Gateway certificates and provider

The configured TLS certificate must cover both the exact gateway IPv4 as an IP SAN
and the selected model authority as a DNS SAN: `api.openai.com` for Codex or
`api.anthropic.com` for Claude Code. The trusted CA bundle must verify that chain.
The listener can bind the exact IPv4, a loopback address the engine forwards or
`0.0.0.0`, on the configured gateway port.
Workers receive only the CA certificates, a fresh per-visit proxy token and
a placeholder provider key. The provider key is read by the trusted gateway
from `providerTokenFile`; `providerTokenKind` marks a sign-in token (`token`)
or ChatGPT plan sign-in (`chatgpt`) there instead. With no `providerTokenFile`,
the harness signs in itself: the worker gets no placeholder and mounts its
login volume, and the gateway forwards its sign-in
([ADR 0047](../../docs/adr/0047-harnesses-sign-in-to-their-own-model-providers.md)). The service refuses to start while that file still holds
a setup placeholder such as `REPLACE_WITH_PROVIDER_API_KEY`.
`providerURL` accepts HTTPS; HTTP is permitted only for a literal loopback IP used
by a controlled, credential-free verification fixture. Upstream redirects and
environment proxy settings are disabled. If a certificate, key or CA file cannot
be used, the service names its key and path, such as
`gateway.certificateFile /absolute/private/gateway.crt: tls: failed to find any PEM data in certificate input`.
For a private HTTPS provider, `gateway.providerCAFile` selects a private CA bundle
and still verifies the provider's hostname/IP. The mobility fixture uses
`host.py prepare --provider-tls` and `host.py provider --tls`; the home gateway
connects to verified TLS on localhost, while Kind connects to the host gateway IP
with the same certificate's IP SAN. The fixture CA must also be selected by the
remote gateway's administrator-owned upstream TLS configuration.

`gateway.maxModelCallSeconds` sets the
[model call deadline](../../GLOSSARY.md#model-call-deadline): how long one
model call may stream, from 1 to 3600 seconds. Omit it for 1200 (20 minutes);
an explicit 0 is not a way to ask for the default. Any whole JSON number is
accepted, so `1000.0` means 1000. For 0 or any other whole number outside that
range, however large, even `1e1000000000`, the service lists `gateway.maxModelCallSeconds (1 to 3600
seconds; omit it for 1200)` among the settings to fix. A fraction, even one as small
as `1e-1000000000`, a quoted number or any other JSON type except `null` fails with `gateway.maxModelCallSeconds: X is
not a whole number of seconds`, where X is the value as written. JSON `null`
counts as omitted. The home gateway waits 45
seconds for a provider's response headers, and ends a response that sends
nothing for 5 minutes. A call whose headers don't arrive in time is charged an
estimate, because the provider already had the request. See
[long model calls](../../docs/guides/model-provider-auth.md#long-model-calls).

## Native clients

Set `local.harness` explicitly to `codex` or `claude-code`; it must match every
remote executor and runtime used by this session. There is no default or client
switch during a move. Sproozi checks no client version; the repository's client images install the
releases pinned in `hack/clients`. For Claude, use an approved Anthropic model/provider URL, omit
`nativeOptions`, and cover `api.anthropic.com` in the inspection certificate.
The managed Claude launch fixes its tools, permissions and completion hooks;
portable transcripts cannot replace them. Foreground Bash is supported;
background/parallel tasks and imported personal settings are excluded. See
[ADR 0011](../../docs/adr/0011-native-session-compatibility-and-completion.md).
The selected Claude transcript accepts the observed bounded 429 retry shape only
when a later successful assistant response establishes a new leaf. Retry messages
remain opaque and cannot grant access or supply usage. Native context and prompt
snapshots are also history; [ADR 0015](../../docs/adr/0015-default-clients-and-native-tool-schemas.md)
describes the supported records. Unsupported native error
records, subagents, compaction and broad long-history portability are unproved.

### Named MCP

Local Codex sessions can use configured MCP servers; Claude Code sessions get
none. The `gateway.mcp*` keys and `mcp.<name>` allowances are described in
[configured MCP: local sessions](../../docs/reference/configured-mcp.md#local-sessions).

## Remote executors

Every remote executor must use the same checkpoint byte limit. Each one pins its
own approved template, capabilities, ceilings and maximum duration. The
service reaches an executor through a kubeconfig context: the context's
credential goes only to the Kubernetes API server, and the executor receives
only a short-lived `sproozi-cli` token bound to the audience
`sproozi-remote-executor` ([remote executor access](../../docs/reference/remote-executors.md)). Keep approved instructions identical
through every visit: native resume retains the original developer instruction
block. Claude receives the approved instructions through its fresh managed launch;
its transcript cannot authorize instructions, settings or permissions. Restart the service with the same configuration while
visits remain active; changed placement or authority configuration fails closed against
retained records.

## Start the service

Run the service under a user service manager, or keep its process independent
of the terminal. For a local trial, where `/absolute/private/home` is the
configured `directory`:

```sh
umask 077
nohup sproozi service run --config /absolute/private/config.json </dev/null >/absolute/private/home/service.log 2>&1 &
```

`service run` checks in order and exits at the first check that fails. It first
lists every missing or invalid setting by JSON key, including a provider token
file that still holds the placeholder `REPLACE_WITH_PROVIDER_API_KEY`. It then
checks Docker once, before writing anything: `dockerBinary`, `dockerSocket`
and `local.nativeImageID` must be usable. It then checks, still before writing
anything, that the gateway port is free; if a service already answers on this
directory's control socket, it reports `another Sproozi service is using
directory PATH; stop it first` instead of the port conflict. Next it checks
that the gateway listener and certificate match `local.gatewayIPv4`,
`local.gatewayPort` and the harness, and that the `local` placement settings
match the ones saved in the state directory. A change to them is accepted while
there are no execution records, and refused once there are. Finally it binds
the gateway port and control socket, and reports a port or socket already in
use. Each failure is printed after `error: ` and names the setting. While
Docker is down the service can't start, so nothing answers, including
`status` and `cancel` for sessions running in Kubernetes. A `model` or
`local.instructions` of only spaces is refused; values are otherwise used as
written. `dockerSocket` must be an absolute path (on Windows, a non-blank named
pipe), so a value padded with spaces is refused.
[If the service doesn't start](../../docs/guides/session-mobility.md#if-the-service-doesnt-start)
lists each message and its fix. On success it prints `Started the Sproozi
service: control socket PATH, gateway HOST:PORT` to standard error, and on a
clean shutdown, after Ctrl-C or `SIGTERM`, `Stopped the Sproozi service`.
Before it stops, the gateway [drains](../../GLOSSARY.md#drain) its model calls
for up to 10 seconds: each call in flight ends with `endReason`
`gateway_restart`. A call the provider had received is charged as
[interrupted](../../docs/guides/model-provider-auth.md#gateway-restarts). One
still waiting for its budget, not yet fully written or still waiting for its
response headers gets a 503 with `the model gateway is shutting down; retry
the call` (for OpenAI, code `gateway_stopping`; the Anthropic body has no
code); only the last is charged. When it opens its ledger, and every minute
while it runs, the service settles any expired [lease](../../GLOSSARY.md#lease)
a previous run left unsettled.

## Start a session

Stop all writers in the source Git repository before `run`. The command
captures its Git history, branch references, index, tracked changes and ordinary
untracked files. Git hooks and config and personal agent state are excluded.
Ignored files are excluded unless selected with repeatable `--include-ignored
PATH` flags. Keep credentials outside this repository, including ordinary
untracked files, because the selected workspace contents travel to each
placement. The source directory is a seed; later work happens in the managed
Docker workspace.

```sh
sproozi run /absolute/path/to/stopped-repository --config /absolute/private/config.json
```

To carry an individual ignored input file, add `--include-ignored needed.bin`.
The selection persists through every visit and usage receipt. At most 32 clean
relative file paths of 512 bytes each are accepted. Paths into `.git`,
traversal, symlinks and special files are rejected. Capture checks that each
selected file still exists as an ignored regular file; a missing or changed
file type stops the checkpoint rather than silently dropping it.

The session's `id` always names the durable session; `executionId`,
`allocationId` and `incarnation` report the bounded execution and exact visit.
If a request fails after it could have been recorded, the error ends with `the
outcome is unknown: check with "sproozi status ID", and to try again repeat
the command with --operation OP`; `run --id ID --operation OP` retries that
exact start and `recover ID` continues it. The `threadId` is empty until the
first successful stop observes the ID the agent generated.

## Attach, move and return

```sh
sproozi attach ID
```

`attach` prints `Attached to the agent's terminal; Ctrl-] detaches` to standard
error, then shows the agent's terminal. Resizes are forwarded, and Ctrl-]
detaches locally. Ctrl-C reaches the agent. Reattach with the same command; the
supervisor keeps a bounded output tail. Detaching does not cancel a turn or
release allowance. Avoid the agent's own `/quit` if you mean to move the
session: the move owns completed-turn shutdown and the container stop and
capture. Codex uses native quit; Claude uses controlled process shutdown after
its observed Stop and settled transcript. Input accepted after a Claude turn
prevents reuse of its earlier Stop, so finish a new turn before moving.

After a completed turn, move to the remote executor and back:

```sh
sproozi move ID --remote
sproozi attach ID
sproozi move ID --local
sproozi attach ID
```

`move --remote` reaches the executor through the kubeconfig context named
`sproozi`; `--context` names another context or a kubeconfig file, and
`--namespace` the executor's namespace. The service, not the CLI, connects,
and records the executor it reached. See
[remote executor access](../../docs/reference/remote-executors.md).

Move gates new input and waits for the active native turn and supported tools to
complete within the original deadline. Source gateway access remains available
during that wait. After native quiet, the placement revokes access, confirms the
actual container stopped, finalizes usage and captures the workspace/native pair.
The first move also binds the harness conversation ID (`threadId`) generated by the harness and imports the
checkpoint into the home session journal. Only then can the destination prepare and
activate. Later visits preserve that ID, instruction digest, absolute deadline,
ignored-file selection and remaining allowance. Unknown stop or usage keeps
allowance encumbered and blocks destination preparation.

## Recover or cancel

```sh
sproozi status ID
sproozi recover ID
sproozi cancel ID
```

A command refused because an earlier operation is unfinished or cancelled
names the command to run instead, such as `run "sproozi recover ID" to finish
it, or "sproozi cancel ID" to stop it now`. The
[command reference](../../docs/reference/session-commands.md#stopping-removing-and-recovering)
maps each reply to its command.

When the outcome of a move or cancellation is unknown, the error names its
operation. Retry `move ID --operation OP` or `cancel ID --operation OP` with
the same intent, or use `recover`. Cancellation revokes authority and forcibly
stops the active incarnation; it does not require a new completed turn. Native
processes enforce their original absolute deadline even while the service is
unavailable. Stopping the service releases its private ownership locks and socket;
it is not a claim that an execution stopped. Restart with the same configuration
and recover the retained operation.

Remote control requests to a remote executor default to a
45-second timeout. This is separate from the home gateway's 45-second wait for
a model provider's response headers, described in
[long model calls](../../docs/guides/model-provider-auth.md#long-model-calls).
If a turn takes longer,
the response may be uncertain while its input gate remains closed. Use `status`
and `recover` or repeat the exact operation. Do not replace an uncertain move with
a new operation ID or enlarge its allowance or deadline to make it proceed.

## Retention

The service retains bounded initial inputs, placement artifacts and journals.
There is no automatic reset or garbage-collection command: retention exhaustion
fails explicitly. The home directory and Docker volumes are trusted service state
and must not be mounted into workers or edited to refill an allowance.

## Verification evidence

Unit tests exercise initial import, lost activation/stop replies, restart between
import and move, conservative unknown usage, cancellation and partial terminal
writes. Isolated journal tests use synthetic native checkpoints; native adapter
tests also validate captured stock-client transcripts. Live Docker/Kind round-trip acceptance must use the stock-client verifier
and genuine terminal/native evidence; compiling this command is not that proof.
The live fixture passed for stock Codex and Claude Code, with the
same harness conversation ID (`threadId`) and original deadline across placements. Codex's extended proof
includes real tool effects, held moves, forced cancellation, denial probes and
accounting replay. Claude's proof includes three genuine TUI turns, a real Bash
file effect, Git and selected ignored files, four stopped visits and eight actual
Messages requests accounting for 320 finalized fixture units. Deterministic provider
responses establish this selected workflow; paid-provider behavior, arbitrary
tools, compaction and large histories require separate evidence. The four ordinary
run harness selectors do not expand the two-client managed mobility contract.
