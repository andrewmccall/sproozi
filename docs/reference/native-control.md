# Native launch and control boundary

Both controlled Docker and Kubernetes placements use the same trusted
`harness.NativeLaunch` contract, common environment/client-binding helpers and
strict native reply validators. The supervisor executes a fixed supported stock
CLI. Approved capability names determine broker client bindings; they do not
carry provider credentials or checkpoint-supplied upstream addresses.

Task intent, selected native thread, deadline, administrator model/options and
instructions have the same meaning in each placement. Native rollout observation
has a 64 MiB limit, separate from the aggregate checkpoint artifact limit.
Docker acquires fresh workload access through its home authority; Kubernetes
reads projected access at fixed runtime-owned paths. Their provisioning and
physical writer-stop evidence remain different backend responsibilities.

The fixed control replies bind operation, state and expected native thread.
First launch explicitly permits an empty thread only until the genuine native
UUID is observed and bound. Resuming a selected checkpoint requires that exact
thread. A malformed, oversized, trailing or incompatible reply is unavailable
native evidence. Acknowledged quiescence does not replace physical container
termination, sealed accounting or the selected checkpoint verification.

Terminal attachment carries bounded raw bytes and actual partial-write
acknowledgements. The client retries only the unwritten suffix, and disconnect
ends attachment rather than the execution. Output has a bounded replay cursor
and an explicit gap indication. No unacknowledged input is automatically replayed
across reconnects. These are private Sproozi native controls, not an ACP or AHP
endpoint.

ACP provides structured conversation methods, while AHP provides host-facing
terminal state and interaction. Neither directly replaces the current native
control and exact byte acknowledgement contract. A future AHP host can retain
the stock PTY with negotiated byte extensions; a future ACP backend changes the
interaction and must establish native checkpoint identity. See
[ADR 0013](../adr/0013-review-boundaries-and-native-contracts.md) for the compared
alternatives and concrete revisit criteria. Existing configured integrations
continue to use [MCP](configured-mcp.md).
