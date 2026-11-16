# Native launch and control

Controlled Docker and Kubernetes sessions share the `harness.NativeLaunch`
contract and control-reply checks. Each placement supplies fresh gateway access
and approved client connections. Checkpoints cannot supply provider credentials,
launch settings or upstream addresses.

## Launch settings

Task input, harness conversation ID, deadline, model options and trusted
instructions have the same meaning in both placements. A launch may have no
deadline: its JSON omits `deadline`, and the supervisor and checkpoint helper
take `--deadline none`, so the harness runs until stopped. A set deadline is
reached at its instant on the wall clock, the same rule in every component
(`internal/deadline`). Native transcript
observation has a 64 MiB limit, separate from the total checkpoint size limit.
Docker obtains gateway access through home. Kubernetes reads projected access
from fixed runtime-owned paths.

Each backend remains responsible for provisioning its resources, confirming
that writers have stopped and finalizing usage accounting.

## Control replies

The supervisor accepts `configure`, `activate`, `status`, `gate`, `quiesce` and
`attach`. It reports the states `unconfigured`, `prepared`, `active`,
`stopping` and `stopped`. These names are defined once in
`internal/harness/native_control.go`; placement journal phases are separate.

Sproozi checks the operation, state and expected harness conversation ID together.
The first launch can report an empty ID until the client creates its conversation.
Once recorded, that ID cannot change. Resume must use the checkpoint's exact ID.

Both placements run the same graceful handoff. They gate new input, then record
the genuine harness conversation ID (`threadId`) if the launch had none, then wait for the completed
turn. The ID is recorded beside the launch; the launch itself never changes. Gateway access stays open until that point. Revoking access, confirming
that the writer stopped and finalizing usage remain backend steps.

`quiesce` waits at most two minutes for the completed turn; past that it
replies `stopping` with `waiting`. Once the turn completed it asks the harness
to quit and waits at most 30 seconds, or until a set deadline if sooner, for
it to exit. A Codex still running then, or whose full terminal input took no
quit, is killed and waited for at most 3 more seconds, within the deadline.
When the kill ends it and the same completed turn is observed again,
`quiesce` replies `stopped` with the conversation and turn and an `exit`
whose reason is `forced-stop`, writes no exit report and exits 0, as after
any graceful stop
([ADR 0035](../adr/0035-bounded-graceful-stops-and-settled-input.md)). Claude
Code and Copilot are always stopped by a kill; the same waits apply to its
confirmation. A harness that exited by itself instead keeps its own exit code
and reason. If the turn observed after the kill differs, or the kill is not
confirmed, `quiesce` replies with an error and the supervisor reports the
cause `failed`; if the deadline passes first, the cause `deadline`.

A kill can cut short a row the harness was appending to its conversation
file. The supervisor's observation refuses that file, so a Codex stop whose
observation after the kill met it fails with the cause `failed` and the error
`the forced stop left a native journal row unfinished; repeat the stop to
complete it with the completed turns kept`. Capture, which runs only after the
placement has confirmed the writer stopped, keeps the complete rows and leaves
the unfinished final one out, then validates them as usual. Repeating the stop
therefore finishes and keeps the visit's completed turns
([ADR 0061](../adr/0061-a-stop-survives-an-interrupted-write.md)).

A first visit with no task that the supervisor reports `turnless` records no
ID: `quiesce` stops the harness and replies `stopped` with `turnless` and no
`threadId`. Both placements then revoke access, confirm the writer stopped and
finalize usage as for any stop, but capture no checkpoint; the session keeps
the workspace the visit started from
([ADR 0032](../adr/0032-stopping-a-visit-with-no-native-conversation.md)). Only
that reply may end a stop without an ID.

Malformed, oversized or incompatible replies, including trailing data, leave
native state unconfirmed. A reply that says the client is quiet does not prove
container termination, finalized accounting or a valid checkpoint. Those checks
remain separate.

## Exit reports

When the harness ends without a graceful `quiesce`, the supervisor writes one
line to its standard output before it exits:

```text
sproozi-native-exit {"code":2,"cause":"exited","thread":"<id>","output":"<last lines>"}
```

| Field | Meaning |
| --- | --- |
| `code` | The harness's own exit code; `-1` when a signal ended it. |
| `cause` | Why the harness ended, decided once by the supervisor (below). Required. |
| `thread` | The conversation of a completed turn, when the supervisor observed one. Codex only; the placement still validates it. |
| `output` | At most the last 20 terminal lines as plain text, at most 2,048 bytes. |

| `cause` | Decided when | Session after the stop |
| --- | --- | --- |
| `exited` | The harness exited by itself, including after input was gated for a stop that had not yet asked it to quit. | `failed` for a nonzero code, otherwise `retained`. |
| `deadline` | The supervisor stopped the harness at the execution deadline. | `retained`, with `"deadline": true`. |
| `terminated` | The supervisor received `SIGTERM` or `SIGINT`, as when its container is stopped. | `retained`; also `"deadline": true` when the host stopped it at the deadline. |
| `failed` | A graceful stop failed and the supervisor stopped the harness itself. | `retained`. |

