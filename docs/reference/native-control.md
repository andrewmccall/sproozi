# Native launch and control

Controlled Docker and Kubernetes sessions share the `harness.NativeLaunch`
contract and control-reply checks. Each placement supplies fresh gateway access
and approved client connections. Checkpoints cannot supply provider credentials,
launch settings or upstream addresses.

## Launch settings

Task input, harness conversation ID, deadline, model options and trusted
instructions have the same meaning in both placements. Native transcript
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
| `deadline` | The supervisor or its terminal stopped the harness at the execution deadline. | `retained`, with `"deadline": true`. |
| `terminated` | The supervisor received `SIGTERM` or `SIGINT`, as when its container is stopped. | `retained`; also `"deadline": true` when the host stopped it at the deadline. |
| `failed` | A graceful stop failed and the supervisor stopped the harness itself. | `retained`. |

A graceful stop writes no report. A forced stop leaves the supervisor's own exit
code nonzero even when the harness exited 0. The Docker placement reads the
last report from the container's last 50 output lines and ignores a report
without a known `cause`, as from a supervisor built before the field existed;
it then uses the container's exit code. The report is a workload diagnostic,
never a writer fence or accounting fact.

## Terminal attachment

The terminal sends raw bytes in bounded messages. Each input acknowledgement
reports how many bytes were written. The client retries only the unwritten part.
Disconnecting ends attachment; it does not stop the execution.

Output has a replay cursor and reports gaps. Reconnecting does not automatically
replay unacknowledged input. This private control API does not expose an ACP or
AHP endpoint. See [ADR 0014](../adr/0014-review-boundaries-and-native-contracts.md)
for the protocol alternatives and conditions for revisiting this choice.
Configured tool integrations use [MCP](configured-mcp.md).
