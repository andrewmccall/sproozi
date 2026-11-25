# 0077: Runtime rules gate the building of a Pod

Status: accepted
Date: 2026-11-25

Partly supersedes [ADR 0027](0027-configurable-concurrent-admission.md): its
rule that "a later edit that breaks the runtime of an active run fails that
run". The rest of ADR 0027 stands, including that a policy tightening fails
active runs.

## Problem and constraints

ADR 0027 checks an `AgentRuntime` (`SandboxClient.ValidateRuntime`: the
template rules, the trust bundle and, for a managed run, the managed rules) in
`resolveAdmission`, which every reconcile of a `Queued`, `Admitted` or
`Running` run goes through. It chose this so that a runtime that cannot launch
fails a run before it takes a place under a cap, and reused the per-run
re-evaluation that already applies a policy change. A consequence it recorded
as intended was that an edit breaking the runtime of an active run fails that
run, as a policy tightening does.

That consequence has a cost the record did not weigh. One bad edit to a
runtime fails every live run and session on it at once: the controller marks
each `Failed` with `RuntimeInvalid` and deletes its Pod, and each managed
session loses the work since its last checkpoint. On the remote executor the
same check made `Activate` deny such a session and cut an attached terminal
within about a second. Yet the edit itself changes nothing in a running
sandbox: a Pod's containers, volumes and security settings are immutable once
created, and the trust bundle is an immutable ConfigMap the Pod already
mounted. The runtime rules protect what a new Pod is built from, and an
existing Pod was built from a runtime that passed them.

The controller builds a run's Pod once. A run has a Pod once
`status.identity.sandboxName` is recorded; it never builds a second, and a Pod
that disappears ends the run with `SandboxGone`.

## Decision

Runtime rules gate the building of a Pod. They are checked when a run is
admitted, so a run on a broken runtime never takes a place under a cap, and
again when its Pod is about to be built. A run or session that already has
its Pod keeps running when its `AgentRuntime` is edited into an invalid state,
and keeps running when the edit is reverted; anything that needs a new Pod on
the broken runtime fails with `RuntimeInvalid`.

- **Controller.** `resolveAdmission` calls `ValidateRuntime` only for a
  `Queued` run. `ensureSandbox`, which runs only while no sandbox is recorded,
  calls it again, then creates the agent contract and builds the Pod. Any
  error wrapping `ErrInvalidRuntime` from these steps (from `ValidateRuntime`,
  the contract's harness rule, `BuildManagedPodSpec`'s managed rules or the
  gateway address) fails the run with `RuntimeInvalid`, so an edit that lands
  between the check and the build still ends the run rather than retrying.
  The agent contract is no longer re-ensured once the Pod exists: it is
  immutable, built from the runtime, and the Pod already mounts it.
- **All of `ValidateRuntime`.** The ordinary template rules, the trust-bundle
  check and the managed rules (ADR 0027's 2026-11-25 correction) all follow
  this rule. The trust bundle is no different: the running Pod already holds
  the bundle it was built with, and a changed or missing ConfigMap matters only
  to the next Pod.
- **A completed session stop** builds no Pod, so it is not checked; it ends
  `Succeeded` with `SessionStopped` on any runtime.
- **Remote executor.** `Prepare` of a launch with no registration yet refuses
  a broken runtime with the start refusal of ADR 0067, after every check that
  answers `denied`. A repeated `Prepare` of a registered launch, `Activate`
  and `Attach` do not apply the runtime rules; they still check the template,
  its instructions, the policy, the allowances and the deadline cap.

## Alternatives

- **Keep failing active runs (ADR 0027).** It stops work on a runtime the
  administrator now considers wrong at once, with no further action. Rejected:
  a mistaken edit costs every live session its unsaved work, and the edit does
  not make any running Pod less safe.
- **Fail them, but drive a graceful stop with a checkpoint first.** It would
  keep each session's work while still ending it. The owner considered this
  and did not choose it: it ends sessions nobody asked to end, adds a
  controller-driven stop path beside the owner's and the executor's, and still
  turns a reversible edit into an outage.
- **Refuse the bad edit at apply with an admission webhook.** Decided against
  separately (ADR 0027 records why: serving certificates, webhook
  infrastructure, and no view of later trust-bundle changes). The owner chose
  instead a status condition on the `AgentRuntime` that reports whether it can
  launch; that is planned, not built.
- **Keep the every-reconcile check, skipped when a sandbox is recorded.** It
  would give the same outcomes with a second conditional path through
  `resolveAdmission`. Rejected for checking where the Pod is built, which is
  the one place the rule protects and also catches an edit that lands after
  admission.

## Tradeoffs and consequences

- A running session can outlive the validity of its runtime. Its Pod was built
  from a runtime that passed every rule, but the runtime it names may no longer
  pass them.
- An administrator who needs sessions off a runtime must stop them. Ask each
  session's owner to `stop` it, which keeps a checkpoint, or set
  `spec.cancel: true` on the AgentRun, which ends it without one. Deleting the
  `AgentRuntime` fails every active run on it with `RuntimeNotFound`.
- Until the status condition exists, a runtime that is invalid is discovered
  only by the next run admitted or started on it, which fails with
  `RuntimeInvalid`.
- Other per-reconcile checks still read the runtime: the policy evaluation
  (for example a resource limit raised above the policy's bound fails an
  active run with `ResourceLimitExceeded`) and the agent-contract input check
  (`InvalidRunInput`). They are policy and input checks, not runtime rules, and
  apply to a stopped session too.
- If the controller loses the status write that records a just-created Pod and
  the runtime is edited before the next reconcile, that reconcile checks the
  runtime again and fails the run although its Pod exists.

## Revisit when

- The `AgentRuntime` status condition exists: an administrator might then want
  an opt-in to stop sessions on a runtime that reports it cannot launch.
- The controller gains a path that rebuilds a Pod for an active run, which
  would then have to pass the runtime rules.
- A runtime rule protects something a running Pod reads after it starts, so
  that an edit changes a running sandbox.

## Links

- [AgentRuntime reference](../reference/agentruntime.md#runtime-class)
- [AgentRun reasons](../reference/agentrun.md)
- [Remote executor placement outcomes](../reference/remote-executors.md#placement-outcomes)
- [ADR 0027](0027-configurable-concurrent-admission.md),
  [ADR 0067](0067-a-start-ends-when-its-run-has-ended.md)
- Implementation: `internal/controller/reconciler.go` (`resolveAdmission`,
  `failInvalidRuntime`), `internal/controller/execution.go` (`ensureSandbox`),
  `internal/runtime/kubernetes/placement/placement.go` (`prepareProfile`,
  `liveProfileInput`)
