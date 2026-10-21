# Session commands and identities

The public executable is `sproozi`. [Install the CLI](../guides/install.md)
covers Homebrew, downloads and source builds. The examples run `sproozi` from
your `PATH` with the default configuration and home address; see
[Select the owner](#select-the-owner) for `--config` and `--socket`.

These commands require administrator configuration, approved images and prepared
execution environments. Building the CLI does not install a cluster or start an
agent.

A [session](../../GLOSSARY.md#session) tracks the harness conversation across executions. Each
execution needs separate approval and has its own deadline and usage allowance.
Each [visit](../../GLOSSARY.md#visit) to a placement gets a new visit ID
(`allocationId`) and backend resource identity. Knowing a session ID or harness conversation ID (`threadId`) doesn't give
you permission to run it.

Use [Run a task remotely, then resume it locally](../guides/portable-workflows.md)
for remote task setup, pull and local resume, or
[Move a live session between Docker and Kubernetes](../guides/session-mobility.md)
for interactive moves within one bounded execution. The
[home configuration reference](../../cmd/sproozi/README.md) describes private files,
images, gateway credentials and remote executors.

## Remote work and local resume

`sproozi --help` lists commands and exits 0; `sproozi` with no command prints
the same list to standard error and exits 2. `sproozi --version` prints the
product version, commit and build date; a source build prints `dev` and the
commit it was built from, or says to use the checkout you built from. Neither reads home configuration.
`sproozi init [--directory PATH]` creates or validates a private directory for
configuration and credentials. When `config.json` is missing, it also writes a
starter configuration that client commands accept and prints the next step
for each use: `sproozi init --local` for local Docker sessions, a remote
executor under `remote` for remote work, or the Docker, local placement and
gateway settings and `sproozi serve` for a hand-built local configuration, as
the [home configuration reference](../../cmd/sproozi/README.md) shows. Each
next step links its setup guide. The starter names no model, so the harness
uses its own default. It never overwrites an existing `config.json`; it
checks one and exits non-zero if it is malformed, unsafe, a directory or a
symbolic link, or has an invalid `remote`, `workSources` or `allowances` entry. It does not
start a service. The default
is the platform user configuration directory's `sproozi` directory.
`sproozi init --local` instead provisions a complete local installation; see
[local installation commands](#local-installation-commands).

Home is the [home daemon](../../GLOSSARY.md#home-daemon): the `sproozi serve`
process on your own machine. A command that finds no configuration reports the
path it read and suggests `sproozi init`. For a missing `--config` file not
named `config.json`, in a directory D, it suggests `create it, for example
with "sproozi init --directory D", which writes D/config.json; then pass
--config D/config.json`, or, when D already holds a `config.json`, `D already
holds config.json; pass --config D/config.json, or run "sproozi init
--directory DIR" with another directory and pass --config DIR/config.json`. A
malformed file is reported with its
path and the key to correct, and a file or directory with unsafe permissions with the `chmod` that
fixes it. Printed commands quote paths that contain spaces. A command that needs
home when it isn't running says so and suggests `sproozi service start` for a
local installation, otherwise `sproozi serve`. A command that changes state
names its session and operation UUIDs for the retry when its outcome is
unknown; a request home refused before recording anything, or that no home
daemon received, has no retry hint.

Local commands use a private Unix socket on macOS and Linux, or a private named
pipe on Windows. See [Windows setup](../guides/windows.md) for native platform
requirements and workspace limits.

| Command | Purpose |
| --- | --- |
| `run` | Submit a task to a configured remote executor. |
| `sessions` | List accessible sessions, optionally filtered by executor or Project. |
| `status` | Inspect one session by its stable session UUID. |
| `pull` | Claim a stopped remote session and import its conversation and Git workspace. This does not start an agent. |
| `resume` | Start a new local execution of a stopped session with fresh permissions. It attaches the terminal unless `--detach` is selected. |
| `stop` | Stop local work and retain its supported state for later resume. |
| `cancel` | Revoke and force-stop a session's current execution, including unfinished work that cannot stop gracefully. |
| `recover` | Continue a journaled operation after an uncertain response or interruption. |

The harness conversation has its own UUID. It is separate from Sproozi's
session and execution IDs. See [core concepts](../explanations/core-concepts.md)
and [Projects and workspace revisions](projects-and-revisions.md) for the basic
model and exact-commit launches.

## Interactive sessions and movement

`serve` starts the home daemon. It first names, by JSON key, every setting
that is missing or invalid, and exits if there are any. It then checks Docker
once and exits if Docker, the native image or the gateway listener and
certificate are unusable; see
[If `serve` doesn't start](../guides/session-mobility.md#if-serve-doesnt-start).
Once its control
socket and gateway are listening, it prints `Started home daemon: control
socket PATH, gateway HOST:PORT` to standard error, with one `HOST:PORT` per
configured harness, and on a clean shutdown,
after Ctrl-C or `SIGTERM`, `Stopped home daemon`. `start` seeds a managed Docker
workspace from a stopped Git repository and attaches its native terminal unless
`--detach` is selected. `attach` reconnects the terminal, and `move` transfers the
supported state to a configured placement. `move --to NAME` with any local
placement name returns a session to the local placement of its own harness. These
client commands accept the configuration and socket selectors described below.

Detaching the terminal does not stop the daemon or agent. Running a container on
your laptop still requires the laptop and Docker engine to remain available.
Follow
[Move a live session between Docker and Kubernetes](../guides/session-mobility.md)
for the complete setup, commands, supported clients and movement limits.

## Local installation commands

These commands manage an installation created by `init --local`. They select it
with `--root PATH`, defaulting to the recorded root or `~/sproozi`. The
[local sessions guide](../guides/local-sessions.md) shows them in use and the
[local installation reference](local-installation.md) lists what they create.

| Command | Purpose |
| --- | --- |
| `init --local` | Provision or repair the installation for one harness and start the home daemon. Repeat it with another `--harness` to add that harness. |
| `service status`, `service start`, `service stop` | Report, start or stop the detached home daemon. `stop` refuses while sessions are active unless `--leave-running` is given. |
| `doctor` | Check permissions, configuration, [profile](profile.md), Docker, image, network, credentials, certificates, the service and the restricted worker path. It changes no installation state. |
| `credentials add openai`, `credentials add anthropic` | Store or replace a provider key, then restart the service if no session is active. |
| `images status`, `images prepare` | Show the installed and release images, or pull or build an image. A prepared image replaces the installed one only while no local session state exists. |
| `certificates status`, `renew`, `rotate-ca`, `replace-ca` | Show certificate state, renew the leaf now, begin routine rotation, or replace a lost or compromised authority. `replace-ca` refuses while sessions are active. |

Results go to standard output, as JSON with `--json`; progress goes to standard
error. A failure names its component (`docker`, `image`, `registry`, `network`,
`tls`, `credential`, `provider`, `config`, `profile`, `service` or `root`) and the next
step to try.

## Command flags

Each command accepts only the flags listed here. `sproozi -h` lists the
commands, and `sproozi COMMAND -h` prints one command's usage and flags,
spelled with two dashes as `--name`. Any
other flag is rejected before configuration is read or a request is sent.

| Command | Flags |
| --- | --- |
| `init` | `--directory`, `--local`, `--root`, `--harness`, `--model`, `--instructions-file`, `--image`, `--build`, `--provider-url`, `--provider-ca-file`, `--credential-file`, `--credential-stdin`, `--skip-credential-check`, `--port`, `--no-service`, `--json`, `--timeout` |
| `service` | `--root`, `--json`, `--leave-running` |
| `doctor` | `--root`, `--json`, `--check-provider` |
| `credentials` | `--root`, `--credential-file`, `--credential-stdin`, `--skip-credential-check` |
| `images` | `--root`, `--image`, `--build`, `--json` |
| `certificates` | `--root`, `--json` |
| `version` |  |
| `run` | `--config`, `--executor`, `--operation`, `--harness`, `--repository`, `--project`, `--task`, `--task-file`, `--workspace-from`, `--checkpoint-digest`, `--revision` |
| `sessions` | `--config`, `--socket`, `--executor`, `--project`, `--json` |
| `status` | `--config`, `--socket`, `--executor`, `--id` |
| `pull` | `--config`, `--socket`, `--executor`, `--id`, `--operation`, `--from` |
| `resume` | `--config`, `--socket`, `--executor`, `--id`, `--operation`, `--detach`, `--json` |
| `stop` | `--config`, `--socket`, `--executor`, `--id`, `--operation`, `--json` |
| `cancel` | `--config`, `--socket`, `--executor`, `--id`, `--operation` |
| `recover` | `--config`, `--socket`, `--executor`, `--id` |
| `serve` | `--config` |
| `start` | `--config`, `--socket`, `--executor`, `--id`, `--operation`, `--workspace`, `--harness`, `--include-ignored`, `--detach`, `--json` |
| `attach` | `--config`, `--socket`, `--executor`, `--id` |
| `move` | `--config`, `--socket`, `--executor`, `--id`, `--operation`, `--to` |

| Flag | Meaning |
| --- | --- |
| `--directory PATH` | Private configuration directory; defaults to the platform user configuration directory plus `sproozi`. A path that names a file fails with `init --directory FILE is a file, not a directory; name the directory to hold config.json`. |
| `--config FILE` | Private global configuration; defaults to the platform path described below. An empty value fails with `--config needs a path`. |
| `--socket PATH` | Private home socket, overriding the configured home. On macOS and Linux a path too long to bind fails with `…; use a shorter --socket path` (see [read status and discovery](#read-status-and-discovery)), and a path that names a directory or regular file fails with `--socket PATH is a directory, not a socket; name home's control.sock` (or `is a regular file`). |
| `--executor NAME` | [Owner](../../GLOSSARY.md#owner) to address: `docker` (home's executor name), or a name under `remote` or `workSources`. `run` accepts only a remote executor. See [Select the owner](#select-the-owner). |
| `--id UUID` | Stable session UUID. Commands that take a session also accept it as a positional argument, before or after the flags. |
| `--operation UUID` | Stable operation UUID. Reuse it when retrying a mutation. It must be a non-zero UUID in lowercase 8-4-4-4-12 hexadecimal form; any other value fails with, for example, `--operation "1" is not a canonical UUID: use a non-zero UUID in lowercase 8-4-4-4-12 hexadecimal form, such as 3f2b8c1e-7d4a-4e5b-9c6d-0a1b2c3d4e5f`. Session UUIDs follow the same rule. |
| `--from NAME` | Remote executor to pull from, configured under `remote` or `workSources`. Without it, `pull` fails with `pull requires --from NAME naming the remote executor to pull from, configured under "remote" or "workSources" in <config>`; an unknown name fails with `pull --from "X" names no remote executor configured under …`. |
| `--harness NAME` | Native harness, `codex` or `claude-code`. For `run`, it defaults to `local.harness`. For `start`, it selects a configured local harness for the new session and defaults to the primary one; a session keeps its harness. For `init --local`, it selects the harness to add and defaults to `codex`. |
| `--repository NAME` | Approved repository for `run`. |
| `--project ID` | Approved Project for `run`, or a filter for `sessions`. |
| `--task TEXT`, `--task-file FILE` | Task for `run`, inline or from a UTF-8 file of at most 16 KiB. Choose one. |
| `--workspace-from UUID`, `--checkpoint-digest SHA256`, `--revision COMMIT` | Start `run` from a retained session's exact recorded commit. All three are required together. |
| `--detach` | Return after `start` or `resume` without attaching the terminal. |
| `--workspace PATH` | Stopped ordinary Git directory that seeds `start`. |
| `--include-ignored PATH` | Ignored file `start` carries on every visit. Repeat it for each file. |
| `--to NAME` | Configured placement for `move`: `local.name` or a name under `remote`. `sproozi move --help` describes it as ``placement `NAME`: the value of local.name (for example local) or a name under "remote"``. Any other name fails before a request; see [move destinations](#move-destinations). |
| `--local` | Make `init` provision a local installation instead of only creating a directory. |
| `--root PATH` | Local installation root; defaults to the recorded root or `~/sproozi`. |
| `--model NAME` | Optional provider model for the harness `init --local` adds. Without it the harness uses its own default model. |
| `--instructions-file FILE` | Approved developer instructions for a new installation, at most 16 KiB. |
| `--image REFERENCE` | Native image as `REPOSITORY@sha256:DIGEST` or a local `sha256:` content ID. |
| `--build DIRECTORY` | Build the native image with Docker from a Sproozi checkout or release source bundle. |
| `--provider-url URL` | Provider base URL for the harness being added; defaults to its public provider. |
| `--provider-ca-file FILE` | PEM CA bundle for a private provider endpoint. Setup copies it into `credentials/` as an owner-only file; the source may be readable by anyone but not writable by every account. Passing it again replaces the stored copy. |
| `--credential-file FILE` | Owner-only file holding the provider key. |
| `--credential-stdin` | Read the provider key from standard input. |
| `--skip-credential-check` | Do not ask the provider to validate the key. |
| `--port PORT` | First gateway port to try; defaults to 18443. |
| `--no-service` | Write the configuration without starting the service. |
| `--json` | Print a machine-readable result. `start`, `resume`, `stop` and `sessions` also print JSON whenever standard output is not a terminal. |
| `--timeout DURATION` | Overall limit for `init --local`; defaults to 30 minutes. |
| `--leave-running` | Stop the service while active sessions keep running within their deadlines. |
| `--check-provider` | Make `doctor` validate each stored key by listing the provider's models. |


## Select the owner

[Why the commands use different flags](../guides/session-mobility.md#why-the-commands-use-different-flags)
explains `--executor`, `--from` and `--to`.

`run`, `serve` and every session command accept `--config FILE`. The session
commands also accept a home `--socket PATH` override; `run` and `serve` don't. Configuration defaults to the platform user configuration directory at
`sproozi/config.json` when that file exists, and otherwise to the local
installation's `config.json` (the recorded root or `~/sproozi`). A local
installation also wins over a platform configuration that names no remote
executor and is not ready for `serve`, such as the starter `sproozi init`
writes. A socket-only local invocation skips that default file;
for example, `sessions --socket PATH` queries only home. Supply
`--config FILE --socket PATH` to combine configured remote sources with a socket
override. An explicit config is always read.

| Command | Owner selection |
| --- | --- |
| `run` | Requires `--executor NAME` naming an executor under `remote` or `workSources` in the configuration. `docker` and `home` are rejected, for example `run does not accept --executor docker: docker is home's executor name, and run submits only to …`: to work at home, use `start`. |
| `sessions` | Without `--executor`, queries home and configured remote catalogues. `--executor docker`, home's executor name, selects home; another configured name selects that remote. |
| `status`, `recover`, `cancel` | `--executor NAME` addresses that remote executor. `--executor docker` or an omitted executor addresses home. |
| `start`, `attach`, `move`, `resume`, `stop` | Require the home owner. Use `--executor docker` or omit it; a remote executor is rejected. `move --to NAME` selects a placement controlled by home. |
| `pull` | Transfers into home from `--from NAME`, a remote executor. A remote `--executor NAME` can name the same source; conflicting names are rejected. |

An explicit remote selection never falls back home. An executor that isn't
configured fails before a request. Three names refer to your machine: `docker`
is home's executor name, `home` is the home daemon, and `local.name` is home's
local placement for `move --to`. Only `docker` addresses home as an executor.
`--executor home` fails with `executor "home" names the home daemon, not an
executor; use --executor docker (home's executor name) or omit --executor`,
and `--executor local` with `executor "local" is local.name, home's local
placement for "move --to", not an executor; …`. A configuration entry named
`docker` or `home` fails with `"docker" is reserved: docker is home's executor
name, home names the home daemon, and local.name names home's local placement
for "move --to"`. Unsupported flags and
operation/owner combinations fail before a request. Commands requiring a session
accept either the first positional UUID or `--id UUID`, never both. A second
positional UUID fails with, for example, `status: unexpected argument "UUID";
usage: sproozi status SESSION_UUID`. UUIDs must
use canonical lowercase spelling. `sessions` takes no session UUID. `start` can
create one when omitted; `run` returns the remote executor's stable session ID.

Use `--project ID` to filter discovery to an approved Project, including with
`--executor docker`. Catalogue rows retain the configured Project association and
repository provenance. Project filtering does not grant membership or execution
authority. [Projects and workspace revisions](projects-and-revisions.md) describes
`run --project ID`, `--workspace-from SESSION_UUID`, and independent home approval.

Use explicit selectors when switching between owners:

```sh
sproozi sessions
sproozi status SESSION_UUID --executor engineering
sproozi recover SESSION_UUID --executor engineering
sproozi status SESSION_UUID --executor docker
sproozi resume SESSION_UUID --executor docker
sproozi stop SESSION_UUID --executor docker --operation ORIGINAL_STOP_UUID
```

When you retry a command that changes state, reuse its operation UUID and
arguments. `pull` derives its operation from the session ID and source unless
`--operation` is given, so repeating the same `pull` command retries it. A
timeout means the outcome is unknown: check `status` on the same owner before
deciding what to do. `recover` finishes the original operation and doesn't
accept a new operation UUID. `stop` waits for supported foreground work and saves a checkpoint;
`cancel` revokes before forced stop. An old operation remains bound to its original
execution and cannot affect a later resume.

### Recovery messages

When an earlier operation or the session's state blocks a command, home
refuses it and names the command that unblocks it. Nothing was recorded, so
the reply has no retry hint. `ID` is the session UUID and `OP` an operation
UUID.

| Reply | Cause | Run |
| --- | --- | --- |
| `session: previous KIND requires recovery; run "sproozi recover ID" to finish it, or "sproozi cancel ID" to revoke and force-stop its current execution`, where `KIND` is `start`, `resume`, `move` or `stop` | A `move` or `stop` while that earlier operation on this execution is unfinished. | `sproozi recover ID` to finish it, or `sproozi cancel ID` to give it up and force-stop the session's current execution. |
| `session: cancellation unfinished; run "sproozi recover ID" to finish the cancellation` | A `move` or `stop` while a cancellation hasn't yet proved the agent stopped and its usage finalized. | `sproozi recover ID`. |
| `session ID is cancelled; start a new session` | A `move` or `stop` of a session whose cancellation finished. Nothing can continue it. | `sproozi start` for new work. |
| `home: previous execution must stop and commit before resume; run "sproozi stop ID" first` | `resume` while the current execution is still running. | `sproozi stop ID`, then `resume` again. |
| `home: start intent conflicts with retained launch; ID was started with operation OP: to continue it, repeat the original start command with --operation OP, or run "sproozi cancel ID" to revoke and force-stop its current execution` | A `start --id ID` with a different operation or different `--include-ignored` flags from the original start. | The original `start` command with `--operation OP`, or `sproozi cancel ID`. |
| `home: start intent conflicts with retained launch; ID was not created by start, see "sproozi status ID"` | A `start --id ID` for a session home didn't create with `start`, such as one it pulled. | `sproozi status ID`, then the command its phase calls for. |
| `home: session ID has no running native process to attach to; run "sproozi status ID" to see its phase: "sproozi recover ID" finishes an unfinished start or move, and "sproozi resume ID" continues a stopped session` | `attach` when no harness process is running. | `sproozi status ID`, then `recover` or `resume` as it says. |

Any other `attach` failure gives its reason followed by `; run "sproozi status
ID" to see the session's phase`.

When a Docker command that home runs fails, the error includes Docker's
standard error, cut to its first 4096 bytes and then marked
` [output truncated]`.

### Move destinations

`move --to` takes a placement, not an executor, so the two names that address
home as an executor or daemon are rejected with the name to use instead:

- `move --to docker: "docker" is home's executor name; use --to local (local.name) to move to this machine`
- `move --to home: "home" names the home daemon; use --to local (local.name) to move to this machine`

Any other name that is neither `local.name` nor a key of `remote` fails with
the configured choices and the file to add it to, for example `move --to "X"
is not a configured placement; use one of: local (local.name), r (remote.r),
or add it under "remote" in FILE`. A `workSources` entry is not a placement.
The messages use your `local.name`, shown here as `local`.
When the command runs without a configuration, for example with only
`--socket`, the home daemon checks the name instead and replies `home: move
destination "X" is not a configured placement; use one of: local, r`. None of
these rejections ends with the retry hint, because nothing was recorded:
correct the name and run `move` again; there is no operation to reuse.

## Read status and discovery

`start`, `resume`, `stop` and `sessions` print a short summary when standard
output is a terminal: for `start`, `resume` and `stop` one line with the session
UUID, harness, phase, placement and deadline, then the next command to run;
for `sessions` a table with the columns `ID`, `HARNESS`, `PHASE`, `PLACEMENT`
and `DEADLINE`, plus `EXECUTOR` when rows come from more than one catalogue.
When standard output is not a terminal, or `--json` is given, they print the
JSON described below, one document per command. `status` and the other
session commands always print JSON.

`attach` needs an interactive terminal on standard input; without one it fails
with `attach needs an interactive terminal, and standard input is not one`
before contacting home. `start` and `resume` without `--detach` still start the
session when standard input is not a terminal, and say on standard error that
they did not attach.

| Public field | Meaning |
| --- | --- |
| `id` | Durable session UUID and operand for subsequent session commands. |
| `executionId` | Current or last retained bounded execution UUID. Initially it can equal `id`; a later resume gets another execution. |
| `allocationId` | Current or last retained visit, distinct from the execution. |
| `incarnation` | Backend `executionId` and immutable `resourceUid` for the exact physical writer. The nested backend ID is distinct from the top-level bounded execution ID. These opaque fields do not establish stopped state by themselves. |
| `threadId` | Harness conversation ID for the selected harness. It is not a session command operand or gateway credential. |
| `harness`, `phase`, `deadline` | Selected native client, observed lifecycle and current execution's absolute deadline. |
| `ownership` | Which owner holds the session, judged from the row's catalogue; see [ownership values](#ownership-values). |
| `executor` | In catalogue rows, the owner catalogue queried: `docker` for the home daemon, or a remote executor name. |
| `placement` | Current or last retained execution destination. It can differ from the queried owner. A row for a local visit shows the `local.name` value, such as `local`, next to `"executor": "docker"`; a remote visit shows its `remote` name. |

### Ownership values

`ownership` says who owns the session, while `executor` says which catalogue
the row came from. The two can differ: after a pull, the session appears in
both catalogues.

| `executor` | `ownership` | Meaning |
| --- | --- | --- |
| `docker` | `home` | The home daemon owns the session: you started it locally, or a pull from a remote executor completed. Commands go to the home daemon. |
| `docker` | `source` | A pull is under way and the remote executor hasn't yet acknowledged the handover. Run the same `pull` command again to finish it. |
| a remote executor, such as `engineering` | `executor` | That remote executor owns the session. Address it with `--executor engineering`, or pull it. |
| a remote executor, such as `engineering` | `home` | A mirror row: the remote executor handed the session to a home daemon and keeps only a read-only record, in phase `home-owned`. Use the `docker` row for that session instead. |

The value `executor` means "owned by the remote executor this row came from";
the JSON value keeps that name so scripts that read it keep working.

Home status also includes the nested `session` execution journal for detailed
inspection, and `initialStage`, which repeats `phase` for older clients. That
nested journal keeps its original execution-bound identifiers and receipts.
Remote work status can include bounded untrusted final text and checkpoint
facts. Discovery omits full journals and final answers.

With JSON output, `sessions` writes one JSON document containing `sessions`, `executors` and
`truncated`. It queries at most 32 catalogues concurrently, with a five-second
request timeout per source, and returns at most 512 rows sorted by executor and
session ID. Check each executor's availability independently:

```json
{
  "sessions": [],
  "executors": [
    {"executor": "docker", "availability": "unavailable", "error": "home_not_running"},
    {"executor": "engineering", "availability": "available"}
  ],
  "truncated": false
}
```

Here engineering returned an empty catalogue, and home could not be queried
because no home daemon is running. An unavailable executor's `error` is
`home_not_running` when nothing listens on home's socket, otherwise one of the
work outcome codes below, such as `unavailable` or `denied`.
An unavailable source doesn't mean its catalogue is empty or that nothing is
running there. Rows from sources that answered are still returned, and the
command exits successfully; when home is not running it also says so on
standard error. When no source answers, `sessions` still writes the JSON, then
exits non-zero; standard error says the results show that no executor
answered and gives each source's problem, for example that home is not
running. When some sources answer, standard error names each remote executor
that did not and its cause. When home is running but can't answer, it says
`Home is unavailable, so its sessions are not listed: CAUSE`. A `directory`
whose `control.sock` path is longer than the platform allows gives the cause
`home: control socket path "..." is N bytes; this platform allows at most M;
set a shorter "directory" in config.json`, and every command addressed to home
fails with the same message. A `--socket PATH` that is too long fails before
any request with `control socket path "..." is N bytes; this platform allows
at most M; use a shorter --socket path`. Inspect the JSON as well as the exit status.
For a local installation created by `init --local`, `sessions`, `start`,
`attach`, `move`, `resume` and `stop` first start an absent home daemon, as
`sproozi service start` does, and say so on standard error.
A command addressed to home while it isn't running fails with
`docker (home): no home daemon is listening on the private control socket`,
followed by `: PATH does not exist` when the socket file is missing, then
`; start home with "sproozi service start"` for a local installation's
configuration (with `--root PATH` when it is not the recorded root), or
`; start home with "sproozi serve", see ...` otherwise. When the command named a
configuration other than the default with `--config`, the hint echoes it, as
in `start home with "sproozi serve --config '/Users/me/home config.json'"`. The
path is quoted only when it holds characters the shell would treat specially.
A session UUID that home doesn't
hold fails with `docker (home): no session UUID; run "sproozi sessions" to list
sessions`. `resume` continues only sessions imported with `pull`; for a session
created with `start`, it fails with `home: session UUID was started at home
with "sproozi start"; resume continues only sessions imported with "sproozi
pull"`.
`truncated: true` means a source or row limit omitted results. A remote
`home-owned` mirror and its local
import remain separate rows labelled by catalogue; matching session IDs do not
merge their provenance.

## Handle work outcomes

The remote work HTTP interface returns one of the fixed JSON error codes below.
MCP tool failures carry the same code with fixed guidance, and so does the
`problem` field of a remote work status. The CLI shows the recognized code and
HTTP status. It never shows upstream error bodies or private transport details.

If status says `unavailable`, Sproozi can't yet confirm that the agent stopped
or how much it used. Run `recover` before starting new work.

| Code | HTTP status | Next step |
| --- | --- | --- |
| `invalid_request` | 400 | Check canonical session/operation UUIDs and supported task options. |
| `denied` | 403 | Check credentials and approved repository, executor and capability scope. |
| `conflict` | 409 | Inspect retained state and retry the original operation/intent. |
| `expired` | 410 | Inspect retained status before requesting fresh work. |
| `unsupported` | 422 | Check harness support and retained native checkpoint compatibility. |
| `unavailable` | 503 | Restore owner/backend connectivity or storage, inspect status, then retry the same operation. |

Neither `unavailable` nor `unsupported` lets you start the work elsewhere,
refunds usage or extends the deadline. Sproozi never treats an unknown filesystem
or transport error as a rejection or as proof that nothing is running.
