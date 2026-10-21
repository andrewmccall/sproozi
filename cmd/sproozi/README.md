# Sproozi CLI and home daemon

For a local Docker installation, run `sproozi init --local` and follow the
[local sessions guide](../../docs/guides/local-sessions.md). It writes the
configuration described below for you. This page documents that configuration
for administrators who write or adjust it by hand.

`sproozi serve` runs the [home daemon](../../GLOSSARY.md#home-daemon). It owns a
local Docker placement for each configured harness, with its private gateway, and the accounting, the session journal,
and HTTPS connections to administrator-selected
[remote executors](../../GLOSSARY.md#remote-executor). Client
commands use a private Unix socket or Windows named pipe. A terminal attachment can disconnect while
the daemon and selected native process keep running.

## docker, home and local

Your machine appears under three names. `docker` is home's executor name:
`--executor docker` addresses the home daemon, and `sproozi sessions` labels
its rows `"executor": "docker"`. `home` is the home daemon itself: its rows
report `"ownership": "home"`, and `--executor home` is rejected. `local.name`,
which `sproozi init` writes as `"local": {"name": "local"}`, is home's local
placement for `move --to`: `move --to local` brings a session back to Docker,
and `--executor local` is rejected. Home's rows in `sproozi sessions` carry a
`placement` field naming where the session runs or last ran: the `local.name`
value or a name under `remote`. It is empty until the session's first launch. Remote executors take any other name. See
[owner](../../GLOSSARY.md#owner) and [home daemon](../../GLOSSARY.md#home-daemon).

## Commands

Remote-first tasks use `run --executor NAME --repository NAME --task-file
FILE --operation UUID`, followed by `sessions` and `status`. A configured
remote executor accepts these without a running home daemon. Start the home
daemon for `pull SESSION_UUID --from NAME`, `resume SESSION_UUID` and `stop
SESSION_UUID`. `--from NAME` names the remote executor to pull from,
configured under `remote` or `workSources`. Product
commands, including `serve`, read the file `sproozi init` creates: the platform
user configuration directory's `sproozi/config.json`. `--config FILE` selects
another file and `--socket PATH` another home address; a relative path for
either names a file from the working directory. `--config ""` fails with
`--config needs a path`. On macOS and Linux, a `--socket` path too long to
bind fails with `control socket path "PATH" is N bytes; this platform allows
at most M; use a shorter --socket path`, and one that names an existing
directory or regular file fails with `--socket PATH is a directory, not a
socket; name home's control.sock` (or `is a regular file`). The examples
below use the defaults. Each command accepts only its own flags;
`sproozi -h` lists the commands and `sproozi COMMAND -h` prints one command's
flags, spelled with two dashes as `--name`. A remote `--executor` never falls back home; local controls
use `--executor docker` or omit it. Keep operation UUIDs stable on retries.
See the [command and output reference](../../docs/reference/session-commands.md)
for socket overrides, session lists and IDs, and the complete
[retained-work setup](../../docs/guides/portable-workflows.md).

For the local and remote setup sequence, see
[Move a live session between Docker and Kubernetes](../../docs/guides/session-mobility.md).
Both selected clients have passed production Docker-to-Kubernetes-to-Docker round
trips. Codex also passed the extended tool, held-move, forced-cancellation and
denial checks. Claude's round trip covers foreground Bash, workspace continuity
and finalized Messages usage; it does not imply the same extended failure coverage.
The repeatable privileged setup is in the
[fixture README](../../hack/verify/session-mobility/README.md).

## Install and create a configuration

Install `sproozi` with Homebrew or a packaged download, as
[installation](../../docs/guides/install.md) describes, or build it from a
checkout with `go build -o bin/sproozi ./cmd/sproozi`. The examples run
`sproozi` from your `PATH`.
`sproozi --help` and `sproozi --version` do not require configuration. A
release prints its version, commit and build date. A `go build` from a
checkout prints `dev` with the commit it was built from, followed by `with
uncommitted changes` when the tree was modified; a build with no recorded
commit, such as `go run`, prints `dev (source build; use the checkout you built
from)`. `sproozi`
with no command prints the command list to standard error and exits 2.
`sproozi init [--directory PATH]` creates a private configuration directory and,
when `config.json` is missing, a starter configuration with a private `home`
state directory. It never overwrites an existing `config.json`. The starter file
holds the fields every command requires (`directory`, `local.name`,
`local.harness`, `allowances`, `maxDuration`) and an empty `remote` map.
`model` is optional: without it, the harness uses its own default. For local
Docker sessions, `sproozi init --local` writes a complete configuration
instead; otherwise edit the starter for one of two uses:

- Remote work: add an executor under `remote` (see the example below), then run
  `sproozi sessions` and `sproozi run --executor NAME`.
  [Run a task remotely, then resume it locally](../../docs/guides/portable-workflows.md) sets up the remote executor.
- Local sessions: add the Docker paths,
  the `local` images, network, instructions and digest, resource limits and
  storage sizes, and the `gateway` certificates and provider, then run
  `sproozi serve`.
  [Prepare the administrator-owned setup](../../docs/guides/session-mobility.md#prepare-the-administrator-owned-setup)
  builds the images, network and certificates. `remote` can stay empty; add an
  executor to it later to move sessions to that executor's Kubernetes placement.

`serve` names every missing or invalid setting by its JSON key before it starts,
one per line, including each invalid `allowances` entry. Until `serve` runs, `sproozi sessions`
reports home's executor `docker` with `"error": "home_not_running"` and exits
non-zero unless a remote executor answers. It still writes the JSON to standard
output, then explains on standard error that no executor answered. `init` does not start the home
daemon or create credentials. Running it again checks an existing `config.json`,
including its allowances, and reports any problem.

A missing, malformed or unsafe configuration fails with its path and the next
step, for example `no configuration at PATH; run "sproozi init" to create it,
then see DOC`, or `chmod 600 PATH` (`700` for a directory) for a file that others can read.
For a `--config` file named `config.json` in another directory DIR, the step is `run
"sproozi init --directory DIR" to create it`. `init` writes only
`config.json`, so for any other name in a directory D the step is `create it,
for example with "sproozi init --directory D", which writes D/config.json;
then pass --config D/config.json`. If D already holds a `config.json`, the
step is instead `D already holds config.json; pass --config D/config.json, or
run "sproozi init --directory DIR" with another directory and pass --config
DIR/config.json`. `init --directory` naming a file fails with `init
--directory FILE is a file, not a directory; name the directory to hold
config.json`.
A file that loads but holds malformed JSON or an invalid setting is reported by
key, such as `local.name: required`; edit the file to correct it. A file must
hold exactly one JSON object: anything after its closing `}` fails with
`configuration PATH: unexpected content after the closing "}" that ends at byte
N; the file must hold exactly one JSON object; ...`.
The configuration must be a regular file, not a directory or symbolic link.
`sproozi init --local` provisions a complete local installation; see the
[local installation reference](../../docs/reference/local-installation.md).
Printed commands quote paths that contain spaces, so you can paste them. Windows users should read
[Windows setup](../../docs/guides/windows.md) for path and permission rules.

## Prepare Docker and private files

Prepare the selected native/helper/anchor images and the administrator-created bridge
network used by `internal/runtime/docker`. The gateway IPv4 must be reachable from
that network, including across Docker Desktop's VM boundary. The anchor permits
worker traffic only to that fixed IPv4 and port. Image IDs are immutable Docker
`sha256:` content IDs, not tags. With a hand-written configuration, `serve`
neither pulls images nor creates a network; `init --local` does both.

Keep configuration, provider keys, home principal tokens and TLS keys outside the worker
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
`serve` reports any other value as `local.instructionDigest (SHA-256 of
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
  },
  "remote": {
    "engineering": {
      "url": "https://approved-profile.example:8443",
      "tokenFile": "/absolute/private/home-principal-token",
      "caFile": "/absolute/private/remote-ca.pem",
      "maxCheckpointBytes": 8388608
    }
  }
}
```

Each `remote` entry is a [remote executor](../../GLOSSARY.md#remote-executor):
a placement that `move --to NAME` can use and an executor for `run`,
`sessions`, `status`, `cancel`, `recover` and `pull`. `url` is its fixed HTTPS
origin, `tokenFile` the absolute path of its
[home principal token](../../GLOSSARY.md#home-principal-token), `caFile` an optional
private CA (omit it to use system roots) and `maxCheckpointBytes` is 1 byte to
64 MiB and must equal `local.maxCheckpointBytes` once that is set. Every
command checks each entry when it reads the configuration and names the bad
key, such as `remote.engineering.url` or `remote.engineering.maxCheckpointBytes`. Earlier
capitalised keys (`URL`, `TokenFile`, `CAFile`, `MaxCheckpointBytes`) still load.
An entry name is 1 to 63 lowercase letters, digits and hyphens, starting and
ending with a letter or digit, like a DNS label. It cannot be `docker`, home's
executor name, `home`, which names the home daemon, or the same as
`local.name`, home's local placement for `move --to`. `local.name` follows the
same rule and cannot be `docker` or `home`; either fails with, for example,
`"docker" is reserved: docker is home's executor name, home names the home
daemon, and local.name names home's local placement for "move --to"`. An unknown key is reported
with the entry that holds it, such as `remote.engineering: unknown key "bogus"`.

`workSources` takes entries of the same shape. Use it for an executor that is
not a placement, such as a remote executor reached only for remote work through
`run`, `sessions`, `status`, `cancel`, `recover` and `pull`, or to give a
`remote` executor a separately provisioned connection for those commands. When
both maps hold the same name, those commands use the `workSources` entry and
`move --to` uses the `remote` one. Use `remote` alone when one remote executor
is both your placement and your executor.

### Fields for home-managed certificates, harnesses and shared instructions

These optional fields are what `init --local` writes:

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
  `start --harness` selects one; `move --to` with any local placement name
  returns a session to its own harness's placement.
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
from `providerTokenFile`. `serve` refuses to start while that file still holds
a setup placeholder such as `REPLACE_WITH_PROVIDER_API_KEY`.
`providerURL` accepts HTTPS; HTTP is permitted only for a literal loopback IP used
by a controlled, credential-free verification fixture. Upstream redirects and
environment proxy settings are disabled. If a certificate, key or CA file cannot
be used, `serve` names its key and path, such as
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
range, however large, even `1e1000000000`, `serve` lists `gateway.maxModelCallSeconds (1 to 3600
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

### Named MCP for Codex

For Codex, named MCP is optional: configure `gateway.mcpAuthority` as an exact DNS authority
on port 443, `mcpRegistryFile`, `mcpCredentialsDirectory`, and `mcpPolicy` using the
existing named MCP registry and `MCPServerScope` schema. Include corresponding
`mcp.<name>` unit allowances. The inspection certificate must also cover the MCP
authority. Bounded monetary MCP allowances are rejected because trusted MCP cost
pricing is not implemented. Administrator `nativeOptions` supplies the matching
stock Codex MCP client configuration; checkpoint contents supply none of it.

## Remote executors

Every remote executor must use the same checkpoint byte limit. Each one pins its
own approved template, capabilities, ceilings and maximum duration. The home
daemon holds only that remote executor's CA and bearer token, never
Kubernetes credentials. Keep approved instructions identical
through every visit: native resume retains the original developer instruction
block. Claude receives the approved instructions through its fresh managed launch;
its transcript cannot authorize instructions, settings or permissions. Restart the home daemon with the same remote executor configuration while
visits remain active; changed placement or authority configuration fails closed against
retained records.

## Start the daemon

Start the daemon under a user service manager or keep its process independently
of the terminal. For a local trial, where `/absolute/private/home` is the
configured `directory`:

```sh
umask 077
nohup sproozi serve </dev/null >/absolute/private/home/daemon.log 2>&1 &
```

`serve` checks in order and exits at the first pass that fails. It first lists
every missing or invalid setting by JSON key, including a `provider-token`
that still holds the placeholder `REPLACE_WITH_PROVIDER_API_KEY`. It then
checks Docker once, before writing anything: `dockerBinary`, `dockerSocket`
and `local.nativeImageID` must be usable. It then checks, still before writing
anything, that the gateway port is free; if a home daemon answers on this
directory's control socket, it reports `home: another home daemon is using
directory PATH; stop it first ("sproozi serve" holds it until it exits)`
instead of the port conflict. Next it checks that the gateway
listener and certificate match `local.gatewayIPv4`, `local.gatewayPort` and
the harness, and that the `local` placement settings match the ones saved in
the home directory. A change to them is accepted while the home has no
execution records, and refused once it has some. Finally it binds the gateway
port and control socket, and reports a port or socket already in use. Each
failure is printed after `Sproozi failed: ` and names the setting. While
Docker is down, `serve` can't start, so home answers nothing, including
`status` and `cancel` for its sessions running in Kubernetes. A `model` or
`local.instructions` of only spaces counts as missing; values are otherwise
used as written. `dockerSocket` must be an absolute path (on Windows, a
non-blank named pipe), so a value padded with spaces is refused.
[If `serve` doesn't start](../../docs/guides/session-mobility.md#if-serve-doesnt-start)
lists each message and its fix. On success it prints `Started home daemon:
control socket PATH, gateway HOST:PORT` to standard error, and on a clean
shutdown, after Ctrl-C or `SIGTERM`, `Stopped home daemon`. Before it stops,
the home gateway [drains](../../GLOSSARY.md#drain) its model calls for up to
10 seconds: each call in flight ends with `endReason` `gateway_restart`. A
call the provider had received is charged as
[interrupted](../../docs/guides/model-provider-auth.md#gateway-restarts). One
still waiting for its budget, not yet fully written or still waiting for its
response headers gets a 503 with `the model gateway is shutting down; retry
the call` (for OpenAI, code `gateway_stopping`; the Anthropic body has no
code); only the last is charged. When it opens its ledger, and every
minute while it runs, the daemon settles any expired
[lease](../../GLOSSARY.md#lease) a previous daemon left unsettled.

## Start a session

Stop all writers in the source ordinary Git directory before `start`. The command
captures its Git history, branch references, index, tracked changes and ordinary
untracked files. Git hooks/config and personal native client state are excluded. Ignored
files are excluded unless selected with repeatable `--include-ignored PATH` flags.
Keep credentials outside this repository, including ordinary untracked files,
because the selected workspace contents travel to each placement. The source
directory is a seed; later native work happens in the managed Docker workspace.

```sh
sproozi start --workspace /absolute/path/to/stopped-repository
```

To carry an individual ignored input file, add `--include-ignored needed.bin`.
The selection persists through every visit and usage receipt. Retry `start` with
the exact same selection; move/recover commands cannot change it. At most 32 clean
relative file paths of 512 bytes each are accepted. Paths into `.git`, traversal,
symlinks and special files are rejected. Capture checks that each selected file
still exists as an ignored regular file; a missing or changed file type stops the
checkpoint rather than silently dropping it.

After the request succeeds, the client prints the session and operation UUIDs,
then a JSON status. If it fails after the request could have been recorded, the
error ends with `to retry, repeat the command with --id UUID --operation UUID`. Its `id` always names the durable session; `executionId`,
`allocationId` and `incarnation` report the bounded execution and exact visit.
Public `productId` is removed; retained internal journals keep their original
execution identifiers. Keep both UUIDs if a response is lost. `start --id UUID
--operation UUID` retries that exact start; `recover --id UUID` continues its
journaled stage. Initial status has an empty harness conversation ID (`threadId`) until the first
successful stop observes the ID generated by the selected stock client.

## Attach, move and return

```sh
sproozi attach SESSION_UUID
```

`attach` prints `Attached to the harness terminal; Ctrl-] detaches` to standard
error, then shows the selected native TUI. Terminal resize is forwarded, and Ctrl-] detaches locally.
Ctrl-C is forwarded to the native process while attached. Reattach with the same command; the
native supervisor retains a bounded output tail. Detach does not cancel a turn or
release allowance. Avoid native `/quit` if a subsequent move is intended: movement
owns completed-turn shutdown and the independent container stop/capture boundary.
Codex uses native quit; Claude uses controlled process shutdown after its observed
Stop and settled transcript. Accepted input after a Claude turn prevents reuse of
its earlier Stop, so finish a new turn before requesting that graceful move.

After a completed turn, move to the remote executor's placement, named `engineering`
in the example configuration, and return to the local placement, `local.name`:

```sh
sproozi move SESSION_UUID --to engineering
sproozi attach SESSION_UUID
sproozi move SESSION_UUID --to local
sproozi attach SESSION_UUID
```

`--to` takes a placement: `local.name` or a key of `remote`. Other names fail
before a request:

- `move --to docker: "docker" is home's executor name; use --to local (local.name) to move to this machine`
- `move --to home: "home" names the home daemon; use --to local (local.name) to move to this machine`
- `move --to "X" is not a configured placement; use one of: local (local.name), r (remote.r), or add it under "remote" in FILE`

The messages name your configured `local.name` and `remote` entries.
When the command runs without a configuration, for example with only
`--socket`, the home daemon checks the name instead and replies `home: move
destination "X" is not a configured placement; use one of: local, r`. None of
these rejections ends with the retry hint, because nothing was recorded:
correct the name and run `move` again; there is no operation to reuse.

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
sproozi status SESSION_UUID
sproozi recover SESSION_UUID
sproozi cancel SESSION_UUID
```

A command refused because an earlier operation is unfinished or cancelled
names the command to run instead, such as `run "sproozi recover ID" to finish
it, or "sproozi cancel ID" to revoke and force-stop its current execution`. The
[recovery messages](../../docs/reference/session-commands.md#recovery-messages)
table maps each reply to its command.

Move and cancellation commands print their operation UUID once the daemon
accepts the request, and name it in the error otherwise. Retry `move --operation UUID --to NAME` or `cancel --operation UUID` with
the same intent, or use `recover`. Cancellation revokes authority and forcibly
stops the active incarnation; it does not require a new completed turn. Native
processes enforce their original absolute deadline even while the home daemon is
unavailable. Stopping the daemon releases its private ownership locks and socket;
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

The home daemon retains bounded initial inputs, placement artifacts and journals.
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
