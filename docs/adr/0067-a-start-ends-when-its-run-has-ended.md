# 0067: A start ends when what it would start from has ended

Status: accepted
Date: 2026-11-17

## Problem and constraints

The Kubernetes placement's `Prepare` creates the visit's
[session registration](../../GLOSSARY.md#session-registration) and its
AgentRun, then waits for the controller to create the sandbox Pod. The
controller fails a run before any Pod exists, on every reconcile, for a
missing template, policy or runtime, a changed admitted policy, an invalid
runtime or run input, any policy denial, or a rejected or cancelled session
binding. Once released, a terminal run loses its sandbox name, and its
retention TTL deletes it.

`Prepare`'s wait swallowed every one of these. While the registration had no
Pod it ignored "exact admitted run unavailable" and every NotFound, never read
the run's phase, and ended only on the execution deadline or the caller's
context. Before [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md) the
deadline always ended it. Since, a task may have no deadline, and the remote
executor's driver calls the coordinator with no timeout. So:

- a remote task with no deadline whose AgentRun failed before its Pod stayed
  `preparing` for ever, with no problem shown; only `cancel` ended it;
- the same held when the run failed, was deleted, or had its registration
  cancelled by another operation while `Prepare` already waited;
- with a deadline, the same task waited until the deadline, showed "Work owner
  unavailable; … recover this session", and the next recover ended it "the
  deadline passed before it started": the right end, the wrong reason;
- an interactive session showed the unavailable problem after 2 minutes, and
  every recover waited 2 more.

Two related gaps made recovery worse. `Inspect` of a bound start whose run had
no sandbox, queued or ended, failed with "exact admitted run unavailable", so a
recover never called `Prepare` again. And `AbortPreparation` refused a
registration that another operation had cancelled before any Pod was bound,
as "not a preparation".

Retrying cannot help: the run name is fixed per launch (`runName`), so
`ensureRun` returns the same ended run. Cancelling worked, because
`AbortPreparation` fences a failed run.

Constraints:

- [ADR 0018](0018-coordinator-owns-every-visit.md) and
  [ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md): never
  start a second writer, never refund unsealed usage, never resurrect
  cancelled work, and change a phase only through a declared transition.
- [ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md): no state waits on a
  deadline to end, and journals keep no new fields.
- A run that is only waiting, queued under a concurrency cap or admitted while a
  ResourceQuota refuses its Pod, can still succeed and must keep waiting.
- The work catalogue and the execution journal are read with unknown fields
  refused; an older binary must still read what this one writes.

## Decision

**A placement that can never prepare a launch says so, and the coordinator
ends that start as it ends one its deadline overtook, whatever the deadline.**

### The placement refuses

`session.StartRefused` is a placement's refusal of a launch it can never
prepare, with a reason of at most 512 printable bytes. The Kubernetes
placement returns it from `Prepare`'s wait for the Pod, on its next read, when
the bound run:

- is terminal: "AgentRun NAME failed (timed out, was cancelled, succeeded)
  before its sandbox started: REASON: MESSAGE", from its `Ready` condition;
- has `spec.cancel` set, was deleted, or was replaced by a run with another
  UID;
- has a registration that left `creating` and `bound`, or was deleted.

It keeps waiting for a run that is queued, admitted, or running with a sandbox
named but no Pod yet; a read that fails is not evidence either way. Only the
start's wait checks this; a stop's wait is unchanged. `Inspect` of a bound
start whose run has no sandbox now reports `preparing`, so a recover calls
`Prepare` and reaches the refusal. `AbortPreparation` fences a registration
another operation cancelled with no Pod ever bound: it was never prepared or
active, so it never had authority.

### The coordinator ends the start

When `Prepare` returns `StartRefused`, the coordinator takes ADR 0060's
`abandoned` event, after the same proof that nothing runs at the destination:
`AbortPreparation` where the placement fences preparations, otherwise
`Inspect` and an abort of any incarnation reported. The execution is `idle`
with its last checkpoint. The error is `StartRefused` marked ended, carrying
the reason; it and `ErrDeadlinePassed` both match `ErrStartEnded`. If the
proof fails, nothing is recorded and the error is `ErrEndUnproven` wrapping
the refusal.

Any other `Prepare` failure leaves the start `preparing` for recover, as
before.

The journal keeps no reason. A replay of the ended operation reports
`Coordinator.StartEnded`: `ErrDeadlinePassed` once the deadline has passed,
otherwise `StartRefused` with no reason.

### The remote executor's driver

The driver records the problem `it can never start: REASON` when `Recover`
reports a refusal, deadline or none. The decision table gains a situation bit,
`refused`, read from that problem while the execution is starting or idle, and
an action, `end-refused`:

- a task whose start was refused is cancelled through `end()` with operation
  `start-refused`, keeping that problem, as `end-expired` and `end-revoked`
  end one. The refusal wins over a deadline that has also passed.
- an interactive session stays `retained` with the problem, and `resume
  --remote` continues it, as for a start its deadline ended.

While the end is unproven, the problem adds "the executor has not yet shown
that nothing runs there, so cancel this session" for a task, which the driver
cancels after its backoff, and "recover or cancel this session" for an
interactive session, which waits for its owner.

### Over the placement protocol

A refused `prepare` answers `refused` with HTTP 409 and a `reason` field,
which only that code carries. The home's client reads it as `StartRefused`,
so a session home coordinates on a cluster ends the same way. No existing code
fitted:

- `conflict` tells the caller to check status and retry the original
  operation, and it already covers a contradicted retained record
  ([ADR 0066](0066-a-contradicted-retained-record-refuses-work-and-never-blocks-an-end.md)),
  a deleted registration and a replaced run, which the operator must deal
  with. A home cannot tell an ended run from those by the code, and ending
  all of them as refused starts would change what those documented cases
  say.
- `expired` names the deadline, the misleading reason this record removes.
- `denied` and `unsupported` describe the caller or the executor's
  capabilities.

A client older than `refused` reads it as an unknown code: `unavailable`, with
the message that the executor is probably newer. The start stays `preparing`
as before this decision, until the home is updated.

### Home

Home retires the execution after any ended start, not only one the deadline
ended, so the session is `retained` with its work kept. The reply says
`session ID can never start at its destination: REASON, so it is stopped with
its work kept; continue it on this machine with "sproozi resume ID"`. While
unproven, it names the refusal, then what is unproven, the repeat and `cancel`.

### Bounding a cancellation's fence

The same review found that `Abort`'s wait for a deleted Pod's agent container
to be seen to stop used its caller's context, which the driver leaves
unbounded. On a lost node the writer-fence finalizer keeps the Pod and its
container status never changes, so the wait never ended. It now ends after 90
seconds, over the default 30-second termination grace and under the home's
2-minute abort wait. The fence is then reported unknown: the coordinator
records `unknown-stop`, nothing is sealed, and a recover waits again.

## Alternatives

- **Keep waiting and only show the reason.** The session would say why it
  waits, but a run that ended cannot start, so the state would still never end
  without a deadline. Rejected: it keeps the defect and adds a status channel.
- **Retry with a new run name.** A fresh AgentRun could succeed once an
  administrator fixes the cause. But the registration, run and launch are bound
  one to one, and a second run for the same launch would need a new
  allocation, which only the coordinator creates; the same cause would also
  fail every retry. Rejected: `resume` already starts a new execution, and is
  what the interactive session's problem names.
- **Fail the session instead of cancelling or abandoning it.** A new terminal
  phase would read clearly, but the journal has no such phase and adding one
  breaks older binaries. An interactive session also has committed work that
  `resume` can continue. Rejected: a task is cancelled, which is its terminal
  state with nothing kept, and an interactive session is retained.
- **Store the reason in the journal.** A replay could repeat it. Rejected
  while the journal has no migrations: an older binary would refuse the
  journal. The driver keeps the reason in its catalogue's existing `problem`.
- **Map the refusal to `conflict` on the wire.** See above.

## Tradeoffs and consequences

- A start whose run was cancelled by a concurrent `cancel` is refused too, and
  ends the same way unless the cancellation intent is already recorded; the
  driver's table carries out a held cancellation first.
- The driver reads `refused` from the problem text it wrote. If the executor
  stops between the coordinator's commit and saving that problem, the task is
  idle with an ended start and no refusal problem, which the table reads as
  a start the deadline ended: it is cancelled as "the deadline passed before
  it started", deadline or none. It ends; only the reason is wrong.
- A replay of an ended start past its deadline reports the deadline, whatever
  ended it.
- A task whose deadline has passed before the driver steps it is ended as
  expired without asking the placement, as before.
- Each refusal check costs two more API reads per 100-millisecond poll of the
  wait.
- `driver-outcomes.txt` gained the `preparing-start-refused` and
  `idle-start-refused` states; no earlier outcome changed.

## Revisit conditions

- Journal migrations become available: the ended operation could keep its
  reason, and replays could repeat it.
- A placement learns to recreate a run for a launch, so an ended run is no
  longer final.
- Another placement gains a wait that can outlive its cause, such as Docker
  pulling an image that can never be pulled; it should return
  `StartRefused` too.
- Something needs to show why a start is still waiting (a queued or
  quota-blocked run): that needs a side channel from the placement while
  `Prepare` runs, which this record does not add.

## Links

- Implementation: `internal/runtime/kubernetes/placement/lifecycle.go`
  (`waitPod`, `startRefused`, `fenceCancelled`),
  `internal/runtime/kubernetes/placement/abort_preparation.go`
  (`preparationRegistration`), `internal/session/types.go`
  (`StartRefused`, `ErrStartEnded`), `internal/session/coordinator.go`
  (`drive`, `abandon`, `StartEnded`), `internal/work/decision.go`,
  `internal/work/driver.go`, `internal/session/profile/protocol.go`,
  `internal/session/profile/server.go`, `internal/session/home/socket.go`
- Tests: `internal/runtime/kubernetes/placement/run_ended_test.go`,
  `internal/runtime/kubernetes/placement/abort_bound_test.go`,
  `internal/session/refused_test.go`, `internal/work/refused_test.go`,
  `internal/work/decision_test.go`, `internal/session/profile/refused_test.go`,
  `internal/session/home/refused_test.go`
- Reference: [session commands](../reference/session-commands.md),
  [remote executors](../reference/remote-executors.md#placement-outcomes)
- Related: [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md),
  [ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
  [ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md),
  [ADR 0066](0066-a-contradicted-retained-record-refuses-work-and-never-blocks-an-end.md)
