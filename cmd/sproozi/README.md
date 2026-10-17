# Sproozi CLI and home service

`sproozi serve` owns one local Docker placement, its private gateway and accounting,
the home coordinator, and administrator-selected remote HTTPS profiles. Client
commands use its private Unix socket. A terminal attachment can disconnect while
the daemon and selected native process keep running.

Remote-first Codex tasks use `run --executor NAME --repository NAME --task-file
FILE --operation UUID`, followed by `sessions` and `status`. A configured remote
owner accepts these without a running home daemon. Start the home for `pull
SESSION_ID --from NAME`, `resume SESSION_ID` and `stop SESSION_ID`. Product
commands accept `--config FILE`, defaulting to the platform user configuration
directory's `sproozi/config.json`. Each command accepts only its own flags;
`sproozi -h` lists the commands and `sproozi COMMAND -h` prints one command's
flags. A remote `--executor` never falls back home; local controls
use `--executor docker` or omit it. Keep operation UUIDs stable on retries.
See the [command and output reference](../../docs/reference/session-commands.md)
for socket overrides, session lists and IDs, and the complete
[retained-work setup](../../docs/guides/portable-workflows.md).

For the local and remote setup sequence, see
[Move a native session between local and remote execution](../../docs/guides/session-mobility.md).
Both selected clients have passed production Docker-to-Kubernetes-to-Docker round
trips. Codex also passed the extended tool, held-move, forced-cancellation and
denial checks. Claude's round trip covers foreground Bash, workspace continuity
and sealed Messages usage; it does not imply the same extended failure coverage.
The repeatable privileged setup is in the
[fixture README](../../hack/verify/session-mobility/README.md).

Build the executable:

```sh
go build -o bin/sproozi ./cmd/sproozi
```

Prepare the selected native/helper/anchor images and the administrator-created bridge
network used by `internal/runtime/docker`. The gateway IPv4 must be reachable from
that network, including across Docker Desktop's VM boundary. The anchor permits
worker traffic only to that fixed IPv4 and port. Image IDs are immutable Docker
`sha256:` content IDs, not tags. This command neither pulls images nor creates a
network automatically.

Keep configuration, provider keys, profile tokens and TLS keys outside the worker
workspace. Directories must be owned and mode `0700`; files must be owned, regular,
mode `0600` (or stricter), and selected by absolute path. `dockerConfig` is a fresh
private Docker CLI directory, not a personal Docker configuration. The engine uses
only the configured local Unix socket and executable.

