# Session commands and identities

The public executable is `sproozi`. Build it from the repository root:

```sh
go build -o bin/sproozi ./cmd/sproozi
```

These commands require administrator configuration, approved images and prepared
execution environments. Building the CLI does not install a cluster or start an
agent.

A [session](../../GLOSSARY.md#session) tracks the native conversation across executions. Each
execution needs separate approval and has its own deadline and usage allowance.
Each [visit](../../GLOSSARY.md#visit) to a placement gets a new allocation
and backend resource identity. Knowing a session or conversation ID doesn't give
you permission to run it.

Use [remote work and local resume](../guides/portable-workflows.md) for remote task setup,
pull and local resume, or [native session mobility](../guides/session-mobility.md)
for interactive moves within one bounded execution. The
[home configuration reference](../../cmd/sproozi/README.md) describes private files,
images, gateway credentials and profile services.

## Remote work and local resume

| Command | Purpose |
| --- | --- |
| `run` | Submit a Codex task to a configured remote executor. |
| `sessions` | List accessible sessions, optionally filtered by executor or Project. |
| `status` | Inspect one session by its stable session UUID. |
| `pull` | Claim a stopped remote session and import its conversation and Git workspace. This does not start an agent. |
| `resume` | Start a new local execution of retained work with fresh permissions. It attaches the terminal unless `--detach` is selected. |
| `stop` | Stop local work and retain its supported state for later resume. |
| `cancel` | Cancel an execution, including unfinished work that cannot stop gracefully. |
| `recover` | Continue a journaled operation after an uncertain response or interruption. |

The harness also has a native conversation UUID. It is separate from Sproozi's
session and execution IDs. See [core concepts](../explanations/core-concepts.md)
and [Projects and workspace revisions](projects-and-revisions.md) for the basic
model and exact-commit launches.

## Interactive sessions and movement

`serve --config FILE` starts the home daemon. `start` seeds a managed Docker
workspace from a stopped Git repository, `attach` connects its native terminal,
and `move` transfers the supported state to a configured destination. These
client commands accept the configuration and socket selectors described below.

Detaching the terminal does not stop the daemon or agent. Running a container on
your laptop still requires the laptop and Docker engine to remain available.
Follow [session mobility](../guides/session-mobility.md) for the complete setup,
commands, supported clients and movement limits.

## Select the owner

Every client command accepts `--config FILE` and a home `--socket PATH` override.
Configuration defaults to the platform user configuration directory at
`sproozi/config.json`. A socket-only local invocation skips that default file;
for example, `sessions --socket PATH` queries only home. Supply
`--config FILE --socket PATH` to combine configured remote sources with a socket
override. An explicit config is always read. `serve` requires `--config FILE`.

| Command | Owner selection |
| --- | --- |
| `run` | Requires an explicitly configured remote `--executor NAME`. |
| `sessions` | Without `--executor`, queries home and configured remote catalogues. `--executor docker` selects home; another configured name selects that remote. |
| `status`, `recover`, `cancel` | `--executor NAME` addresses that remote owner. `--executor docker` or an omitted executor addresses home. |
| `start`, `attach`, `move`, `resume`, `stop` | Require the home owner. Use `--executor docker` or omit it; a remote executor is rejected. `move --to NAME` selects a placement controlled by home. |
| `pull` | Transfers into home from `--from NAME`. A remote `--executor NAME` can name the same source; conflicting names are rejected. |

An explicit remote selection never falls back home. Unsupported flags and
operation/owner combinations fail before a request. Commands requiring a session
accept either the first positional UUID or `--id UUID`, never both. UUIDs must
use canonical lowercase spelling. `sessions` takes no session UUID. `start` can
create one when omitted; `run` returns the remote owner's stable session ID.

Use `--project ID` to filter discovery to an approved Project, including with
`--executor docker`. Catalogue rows retain the configured Project association and
repository provenance. Project filtering does not grant membership or execution
authority. [Projects and workspace revisions](projects-and-revisions.md) describes
`run --project ID`, `--workspace-from SESSION_UUID`, and independent home approval.

Use explicit selectors when switching between owners:

```sh
sproozi sessions --config /private/sproozi/config.json
sproozi status SESSION_UUID --executor engineering --config /private/sproozi/config.json
sproozi recover SESSION_UUID --executor engineering --config /private/sproozi/config.json
sproozi status SESSION_UUID --executor docker --config /private/sproozi/config.json
sproozi resume SESSION_UUID --executor docker --config /private/sproozi/config.json
sproozi stop SESSION_UUID --executor docker --config /private/sproozi/config.json \
  --operation ORIGINAL_STOP_UUID
```

When you retry a command that changes state, reuse its operation UUID and
arguments. A timeout means the outcome is unknown: check `status` on the same
owner before deciding what to do. `recover` finishes the original operation and
doesn't accept a new operation UUID. `stop` waits for supported foreground work and saves a checkpoint;
`cancel` revokes before forced stop. An old operation remains bound to its original
execution and cannot affect a later resume.

## Read status and discovery

| Public field | Meaning |
| --- | --- |
| `id` | Durable session UUID and operand for subsequent session commands. |
| `executionId` | Current or last retained bounded execution UUID. Initially it can equal `id`; a later resume gets another execution. |
| `allocationId` | Current or last retained visit allocation, distinct from the execution envelope. |
| `incarnation` | Backend `executionId` and immutable `resourceUid` for the exact physical writer. The nested backend ID is distinct from the top-level bounded execution ID. These opaque fields do not establish stopped state by themselves. |
| `threadId` | Retained native conversation UUID for the selected harness. It is not a session command operand or gateway credential. |
| `harness`, `phase`, `deadline` | Selected native client, observed lifecycle and current execution's absolute deadline. |
| `ownership` | `home` for claimed/local ownership, `source` while an imported claim is unacknowledged, or `executor` for remote ownership in discovery. |
| `executor` | In catalogue rows, the owner catalogue queried. |
| `placement` | Current or last retained execution destination. It can differ from the queried owner. |

Home status also retains `initialStage` and the existing nested `session`
execution journal for detailed inspection. That nested journal keeps its original
execution-bound identifiers and receipts. Remote work status can include bounded
untrusted final text and checkpoint facts. Discovery omits full journals and final
answers.

`sessions` writes one JSON document containing `sessions`, `executors` and
`truncated`. It queries at most 32 catalogues concurrently, with a five-second
request timeout per source, and returns at most 512 rows sorted by executor and
session ID. Check each executor's availability independently:

```json
{
  "sessions": [],
  "executors": [
    {"executor": "docker", "availability": "unavailable", "error": "unavailable"},
    {"executor": "engineering", "availability": "available"}
  ],
  "truncated": false
}
```

Here engineering returned an empty catalogue, and home could not be queried.
An unavailable source doesn't mean its catalogue is empty or that nothing is
running there. Rows from sources that answered are still returned. Discovery can exit successfully
with unavailable sources, so inspect the JSON as well as the command exit status.
`truncated: true` means a source or row limit omitted results. A remote
`home-owned` mirror and its local
import remain separate rows labelled by catalogue; matching session IDs do not
merge their provenance.

## Handle work outcomes

The remote work HTTP interface returns one of the fixed JSON error codes below.
MCP tool failures carry the same code with fixed guidance, and so does the
`problem` field of a retained work status. The CLI shows the recognized code and
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
