# Session commands and identities

The public `sproozi` executable addresses durable sessions. A session can retain
its native conversation across several independently authorized executions. Each
execution has its own deadline and ceiling, and each placement visit has a fresh
allocation and exact backend incarnation. A catalogue entry or native thread UUID
is an observation, not permission to activate a writer.

Use [retained remote work](../guides/portable-workflows.md) for remote task setup,
pull and local continuation, or [native session mobility](../guides/session-mobility.md)
for interactive moves within one bounded execution. The
[home configuration reference](../../cmd/sproozi/README.md) describes private files,
images, gateway credentials and remote profiles.

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

Keep the operation UUID and exact intent when retrying a mutation. A timeout is
an uncertain result; inspect the selected owner's retained status before choosing
a next step. `recover` continues retained intent and does not accept a replacement
operation UUID. `stop` waits for supported foreground work and seals a checkpoint;
`cancel` revokes before forced stop. An old operation remains bound to its original
execution and cannot affect a later resume.

## Read status and discovery

| Public field | Meaning |
| --- | --- |
| `id` | Durable session UUID and operand for subsequent session commands. |
| `executionId` | Current or last retained bounded execution UUID. Initially it can equal `id`; a fresh continuation gets another execution. |
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

Here engineering has an empty successful catalogue; home could not be queried.
An unavailable source does not establish an empty catalogue or an absent writer.
Successful rows survive another source's failure. Discovery can exit successfully
with unavailable sources, so inspect the JSON as well as the command exit status.
`truncated: true` means the
source or row bound omitted results. A remote `home-owned` mirror and its local
import remain separate rows labelled by catalogue; matching session IDs do not
merge their provenance.

## Handle work outcomes

The remote work HTTP interface returns a fixed JSON error code, and MCP tool
failures include that same code with fixed guidance. The owner client preserves
recognized code/status pairs and does not expose upstream error bodies or private
transport diagnostics. A retained asynchronous work status `problem` uses the
same safe code and recovery guidance. An unavailable problem keeps writer-stop
and usage unconfirmed; inspect or recover it before any fresh activation.

| Code | HTTP status | Next step |
| --- | --- | --- |
| `invalid_request` | 400 | Check canonical session/operation UUIDs and supported task options. |
| `denied` | 403 | Check credentials and approved repository, executor and capability scope. |
| `conflict` | 409 | Inspect retained state and retry the original operation/intent. |
| `expired` | 410 | Inspect retained status before requesting fresh work. |
| `unsupported` | 422 | Check harness support and retained native checkpoint compatibility. |
| `unavailable` | 503 | Restore owner/backend connectivity or storage, inspect status, then retry the same operation. |

Missing writer-stop or accounting evidence remains unavailable. An incompatible
checkpoint is unsupported. Neither outcome permits a fresh destination writer,
refund or deadline extension. Unknown filesystem/transport failures are not
interpreted as semantic rejection or known absence.

## Migrate output consumers

The unreleased public home `productId` field has been removed. Read `id` for the
stable session operand. Remote work `executionId` now names the bounded execution;
its former allocation meaning moved to `allocationId`. Read `incarnation` for
exact resource identity. Internal retained launch journals keep their historical
field meanings and should not be edited or migrated by clients.

Consumers of `sessions` must decode the single labelled catalogue document rather
than concatenate independent owner arrays. Always inspect `executors` and
`truncated` before treating discovery as complete.

Update the CLI, home daemon and remote owners together when adopting the corrected
output contract. Socket-only local commands can still run without a global
configuration file.