The following JSON shows the supported configuration shape. Replace the example
image IDs, paths, instruction digest, model and network address with approved
values. Compute `instructionDigest` as SHA256 of the exact UTF-8 `instructions`
bytes. Unit ceilings must be finite and positive. A cost limit with `bounded:false`
and `value:0` explicitly has no monetary cap; a bounded cost limit must be positive
and requires administrator pricing.

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
  "model": "ADMINISTRATOR_SELECTED_MODEL",
  "nativeOptions": [],
  "gateway": {
    "listen": "0.0.0.0:18443",
    "certificateFile": "/absolute/private/gateway.crt",
    "keyFile": "/absolute/private/gateway.key",
    "trustBundleFile": "/absolute/private/ca.pem",
    "providerURL": "https://api.openai.com",
    "providerTokenFile": "/absolute/private/provider-token"
  },
  "remote": {
    "remote": {
      "URL": "https://approved-profile.example:8443",
      "TokenFile": "/absolute/private/remote-token",
      "CAFile": "/absolute/private/remote-ca.pem",
      "MaxCheckpointBytes": 8388608
    }
  }
}
```

The configured TLS certificate must cover both the exact gateway IPv4 as an IP SAN
and the selected model authority as a DNS SAN: `api.openai.com` for Codex or
`api.anthropic.com` for Claude Code. The trusted CA bundle must verify that chain.
The listener can bind the exact IPv4 or `0.0.0.0`, on the configured gateway port.
Workers receive only the CA certificates, a fresh per-allocation proxy token and
a placeholder provider key. The provider key is read by the trusted gateway.
`providerURL` accepts HTTPS; HTTP is permitted only for a literal loopback IP used
by a controlled, credential-free verification fixture. Upstream redirects and
environment proxy settings are disabled.
For a private HTTPS provider, `gateway.providerCAFile` selects a private CA bundle
and still verifies the provider's hostname/IP. The mobility fixture uses
`host.py prepare --provider-tls` and `host.py provider --tls`; the home gateway
connects to verified TLS on localhost, while Kind connects to the host gateway IP
with the same certificate's IP SAN. The fixture CA must also be selected by the
remote gateway's administrator-owned upstream TLS configuration.

Set `local.harness` explicitly to `codex` or `claude-code`; it must match every
remote profile and runtime used by this session. There is no default or client
switch during a move. Client packages use upstream defaults without version selectors. For Claude, use an approved Anthropic model/provider URL, omit
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

For Codex, named MCP is optional: configure `gateway.mcpAuthority` as an exact DNS authority
on port 443, `mcpRegistryFile`, `mcpCredentialsDirectory`, and `mcpPolicy` using the
existing named MCP registry and `MCPServerScope` schema. Include corresponding
`mcp.<name>` unit allowances. The inspection certificate must also cover the MCP
authority. Bounded monetary MCP allowances are rejected because trusted MCP cost
pricing is not implemented. Administrator `nativeOptions` supplies the matching
stock Codex MCP client configuration; checkpoint contents supply none of it.

Every remote profile must use the same checkpoint byte limit. Its separate profile
service pins its own approved template, capabilities, ceilings and maximum
duration. The home service holds only that profile's CA and bearer token, never
Kubernetes credentials. Keep approved instructions identical
through every visit: native resume retains the original developer instruction
block. Claude receives the approved instructions through its fresh managed launch;
its transcript cannot authorize instructions, settings or permissions. Restart the home service with the same profile while allocations remain
active; changed placement or authority configuration fails closed against
retained records.

Start the daemon under a user service manager or keep its process independently
of the terminal. For a local trial:

```sh
umask 077
mkdir -p /absolute/private/home
chmod 700 /absolute/private/home
nohup bin/sproozi serve --config /absolute/private/session.json \
  </dev/null >/absolute/private/home/daemon.log 2>&1 &
```

Stop all writers in the source ordinary Git directory before `start`. The command
captures its Git history, branch references, index, tracked changes and ordinary
untracked files. Git hooks/config and personal native client state are excluded. Ignored
files are excluded unless selected with repeatable `--include-ignored PATH` flags.
Keep credentials outside this repository, including ordinary untracked files,
because the selected workspace contents travel to each placement. The source
directory is a seed; later native work happens in the managed Docker workspace.

```sh
bin/sproozi start --socket /absolute/private/home/control.sock \
  --workspace /absolute/path/to/stopped-repository
```

To carry an individual ignored input file, add `--include-ignored needed.bin`.
The selection persists through every visit and sealed receipt. Retry `start` with
the exact same selection; move/recover commands cannot change it. At most 32 clean
relative file paths of 512 bytes each are accepted. Paths into `.git`, traversal,
symlinks and special files are rejected. Capture checks that each selected file
still exists as an ignored regular file; a missing or changed file type stops the
checkpoint rather than silently dropping it.

The client prints the session and operation UUIDs before sending the request, then
prints a JSON status. Its `id` always names the durable session; `executionId`,
`allocationId` and `incarnation` report the bounded execution and exact visit.
Public `productId` is removed; retained internal journals keep their original
execution identifiers. Retain both UUIDs if a response is lost. `start --id UUID
--operation UUID` retries that exact start; `recover --id UUID` continues its
journaled stage. Initial status has an empty native UUID until the first successful
stop observes the UUID generated by the selected stock client.

```sh
bin/sproozi attach --socket /absolute/private/home/control.sock --id SESSION_UUID
```

Use the selected native TUI. Terminal resize is forwarded, and Ctrl-] detaches locally.
Ctrl-C is forwarded to the native process while attached. Reattach with the same command; the
native supervisor retains a bounded output tail. Detach does not cancel a turn or
release budget. Avoid native `/quit` if a subsequent move is intended: movement
owns completed-turn shutdown and the independent container stop/capture boundary.
Codex uses native quit; Claude uses controlled process shutdown after its observed
Stop and settled transcript. Accepted input after a Claude turn prevents reuse of
its earlier Stop, so finish a new turn before requesting that graceful move.

After a completed turn, move to the fixed remote profile and return:

```sh
bin/sproozi move --socket /absolute/private/home/control.sock \
  --id SESSION_UUID --to remote
