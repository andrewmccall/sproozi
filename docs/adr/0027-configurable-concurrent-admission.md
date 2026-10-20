# Optional caps replace the single active-run slot

Status: accepted
Date: 2026-10-19

## Problem and constraints

The controller admitted one active AgentRun per cluster. `handleQueued` listed
runs in every namespace and kept a run queued while any other run was
Admitted or Running. Nothing records why there is one slot. The rule dates
from the first code commit, and [ADR 0003](0003-persistent-assistants-and-retained-results.md)
treats it as a given. The single slot is a launch blocker: a cluster can do
one job at a time, Hermes cannot fan work out, and an interactive session move
waits behind any unrelated run.

The 2026-10-19 investigation of every place that relies on the single slot
found:

- **Per-run state is already keyed by run UID:**
  - budget accounts;
  - sandbox Pod, ServiceAccount, contract and NetworkPolicy names;
  - gateway identity and the per-account inference queue from ADR 0015;
  - GitHub branch ownership.
- **One isolation gap:** each run's NetworkPolicy selected Pods by run name.
  Runs with the same name in different namespaces would have shared each
  other's egress. It now selects by run UID.
- **Admission:** it needs counting, and selection that a full queue head
  cannot block. Releasing a slot woke only one queued run.
- **Gateway load:** it grows with concurrent runs.
  - TLS inspection costs memory and CPU per connection.
  - Each inspected connection re-reads run, template and policy from the API
    server every second.
  - The ChatGPT authorize path holds one process-wide lock across a Secret
    read for every model request.
  - The connection pool is shared, and the gateway Pod had a 250m CPU and
    256Mi memory limit.
- **Webhook bursts:** event bursts were throttled only by the single slot.

The gateway remains one process ([ADR 0026](0026-single-gateway-refresh-owner-with-recreate.md)),
so every concurrent run's traffic shares its CPU, memory and connection pool.

## Decision

- **Cluster cap.** The controller flag `--max-active-runs` is optional. Unset
  or `0` means no cluster cap, and the default install leaves it unset.
  A positive value caps how many AgentRuns can be Admitted or Running at once
  across the cluster.
- **Policy cap.** `AgentPolicy.spec.maxActiveRuns` is optional and at least 1.
  It caps how many active runs are admitted under that policy. It sits next to
  the policy's other limits on runs: budgets, duration, resource bounds and
  retention. Templates that share a policy share its cap.
- **Counting key.** At admission the controller records the admitting policy's
  name in `status.policyName`. Active runs are counted against that recorded
  policy, not against what their template references now. Templates can be
  re-pointed, and labels are writable by the run's author, so neither would
  hold the count. Active runs admitted before this field existed are resolved
  through their template.
- **Re-pointed templates fail closed (addendum).** If an active run's template
  is re-pointed to another policy, the controller fails the run with reason
  `PolicyChanged` and the message `AgentTemplate T now references AgentPolicy
  B, but this run was admitted under AgentPolicy A; submit a new run`. The
  gateway denies its new connections at once, on both the cached and the live
  path, and closes its open ones within the revocation bound (under 16 seconds
  with a stalled watch). A run admitted before `status.policyName` existed is
  not failed; its new connections get the newly referenced policy. This matches deletion, which fails runs with `PolicyNotFound`, and
  keeps permissions and the cap on the same policy: otherwise the run would
  count against A's cap while the gateway checked it against B's permissions.
  The alternative, keeping the run under A, would need the gateway to check a
  policy its template no longer names.
- **Admission order.** Queued runs are ordered first-in, first-out by creation
  time, with the namespace and then the name as tiebreaks. The controller walks that order and
  simulates admission against the free cluster capacity and each policy's free
  capacity. A run is admitted when the simulation admits it. A run whose
  policy is full is skipped rather than blocking later runs under other
  policies.
- **Waiting.** Being at capacity keeps a run Queued, with Ready False, reason
  `WaitingForConcurrencyCap` and a message naming the full cap, such as
  `AgentRun is held by a concurrency cap: cluster cap 4 reached` or
  `AgentRun is held by a concurrency cap: policy cap 1 (AgentPolicy sre) reached`. Capacity is
  never a policy denial.
- **Wake-ups.** When a run becomes terminal or is deleted, or a policy
  changes, the controller wakes every queued run instead of one successor.
