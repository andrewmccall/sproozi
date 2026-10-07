# Agent contract and completion

The controller mounts a versioned JSON input document at
`/etc/sproozi/contract/input.json` in selected workload containers. Its version
is `sproozi.agentcontract/v1alpha1`. The controller creates the immutable contract
ConfigMap for the run; the runtime command chooses how the agent reads it.

| Field | Source and meaning |
| --- | --- |
| `run` | Run namespace, name and immutable UID |
| `trusted.instructions` | Administrator-owned `AgentTemplate.spec.instructions` |
| `untrusted.task` | Caller-provided `AgentRun.spec.task` |
| `untrusted.eventContext` | Caller or webhook evidence, never operating instructions |
| `capabilities` | The run's requested capability list |
| `workspace` | Fixed `/workspace` path |
| `gateway` | Shared endpoint and projected identity token path |

```json
{"gateway":{"endpoint":"https://sproozi-gateway.sproozi-system.svc:8443","tokenPath":"/var/run/sproozi/tokens/gateway/token"}}
```

All capabilities share this identity. There are no per-service tokens or gateway
aliases. The endpoint comes from `AgentRuntime.spec.gatewayEndpoint`. Task and
event content are not injected into the environment.

## Trusted client configuration

The same immutable ConfigMap also carries the generated kubeconfig. When
`AgentRuntime.spec.clientConfig.harness` is `codex`, the controller adds
`codex-mcp.toml` and mounts it at `/etc/sproozi/contract/codex-mcp.toml`. This public
fragment selects only the run's requested `mcp.<server>` connections at the
runtime's gateway endpoint. It contains no provider addresses or credentials.
The trusted runtime launch copies it into disposable Codex configuration before
starting the agent. Native Kubernetes MCP launch settings remain a separate
trusted opt-in for the existing `kubernetes.read` capability.

These files supply client configuration; the immutable requested grant and live
policy remain the authorization boundary. See
[configured MCP capabilities](configured-mcp.md#deliver-ordinary-codex-configuration)
and [ADR 0001](../adr/0001-mcp-capabilities-through-shared-gateway.md).

## Completion

Run the CLI in one-shot mode and use `exec` so its exit code becomes the container
exit code. Sproozi maps zero to `Succeeded` and non-zero or unavailable exit status
to `Failed`. Explicit cancellation and deadline expiry produce `Cancelled` and
`TimedOut`, followed by Pod deletion. A PR, summary, log line or model response
never establishes the lifecycle outcome.

The current Pod adapter waits for a terminal Pod and reads the container named
`agent`. Use one workload container with that name. Long-running ordinary
sidecars prevent completion; arbitrary container names accepted by validation
are not supported by completion. See [AgentRuntime](agentruntime.md).