bin/sproozi attach --socket /absolute/private/home/control.sock --id SESSION_UUID
bin/sproozi move --socket /absolute/private/home/control.sock \
  --id SESSION_UUID --to local
bin/sproozi attach --socket /absolute/private/home/control.sock --id SESSION_UUID
```

Move gates new input and waits for the active native turn and supported tools to
complete within the original deadline. Source gateway access remains available
during that wait. After native quiet, the placement revokes access, confirms the
actual container stopped, seals usage and captures the workspace/native pair.
The first move also binds the native UUID generated by the native client and imports the
checkpoint into the coordinator. Only then can the destination prepare and
activate. Later visits preserve that UUID, instruction digest, absolute deadline,
ignored-file selection and remaining allowance. Unknown stop or usage keeps
allowance encumbered and blocks destination preparation.

```sh
bin/sproozi status --socket /absolute/private/home/control.sock --id SESSION_UUID
bin/sproozi recover --socket /absolute/private/home/control.sock --id SESSION_UUID
bin/sproozi cancel --socket /absolute/private/home/control.sock --id SESSION_UUID
```

Move and cancellation commands print their operation UUID before calling the
daemon. Retry `move --operation UUID --to NAME` or `cancel --operation UUID` with
the same intent, or use `recover`. Cancellation revokes authority and forcibly
stops the active incarnation; it does not require a new completed turn. Native
processes enforce their original absolute deadline even while the home daemon is
unavailable. Stopping the daemon releases its private ownership locks and socket;
it is not a claim that an execution stopped. Restart with the same configuration
and recover the retained operation.

Remote control requests default to a 45-second timeout. If a turn takes longer,
the response may be uncertain while its input gate remains closed. Use `status`
and `recover` or repeat the exact operation. Do not replace an uncertain move with
a new operation ID or enlarge its budget/deadline to make it proceed.

The home service retains bounded initial inputs, placement artifacts and journals.
There is no automatic reset or garbage-collection command: retention exhaustion
fails explicitly. The home directory and Docker volumes are trusted service state
and must not be mounted into workers or edited to refill an allowance.

Unit tests exercise initial import, lost activation/stop replies, restart between
import and move, conservative unknown usage, cancellation and partial terminal
writes. Isolated journal tests use synthetic native checkpoints; native adapter
tests also validate captured stock-client transcripts. Live Docker/Kind round-trip acceptance must use the stock-client verifier
and genuine terminal/native evidence; compiling this command is not that proof.
The live fixture passed for stock Codex and Claude Code, with the
same native UUID and original deadline across placements. Codex's extended proof
includes real tool effects, held moves, forced cancellation, denial probes and
accounting replay. Claude's proof includes three genuine TUI turns, a real Bash
file effect, Git and selected ignored files, four stopped visits and eight actual
Messages requests accounting for 320 sealed fixture units. Deterministic provider
responses establish this selected workflow; paid-provider behavior, arbitrary
tools, compaction and large histories require separate evidence. The four ordinary
run harness selectors do not expand the two-client managed mobility contract.