- **Quota rejection.** The controller creates the sandbox Pod after the run
  moves to Running. If a namespace ResourceQuota rejects the Pod, the run stays
  Running, and still counts against the caps, with Ready False, reason
  `QuotaExceeded` and the API server's message, which names the quota. The
  controller recognises the rejection from that error: a Forbidden response
  whose message contains "exceeded quota". It retries with a backoff of 5
  seconds to one minute until the Pod is created, which sets Ready True again,
  or the run passes its execution deadline and ends TimedOut. A ResourceQuota
  on `sproozi-agents` is the documented Kubernetes-level backstop.
- **Reconciles stay serialized** (`MaxConcurrentReconciles: 1`), and the
  active-run List stays uncached. Counting is correct because every earlier
  admission is visible to the next reconcile. A counter object or distributed
  lock would add coordination with no observed need.
- **Gateway sizing.**
  - The gateway connection limits become configuration, with larger defaults:
    the connection limit and the limit on inspected connections per execution.
    An inspected connection is one CONNECT tunnel the gateway terminates, not a
    user's session.
  - The per-execution limit keeps one run from taking the whole pool.
  - The gateway's CPU and memory **limits** are raised and its requests are
    unchanged, so idle clusters reserve no more than before.
  - Defaults are 2048 connections and 16 inspected connections per execution,
    set by `SPROOZI_GATEWAY_MAX_CONNECTIONS` and
    `SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION`; the second keeps its name
    but counts inspected connections. `TestInspectedSessionMemory` measures
    about 150–160 KiB of heap and stack per streaming inspected connection
    across runs. The sizing rule is one connection per MiB of the memory
    limit, with a 256 KiB budget for each connection: the default 2048
    connections take 512 MiB, a quarter of the 2Gi limit, and the rest is
    headroom for model bodies, the gateway's own heap and the garbage
    collector. The gateway sets its Go memory limit to 90% of
    `SPROOZI_GATEWAY_MEMORY_LIMIT`, which the install fills from
    `limits.memory`, unless `GOMEMLIMIT` is set.
  - Each model request in flight reads request and response bodies of up
    to 1 MiB each: up to about 2 MiB of body data per call, plus allocation
    overhead at peak. ([ADR 0028](0028-streamed-model-responses-and-estimated-interrupts.md)
    later replaced the buffered response with one parsed event of up to
    8 MiB.) That memory
    is outside the per-connection budget, so the headroom fits a few hundred
    concurrent model calls rather than one on every connection.
  - **Revocation and freshness.** A new connection (CONNECT) still uses a live
    TokenReview and live reads. The per-request and one-second checks of open
    inspected connections read AgentRun, AgentTemplate, AgentPolicy and
    ServiceAccount from a watch cache. A mismatch with the admitted grant, or a
    cache error, is confirmed with a live read before a connection is closed.
    An edit to `maxActiveRuns` alone is not a mismatch: the cap bounds how many
    runs start, not what an admitted run may do, so open connections continue
    and their audit records keep the policy generation they were admitted
    under. Any other change to the policy `spec` closes them; label and
    annotation edits don't. Each execution with open
    inspected connections also gets a live recheck at least every 10 seconds
    (`LiveRecheckInterval`), so a stalled watch cannot hide a revocation.
    Managed-session registration ConfigMaps and sandbox Pods stay live reads,
    because a session move revokes the source through them.
  - **Revocation bound.** Before, with a lookup deadline of 5 seconds, a
    revocation closed open connections within about 1 second plus 5 seconds.
    With a working watch it takes the smaller of 1 second plus watch delay plus
    5 seconds, and under 16 seconds, because the live recheck below also
    applies to a slow watch. If the watch stalls, it takes under 16 seconds (10 + 1 + 5): up to 10 seconds
    until the execution's live recheck is due, a one-second tick to claim it,
    and the 5-second lookup deadline. Every other connection of the execution
    reads live from that claim until a live read completes. When the API
    server answers promptly, that is about 11 seconds plus read latency.
  - **ChatGPT.** The authorize path takes its lock only to reload or refresh
    the session. It reads the Secret at most every 10 seconds
    (`SessionReloadInterval`), and sooner near token expiry, instead of on every
    model request. An administrator's replacement or removal of the Secret
    therefore reaches every request that authorizes 10 seconds or more after
    it, rather than the next request.

