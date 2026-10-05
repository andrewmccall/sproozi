# AgentRun

**Audience:** operators, contributors

`AgentRun` is one bounded request to execute a trusted `AgentTemplate`.

## What it owns

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

## Lifecycle

`AgentRun.status.phase` moves through:

```text
Queued -> Admitted -> Running -> Succeeded | Failed | TimedOut | Cancelled
```

Success uses the process exit of the container named `agent`, after the Pod
becomes terminal. A proposed PR is separate evidence. See
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

Those fields record what Sproozi created and how the run ended without exposing credentials or raw prompt/output content.
