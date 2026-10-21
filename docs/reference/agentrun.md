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
and message. A queued run that fails this check is never admitted. Until its
own reconcile fails it, it can briefly hold a place in the admission queue and
delay later runs under the cluster cap. A run whose template or policy is
missing is skipped at once. Deleting an AgentPolicy fails every non-terminal run
whose template currently references it, running ones included, with
`PolicyNotFound`, and stops their sandboxes. If an active run's template is
re-pointed to another policy, the run fails with `PolicyChanged`.

### Admission and concurrency caps

By default the controller admits every queued run that passes these checks, so
any number of runs can be `Admitted` or `Running` at once. Two optional caps
limit that:

- the controller flag `--max-active-runs`, which caps active runs across the
  cluster;
- the policy field
  [`maxActiveRuns`](agentpolicy.md#concurrent-runs), which caps active runs
  admitted under that policy.

Queued runs are considered in creation order, oldest first, with the namespace
and then the name breaking ties. A run whose policy is full is skipped, so it
doesn't hold back later runs under other policies. A run held by a cap stays
`Queued` with reason `WaitingForConcurrencyCap`; it is never denied. When a run
finishes or is deleted, or a policy changes, the controller re-checks every
queued run. See
[Limit concurrent runs](../guides/install-from-checkout.md#limit-concurrent-runs).

Editing an admitted run's AgentPolicy doesn't fail or restart the run. Any `spec` edit
other than `maxActiveRuns` closes the run's open gateway connections within the
[revocation bound](security-hardening.md#protect-transport-and-identities), but
the run carries on: the AgentRun stays `Running` with `Ready` `True`, and its new connections
get the edited policy. The only trace is the gateway log line `Revoked
inspected connection`, with `reason="gateway: execution grant revoked: gateway:
the AgentRun's policy, template, capabilities or allocation changed"`. Read it
with `kubectl logs -n sproozi-system deployment/sproozi-gateway`.

Success uses the process exit of the container named `agent`, after the Pod
becomes terminal. Review a proposed PR separately. See
[agent contract and completion](agent-contract.md).

## Ready condition reasons

Read the reason and message with
`kubectl get agentrun NAME -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'`.
`Ready` is `True` in `Admitted`, `Running` and `Succeeded`, and `False`
otherwise, with one exception: a `Running` run whose sandbox Pod a ResourceQuota
rejected has `Ready` `False` with reason `QuotaExceeded` until the Pod is
created.

| Reason | Phase | Meaning | What to do |
| --- | --- | --- | --- |
| `Initialized` | `Queued` | Created and waiting for its first admission check. | Nothing. |
| `WaitingForConcurrencyCap` | `Queued` | A concurrency cap is full. The message names the policy cap, the cluster cap or both, for example `AgentRun is held by a concurrency cap: policy cap 1 (AgentPolicy sre) reached`, and ends `; waiting for earlier queued runs` when earlier queued runs are ahead. | Nothing; it is admitted when capacity frees. To admit it sooner, cancel other runs or raise the cap. |
| `Admitted` | `Admitted` | Admitted under the AgentPolicy recorded in `status.policyName`. | Nothing. |
| `Running` | `Running` | Run identity exists; the sandbox Pod is starting or running. | Nothing. |
| `QuotaExceeded` | `Running` | A namespace ResourceQuota rejected the sandbox Pod. The run stays active and counts against the caps. The controller retries with a backoff from 5 seconds up to one minute until the Pod is created or the run passes its execution deadline. | Free quota in `sproozi-agents`, raise the quota, or cancel the run. The wait counts toward the run's execution duration. |
| `ExecutionSucceeded` | `Succeeded` | The `agent` container exited with code 0. | Review any proposed PR. |
| `TemplateNotFound`, `PolicyNotFound`, `RuntimeNotFound` | `Failed` | A referenced `AgentTemplate`, `AgentPolicy` or `AgentRuntime` is missing from the namespace. Deleting an AgentPolicy fails every non-terminal run whose template currently references it, running ones included, with `PolicyNotFound`, and stops their sandboxes. | Create the named object, or fix the reference, then create a new run. |
| `PolicyChanged` | `Failed` | The run's template was re-pointed to another policy after admission. The message is `AgentTemplate T now references AgentPolicy B, but this run was admitted under AgentPolicy A; submit a new run`. The gateway denies the run's new connections at once and closes its open ones within the [revocation bound](security-hardening.md#protect-transport-and-identities), under 16 seconds. A run admitted before `status.policyName` existed has no recorded policy, so it isn't failed: its new connections are checked against the policy the template now references. | Submit a new run, which is admitted under the policy the template now references. |
| `RuntimeInvalid` | `Failed` | The `AgentRuntime` cannot launch a sandbox; the message names the problem. | Fix the runtime, then create a new run. |
| `CapabilityNotAllowed`, `EgressProfileNotAllowed`, `ResourceLimitExceeded`, `PackageScopeInvalid`, `MCPScopeInvalid` | `Failed` | The policy denied the run, template or runtime; the message names the capability, profile or limit. | Request fewer capabilities, or have an administrator change the policy or runtime. |
| `SessionBindingRejected` | `Failed` | The run does not match the visit the remote executor registered for it, or that registration no longer exists. | Start the work again from the session command that created it. |
| `SessionDeadlineExceeded` | `TimedOut` | The run's execution passed its deadline. A move doesn't extend it. | Resume the session, which starts a new execution with a new deadline. |
| `MaxExecutionDurationExceeded` | `TimedOut` | The run passed its execution deadline. | Raise the policy `maxExecutionDuration`, or narrow the task. |
| `SandboxGone` | `Failed` | The sandbox Pod disappeared before the `agent` container exited, for example after eviction or deletion. | Check node and Pod events, then retry. |
| `SandboxExitNonZero` | `Failed` | The `agent` container exited with a non-zero code. | Read `status.result` and any collected sandbox logs. |
| `SandboxExitUnavailable` | `Failed` | The controller could not read the `agent` container exit code. | Check the Pod events, then retry. |
| `CancelRequested` | `Cancelled` | `spec.cancel` was set to `true`. | Nothing. |

## Immutability

After creation, these fields are immutable:

- `spec.templateRef`
- `spec.task`
- `spec.eventContext`
- `spec.capabilities`

The only mutable intent in `spec` is `cancel: true`.

## Status highlights

- `status.policyName`: the AgentPolicy the run was admitted under, recorded in
  the same status update that admits it. Active runs count against this
  policy's `maxActiveRuns`, even if their template later references another
  policy.
- `status.identity.serviceAccountName`
- `status.identity.sandboxName`
- `status.startedAt`
- `status.completedAt`

These fields record created resources and the run outcome. They contain no
credentials or raw prompt and output text.

The agent can explicitly publish a final answer tied to its Run UID. The
controller saves it as `status.result`, with `{text, truncated}`, before
deleting the Pod. This field can contain sensitive, untrusted output. It is
separate from the conditions and expires with the Run. A
missing result means unavailable; an available result can have empty text. See
[result publication](agent-contract.md#retained-final-answers).
