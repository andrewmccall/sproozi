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

The same immutable ConfigMap carries the generated kubeconfig and the selected
CLI's native MCP configuration. `clientConfig.harness` supports `codex`,
`claude-code` and `opencode`; omitted selection leaves preparation to the
administrator. Native files select only requested `mcp.<server>` connections at
the gateway, without provider URLs or credentials. Claude Code and OpenCode also
receive `instructions.txt` and `request.json`, retaining trusted instructions
and untrusted request data separately. Retries require an exact match of all
contract files. Native Kubernetes MCP remains an explicit trusted opt-in.

These files supply client configuration; the requested grant and live policy
remain the authorization boundary. See [harness configuration](harnesses.md),
[configured MCP capabilities](configured-mcp.md#deliver-ordinary-codex-configuration)
and [ADR 0002](../adr/0002-native-cli-harnesses-and-model-protocols.md).

## Completion

Launch a bounded CLI invocation in the completion container. Codex and Claude
Code use `exec` so their process status becomes the container status. OpenCode's
trusted launch adapter supervises the stock CLI and checks its native exported
session before producing a container status, because OpenCode 1.15.1 can exit
zero after an API error. The adapter does not interpret task quality or replace
the agent loop.

Sproozi maps zero to `Succeeded` and nonzero or unavailable status to `Failed`.
Explicit cancellation and deadline expiry produce `Cancelled` and `TimedOut`,
followed by Pod deletion. A PR, summary or model answer never establishes the
controller lifecycle outcome. See [harness completion](harnesses.md).
