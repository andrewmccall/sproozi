# Run lifecycle

An `AgentRun` requests one task through an administrator-owned template. It
records the requested capabilities, task text and event context. The controller
checks the request, starts the agent Pod, records its outcome and cleans up.

Read [core concepts](core-concepts.md) for the resource names. This page
describes the ordinary Kubernetes run lifecycle. Managed session stop and move
operations have [additional checks](../guides/session-mobility.md).

```mermaid
sequenceDiagram
    participant Caller as Operator or signed webhook
    participant API as Kubernetes API
    participant Ctrl as Controller
    participant Policy as Template, policy and runtime
    participant Pod as Sandbox Pod
    participant Gateway as Shared gateway
    Caller->>API: Create AgentRun
    API->>Ctrl: Reconcile Queued
    Ctrl->>Policy: Resolve references and evaluate admission
    Ctrl->>API: Mark Admitted and provision identity/network
    Ctrl->>API: Mark Running
    Ctrl->>API: Create immutable input contract
    Ctrl->>Pod: Create disposable workload
    Pod->>Gateway: Request external operation
    Gateway-->>Pod: Allow or deny using live run and policy
    Ctrl->>Pod: Observe terminal process exit
    Ctrl->>API: Record terminal phase
    Ctrl->>Pod: Delete recorded sandbox
    Ctrl->>API: Remove recorded identity, network and contract
```

## Phases and admission

```text
Queued -> Admitted -> Running -> Succeeded | Failed | TimedOut | Cancelled
```

Before admission, the controller resolves the template, policy and runtime
references. It checks the requested capabilities and resource limits.

After admission, the controller creates a dedicated ServiceAccount,
NetworkPolicy and immutable input ConfigMap. It creates the Pod from the runtime
configuration. The workload ServiceAccount has no direct Kubernetes read
permissions. The gateway checks Kubernetes reads; the controller creates no
target-namespace Roles or RoleBindings for the run.

## Completion

The Pod adapter waits for the whole Pod to become terminal, then reads the exit
code of the container named `agent`. Zero means `Succeeded`; non-zero or
unavailable exit means `Failed`. Use one workload container with that name.
Long-running ordinary sidecars prevent completion. Review the task result
separately. A PR or agent summary does not determine run success.

Explicit cancellation and deadline expiry record `Cancelled` or `TimedOut` and
start Pod cleanup. They do not depend on the agent reporting an outcome.

## Cleanup and retention

Normal terminal handling and explicit deletion remove the recorded Pod before
releasing its contract ConfigMap, NetworkPolicy and ServiceAccount. The
`AgentRun` record remains until the policy's `retentionTTL` expires, unless you
delete it first.

If provisioning stopped before status was saved, cleanup looks up the Pod and
checks that it belongs to the run. It keeps identity and network controls until
the Pod is confirmed absent or terminal. Lookup errors, unknown Pod state and
ownership mismatches block cleanup. Managed sessions also need confirmation
that the writer stopped and its usage was finalized.

Budget ConfigMaps and unique webhook replay records have no automatic retention
cleanup. See [security hardening](../reference/security-hardening.md#cleanup-and-state-retention)
for ownership checks and retained state.

## Gateway revocation

The projected gateway token lasts two hours; policy limits execution to one
hour. The demo reads the token once. The gateway does not wait for token expiry
to revoke access. It checks run identity and policy on every new request and
rechecks open connections every second by default. Each policy lookup has a
five-second deadline. Closing a connection doesn't prove that the upstream work
stopped. See the [transport
limits](../reference/security-hardening.md#protect-transport-and-identities).
The workload has no Sproozi proxy sidecar or local proxy process.
