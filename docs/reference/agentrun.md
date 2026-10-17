# AgentRun

`AgentRun` requests one task through a trusted `AgentTemplate`. It supplies task
text and requested capabilities. The template selects the runtime and policy.

## What it records

- the referenced template
- untrusted task text
- immutable event context
- the explicitly requested capability subset
- cancellation intent
- observed lifecycle and disposable identity

## Key fields

```yaml
spec:
  templateRef:
    name: sre-remediation
  task: Investigate the CrashLoopBackOff incident.
  eventContext:
    namespace: sproozi-demo
    alertname: CrashLoopBackOff
  capabilities:
    - kubernetes.read
    - github.pull_request
    - model.inference
  cancel: false
```

The same immutable capability list can request configured MCP servers, for
example `capabilities: [model.inference, mcp.docs]`. The selected policy must allow
`mcp.docs` and define its permitted tools; the gateway must register `docs`.
The run cannot choose a provider URL, credential file or tool-policy override.
See [configured MCP capabilities](configured-mcp.md).

## Lifecycle

`AgentRun.status.phase` moves through:

```text
Queued -> Admitted -> Running -> Succeeded | Failed | TimedOut | Cancelled
```

While a run is `Queued`, `Admitted` or `Running`, each reconcile re-checks it
against its live template, policy, runtime and session binding. A missing
reference, a policy denial or an `AgentRuntime` that cannot launch a sandbox
(`RuntimeInvalid`) moves it to `Failed`. The `Ready` condition holds the reason
and message. A queued run that fails this check never takes the single active
slot.

Success uses the process exit of the container named `agent`, after the Pod
becomes terminal. Review a proposed PR separately. See
[agent contract and completion](agent-contract.md).

## Immutability

After creation, these fields are immutable:

- `spec.templateRef`
- `spec.task`
- `spec.eventContext`
- `spec.capabilities`

The only mutable intent in `spec` is `cancel: true`.

## Status highlights

- `status.identity.serviceAccountName`
- `status.identity.sandboxName`
- `status.startedAt`
- `status.completedAt`
- `status.summary`

These fields record created resources and the run outcome. They contain no
credentials or raw prompt and output text.

A worker can explicitly publish a final answer tied to its Run UID. The
controller saves it as `status.result`, with `{text, truncated}`, before
deleting the Pod. This field can contain sensitive, untrusted output. It is
separate from the audit-safe summary and conditions and expires with the Run. A
missing result means unavailable; an available result can have empty text. See
[result publication](agent-contract.md#retained-final-answers).