## Alternatives

- **Keep one slot, or a fixed default such as 4.** Predictable and
  conservative, but it makes the product unable to run parallel work, which
  the owner rejected. An optional cap gives administrators the same safety
  when they want it.
- **Cap on AgentTemplate.** A template is a task type, while limits on runs
  already live on the policy and the gateway reads the policy on every
  request. A template cap can be added later if a task type must not overlap
  with itself.
- **Kueue or the Kubernetes Agent Sandbox project.** Kueue can gate Pods by
  queue, but it would control Pods the controller has already committed to,
  and it adds an operator dependency for a count the controller can keep.
  Agent Sandbox (v1.0.7, beta) has no concurrency limits:
  - its warm pool size is the number of idle sandboxes, not a cap;
  - its NetworkPolicy is shared by a template and allows public egress by
    default;
  - a pooled Pod cannot carry the run's ServiceAccount and token, which the
    gateway's identity depends on.
- **Parallel reconciles with a lease or counter object.** It would give more
  admission throughput, but it adds a coordination protocol. One worker that
  reads the live run list is enough for the admission rates seen so far.

## Tradeoffs and consequences

- **No cluster cap.** Without one, concurrency is bounded only by policy caps,
  quotas, node capacity and the gateway.
  - Each concurrent connection the gateway inspects holds TLS buffers and
    certificate state in its memory.
  - Model streams keep connections open for minutes.
  - API server traffic, ChatGPT plan rate limits, and provider spend rate
    all grow with concurrency.
  - Budgets remain per run, so N concurrent runs can spend N budgets at once.
  - The guides tell administrators to set a cap, policy caps or a
    ResourceQuota for shared clusters.
- **Limits, not requests.** Raising the limits means a busy gateway can use
  more of a node, but the scheduler does not reserve it. A gateway that
  exceeds the node's free memory can still be evicted. Administrators who need
  guaranteed capacity raise the requests themselves.
- **Admission cost.** Every queued run lists the cluster's AgentRuns at each
  reconcile and resolves the policy of each earlier queued run. That cost
  grows with the queue length.
- **Cache delay.** The watch cache adds watch delay to how quickly a
  revocation reaches open inspected connections. If the watch stalls, the
  bound becomes under 16 seconds instead of about 6. This partly replaces
  [ADR 0016](0016-dispatcher-capability-enforcement-and-fresh-revalidation.md)'s
  rejection of an informer cache. ADR 0016 rejected it because revocation
  would wait for watch delivery, the cache would hold every watched object,
  and a stale cache could keep a revoked connection open. The 16× larger
  connection default made 0016's uncached reads (about 770 GET requests per
  second at the old 128 connections) grow to thousands. The live confirmation
  and the 10-second live recheck bound the stale-cache risk, and only the four
  authority types are cached, with ServiceAccounts limited to `sproozi-agents`.
  Registrations and Pods, which 0016 also counted, stay live.

## Revisit when

- Admission latency or API load from long queues is measured as a problem.
- The gateway needs more than one replica. That needs a new ChatGPT refresh
  owner (ADR 0026).
- Agent Sandbox ships identity and NetworkPolicy attached at claim time.
- A real task type needs its own cap on overlapping with itself.

## Links

- Implementation:
  - `internal/controller/queue.go` and `reconciler.go`;
  - `api/v1alpha1/agentpolicy_types.go` and `agentrun_types.go`;
  - `cmd/main.go`;
  - `internal/proxytransport/gateway.go`;
  - `config/manager/gateway.yaml`.
- References: [AgentPolicy](../reference/agentpolicy.md),
  [AgentRun](../reference/agentrun.md),
  [Security hardening](../reference/security-hardening.md).
- Implementation, gateway freshness: `cmd/gateway/main.go`
  (`startAuthorityCache`, the gateway connection limit and memory limit
  environment variables),
  `internal/gateway/kubernetes/auth.go` and `internal/modelauth/auth.go`.
- Earlier records: [ADR 0003](0003-persistent-assistants-and-retained-results.md),
  [ADR 0016](0016-dispatcher-capability-enforcement-and-fresh-revalidation.md),
  which this record partly supersedes,
  [ADR 0017](0017-runtime-admission-and-replay-retention.md),
  [ADR 0026](0026-single-gateway-refresh-owner-with-recreate.md).