A graceful stop writes no report. A stop that writes one (`deadline`,
`terminated` or `failed`) leaves the supervisor's own exit code nonzero even
when the harness exited 0. Both placements read the last
report from the agent's last 50 output lines and ignore a report without a
known `cause`, as from a supervisor built before the field existed; they then
use the container's exit code, and the host's clock decides `deadline`. The
Docker placement reads the agent container's output. The Kubernetes placement
reads the native container's output through `pods/log` (at most 64 KiB) when
it stops a visit whose native container already ended. It reports the
container as ended through `Exited` while the visit is still active, so the
remote executor's exit sweep, or home's exit watcher through the placement
protocol's `exited` action, can stop it
([remote executors](remote-executors.md#noticing-an-exit)). A first visit of a
harness whose state proves completion (Codex, Copilot) binds the thread a
report names; the supervisor reports one only for Codex today. A Claude Code
visit binds no thread from a report. The report is a workload diagnostic,
never a writer fence or accounting fact.

A stop whose quiesce reply was lost, of a harness whose state proves
completion, is a graceful stop on both placements: the agent's container is
seen to end within about ten seconds, or a later stop finds it ended with
the supervisor's exit code 0 and no exit report in its output. No exit
is recorded, and the stop captures as any graceful stop does. A Claude Code
stop whose reply was lost is an exit
([ADR 0062](../adr/0062-one-exit-rule-and-exits-over-the-wire.md)).

### What a stopped visit keeps

Once the writer is fenced and usage sealed, both placements decide what the
stop keeps by one rule
([ADR 0062](../adr/0062-one-exit-rule-and-exits-over-the-wire.md)). "Started
from" is the checkpoint a resumed visit started from, or, for a first visit,
no checkpoint: the session keeps the workspace the visit started from.

| How the visit ended | Kept | If the stopped state is refused | If the capture fails otherwise |
| --- | --- | --- | --- |
| Graceful stop, with a conversation | A new checkpoint | The stop fails | The stop fails |
| Turnless first visit | What it started from | Nothing is captured | Nothing is captured |
| Harness exited, its state proves completion (Codex, Copilot) and it has a conversation | A new checkpoint | What it started from; the stop finishes | The stop fails |
| Harness exited, and its state does not prove completion (Claude Code) | What it started from, even when the visit bound a conversation before it exited | Nothing is captured | Nothing is captured |
| Harness exited before any conversation | What it started from | Nothing is captured | Nothing is captured |

The stopped state is refused when the checkpoint helper's `capture-stopped`
exits 65, or when what it returned does not validate in the placement's own
checks. The writer is fenced, so capturing again would be refused the same
way. The helper's exit codes
([checkpoint helper](../../cmd/checkpoint/README.md#capture-results)):

| Code | Meaning | Docker | Kubernetes |
| --- | --- | --- | --- |
| 65 | The stopped state was refused: no completed turn, as when the agent left its turn unfinished; another conversation; a conversation file the agent deleted, made unreadable or replaced, or a native directory that fails the safety checks; a failed continuity check; a state over the checkpoint bound. | Refused | Refused |
| 74 | The capture could not be carried out, such as a file it could not read. | The stop fails | The stop fails |
| 1 | Any failure of a helper image older than these codes. Docker counts it only when the failure carries the helper's line `Checkpoint operation failed:`; the docker CLI's own code 1 fails the stop. | Refused, as before them | The stop fails, as before them |

Everything else fails the stop too, and repeating it captures again: the
stop's bound or an abort cutting the capture off, any other exit code or a
killed helper, an exec or Docker failure, a failed write. A stop that kept
what the visit started from still reports the visit's `exit`, and the
placement logs `Discarded the captured state of stopped visit ALLOCATION of
execution EXECUTION and kept the checkpoint it started from: ...` (or `the
workspace`) to the service's or the executor's log. A stop that fails on
every repeat, as a Kubernetes stop of a refused state with an older helper
image does, ends with `cancel`, or with a newer image.

## Terminal attachment

The terminal sends raw bytes in bounded messages. Each input acknowledgement
reports how many bytes were written. The client retries only the unwritten part.
Disconnecting ends attachment; it does not stop the execution.

Output has a replay cursor and reports gaps. Reconnecting does not automatically
replay unacknowledged input. This private control API does not expose an ACP or
AHP endpoint. See [ADR 0014](../adr/0014-review-boundaries-and-native-contracts.md)
for the protocol alternatives and conditions for revisiting this choice.
Configured tool integrations use [MCP](configured-mcp.md).
