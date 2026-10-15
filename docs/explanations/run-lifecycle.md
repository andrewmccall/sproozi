# Run lifecycle

An `AgentRun` records one requested execution and its immutable capabilities,
untrusted task and event context. The template, policy and runtime are
administrator-owned.

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

Admission resolves references, checks requested capabilities and resources, and
provisions a dedicated ServiceAccount and NetworkPolicy. The controller creates
an immutable contract ConfigMap and the runtime-derived Pod. It does not create
per-run target-namespace Roles or RoleBindings. The ServiceAccount has no direct
Kubernetes grant; the shared gateway authorizes Kubernetes reads.

## Completion

The Pod adapter waits for the whole Pod to become terminal, then reads the exit
code of the container named `agent`. Zero means `Succeeded`; non-zero or unavailable
exit means `Failed`. Use one workload container with that name. Long-running
ordinary sidecars prevent completion. A PR or agent summary is separate task
evidence, not the lifecycle success condition.

Explicit cancellation and deadline expiry record `Cancelled` or `TimedOut` and
start Pod cleanup. They do not depend on the agent reporting an outcome.

## Cleanup and retention

Normal terminal handling and explicit deletion remove the recorded Pod before
releasing its contract ConfigMap, NetworkPolicy and ServiceAccount. The Run record
remains until the policy's `retentionTTL` expires unless explicitly deleted.

If provisioning created a Pod before its status write succeeded, release
recovers its deterministic name and verifies the managed-by and immutable Run UID
labels. It also recovers the deterministic identity/network names. An existing
Unknown or empty-phase Pod keeps containment; only a successful absent lookup or
terminal Pod status permits release. Lookup errors and ownership mismatches block
cleanup. Managed writer finalizers still require their independent termination
and accounting evidence. Budget ConfigMaps and unique webhook replay records
have no automatic retention cleanup. See
[security hardening](../reference/security-hardening.md).

## Gateway revocation

One projected gateway-audience token has a two-hour lifetime; policy bounds
execution to one hour. The demo reads the token once. Token expiry is not the
revocation boundary. New requests check active Run identity and policy; existing
sessions close through bounded revalidation. Each authority lookup has a
five-second deadline; the default periodic interval is one second. Transport
closure does not prove dependency or upstream work stopped. See the
[transport limits](../reference/security-hardening.md#protect-transport-and-identities).
The workload has no Sproozi proxy
sidecar or local proxy process.

[Recorded acceptance](../demos/verified-sre-demo.md) covers successful semantic
runs and scoped sandbox cleanup. It does not cover partial-provisioning recovery.
