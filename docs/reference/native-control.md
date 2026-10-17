# Native launch and control

Controlled Docker and Kubernetes sessions share the `harness.NativeLaunch`
contract and control-reply checks. Each placement supplies fresh gateway access
and approved client connections. Checkpoints cannot supply provider credentials,
launch settings or upstream addresses.

## Launch settings

Task input, native conversation ID, deadline, model options and trusted
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

Sproozi checks the operation, state and expected native conversation ID together.
The first launch can report an empty ID until the client creates its conversation.
Once recorded, that ID cannot change. Resume must use the checkpoint's exact ID.

Both placements run the same graceful handoff. They gate new input, then record
the genuine conversation ID if the launch had none, then wait for the completed
turn. Gateway access stays open until that point. Revoking access, confirming
that the writer stopped and finalizing usage remain backend steps.

Malformed, oversized or incompatible replies, including trailing data, leave
native state unconfirmed. A reply that says the client is quiet does not prove
container termination, finalized accounting or a valid checkpoint. Those checks
remain separate.

## Terminal attachment

The terminal sends raw bytes in bounded messages. Each input acknowledgement
reports how many bytes were written. The client retries only the unwritten part.
Disconnecting ends attachment; it does not stop the execution.

Output has a replay cursor and reports gaps. Reconnecting does not automatically
replay unacknowledged input. This private control API does not expose an ACP or
AHP endpoint. See [ADR 0014](../adr/0014-review-boundaries-and-native-contracts.md)
for the protocol alternatives and conditions for revisiting this choice.
Configured tool integrations use [MCP](configured-mcp.md).
