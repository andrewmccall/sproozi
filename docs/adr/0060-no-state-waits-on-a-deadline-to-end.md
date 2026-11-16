# 0060: No state waits on a deadline to end

Status: accepted
Date: 2026-11-15

## Problem and constraints

Before [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md), every
execution had a deadline, and a stuck state ended once it passed. ADR 0056 lets
work its owner starts run without one. A review then looked for states whose
only exit was the deadline, and for states that a passed deadline makes
permanent.

It found one in the execution journal. The table in
[ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md) refuses
`prepared` and `activated` after the deadline, because they start work, but
not `move`. So:

- A move that begins after the deadline, or whose source stop finishes after
  it, stops the source writer, commits its checkpoint and records `preparing`.
  Every attempt to continue is then refused with `session: original deadline
  expired`.
- From `preparing` after the deadline, `stop` is refused because the pending
  operation is not a stop. `recover` hits the deadline again, and `resume`
  says to run `recover`.
- `activating` is stuck the same way when the destination was prepared but
  never activated. A late activation that did happen is not stuck: when the
  placement reports the destination active, the coordinator records it.
- That leaves `cancel`. After a cancellation, the home catalogue still names
  the cancelled execution as current, so `resume` answers that the session was
  cancelled, although a checkpoint was committed when the source stopped. The
  only thing left to do with the session is `rm`.

Two ordinary routes reach this: a `resume` interrupted before its activation
and retried after its deadline, and `sproozi move ID --local` of a session on a
cluster after its deadline. The second is the natural command, because nothing
else brings such a session home.

The remote executor had the same defect. Its driver cancels a session whose
execution is still starting when the deadline passes. For an interactive
session that has been resumed, the cancellation discards the checkpoint the
earlier execution committed, and the executor refuses to resume a cancelled
session.

Constraints:

- [ADR 0018](0018-coordinator-owns-every-visit.md) and ADR 0058: never start a
  second writer, never refund usage that is not sealed, and never resurrect
  cancelled work.
- Journals on users' machines are read with unknown fields refused, and there
  is no migration step. An execution already stuck in `preparing` must be
  rescued by the new code as it is stored.
- [ADR 0038](0038-exited-agents-and-one-recovery-path.md): each state names one
  next command.
- [ADR 0034](0034-resume-continues-every-home-session.md): `resume` continues a
  stopped session from its last checkpoint in a new execution with a new
  deadline.

## Decision

**No state depends on a deadline to end, and a passed deadline never leaves an
execution in a state whose only exit destroys the session.** A deadline refuses
the events that start or extend work. Wherever those refusals could leave an
execution, its lifecycle declares another way on. The deadline never refuses
that exit, and it keeps what the session has already committed.

This record applies the principle to the execution journal, the home service
and the remote executor's interactive sessions. Two other changes apply it and
are recorded with dated notes in their own ADRs: cancelling a Kubernetes
preparation that has no Pod, and bounding how long a stop waits for Codex to
quit.

### The `abandoned` event

| Event | From → to | Refused after the deadline |
| --- | --- | --- |
| `abandoned` | `preparing`, `activating` → `idle` | no |

`abandoned` ends a move, or an execution's first visit, whose deadline passed
before its destination became active. The coordinator takes it only when all of
these hold:

- the execution's deadline has passed on the coordinator's clock
  (`internal/deadline`; an execution without a deadline never takes it);
- the pending operation is a move or a first visit, not a stop;
- the record has no cancellation intent;
- `prepared` or `activated` would now be refused. The placement did not report
  the destination active.

Before it records `abandoned`, the coordinator establishes that no writer exists
or can come to exist at the destination:

1. **No launch.** The operation has no launch, so `Prepare` was never called.
   The deadline check now runs before a move's launch is created, so this
   holds for every move that reaches its deadline before preparing.
2. **A placement that fences preparations.** If the operation has no prepared
   incarnation and the placement implements `PreparationAborter`,
   `AbortPreparation` must succeed.
3. **Any other placement.** The coordinator inspects the launch. `absent`
   means nothing was prepared. An incarnation the placement reports, or the
   prepared incarnation of an `activating` operation, is aborted. The abort
   must prove that incarnation stopped and return a sealed receipt. The
   incarnation is then retired as a visit with that receipt, its usage is
   deducted and any checkpoint it captured is kept, as cancellation does.

The operation then ends with its existing `cancelled` stage and no new visit.
The record has no cancellation intent, which separates the two. The execution
is `idle`, keeping its last checkpoint: the one the source's stop committed, or
the one the first visit started from. A replay of that move, start or resume
reports `ErrDeadlinePassed`.

If the coordinator cannot establish this, it records nothing and returns
`ErrEndUnproven`, naming what is missing and the placement's error. The phase
stays `preparing` or `activating`, which is true: the pending destination is
not active, and no other writer runs. `unknown-stop` is not used. The journal
describes it only for a writer being stopped or cancelled, and stating "a
pending destination may run" without cancellation intent would need a new
field.

Two ways on remain, and the reply names both in one sentence: repeat the
command once the placement is reachable, which ends the start if the
placement can then show that nothing runs; or `cancel`, which ends the
session. Some placement states cannot be shown to hold no writer however
often the command is repeated. On Kubernetes today these include a queued
AgentRun with no sandbox yet, a bound registration with no Pod, and intent
saved on the executor with no registry reference. For those `cancel` is the
only way on. That is a limit of what the placement can prove, not a state
the deadline created, and making such states provable is the placement's
work.

### Who takes the exit

Recovery and the live operations use the same event:

- **`recover`** continues the pending operation as before. Where the deadline
  refuses its next step, it takes `abandoned` and returns `ErrDeadlinePassed`.
- **`stop`** (`Finalize`) continues a move or first visit whose deadline has
  passed to its end. It then finalizes what is left: an idle execution, or the
  destination that did become active. Before the deadline it still says that
  the pending operation needs recovery.
- **`move`** does the same with a pending operation. A move of an idle
  execution whose deadline has passed is refused with `ErrDeadlinePassed`
  before anything is recorded.

### Home

When the current execution of a catalogued session is idle and not
cancelled, home retires it, so the session is `retained`. The session keeps:

- the stopped visit and checkpoint, for a move;
- the stop it had before, for a resume that never ran;
- the workspace its start captured, for a start that never ran. `resume`
  starts a new conversation from that workspace.

Home retires the execution after a `recover`, `move` or retried
`start` or `resume` that the deadline ended, and after a `recover` that
finishes a stop.

- `resume` ends an overdue start or move itself, then continues the session in
  a new execution with a new deadline.
- `recover`, `move`, and a retried `start` or `resume` with the same
  operation, reply `session ID reached its deadline before it started, so it
  is stopped with its work kept; continue it on this machine with "sproozi
  resume ID", which gives it a new deadline`.
- `stop` succeeds and stays idempotent.
- `status` of a session still `preparing` or `activating` after its deadline
  prints `Overdue`, names `sproozi resume ID`, and says that `sproozi cancel
  ID` ends the session if the placement cannot show that nothing runs there.
- While the placement cannot show that, every command replies `session ID
  reached its deadline before it started, but the placement has not shown that
  nothing runs at the destination: WHAT; repeat the command once Docker or the
  cluster is reachable to end the start with the work kept, or end the session
  with "sproozi cancel ID"`.

One next command remains for each state: `recover` before the deadline,
`resume` after it, and, while the end is unproven, the same command or
`cancel`, in one sentence of advice.

### The remote executor

The driver no longer cancels an interactive session whose execution is still
starting when its deadline passes. It recovers the execution, and the
coordinator takes `abandoned`. The session is `retained` with the problem `the
deadline passed before it started`, and `resume --remote` continues it. Tasks
are unchanged. A task was started on someone else's behalf and has nothing to
resume, so the driver still cancels it.

### Stored format

There are no new fields, phases or stage values. A journal left in `preparing`
past its deadline, with its launch saved, reads unchanged, and the next
`recover`, `stop`, `move` or `resume` takes the exit.

## Alternatives

- **Let `cancel` be the exit and make `resume` work after a cancellation.**
  This needs no new event. But cancellation is final in ADRs 0034, 0038 and
  0058, the executor's `CancelUnsettled` exists so that a kept result is never
  cancelled, and "cancelled" would come to mean both "discarded" and "stopped
  with its work kept". A user who chose `cancel` would also find the session
  resumable. Rejected: the exit must not depend on a command whose meaning is
  to give the work up.
- **Stop refusing `prepared` after the deadline.** The move would then finish
  and the session would run at its destination. But that starts work after the
  deadline, which is what the deadline exists to stop. The supervisor and the
  Docker placement refuse a launch past its deadline anyway, so the move would
  still fail, one step later. Rejected.
- **Record `unknown-stop` when the destination cannot be proven fenced.** It
  would name the risk in the phase. But the phase is derived from the record's
  structure, and no structure without cancellation intent describes it. A new
  field would break older binaries. Rejected while the journal has no
  migrations; the error names the risk instead.
- **A new `abandoned` operation stage.** It would read more clearly in the
  journal, but older binaries would refuse such a journal. The `cancelled`
  stage already means that an operation ended without its destination becoming
  active, and cancellation intent tells the two cases apart. Rejected.
- **End overdue starts from a timer at the deadline.** This would end the
  state without anyone acting. But the deadline would become an actor again,
  and every owner would need a timer. The next command, or the executor's
  driver, already reaches the state. Rejected.

## Tradeoffs and consequences

- A move that begins after the deadline, or whose source stop finishes after
  it, stops the session instead of moving it. `resume` always runs locally
  (ADR 0034), so continuing on a cluster means `resume`, then `move --remote`.
- Ending a start or move costs an `Inspect`, and an `Abort` when an
  incarnation exists, unless the operation has no launch or the placement
  fences preparations. The Kubernetes placement does not implement
  `PreparationAborter` on this branch. An absent launch is ended at once, a
  reported incarnation must abort with sealed usage, and anything else is
  refused with `ErrEndUnproven` until the placement can show it, or the user
  cancels.
- The transition table cannot tell a move's `preparing` from a stop's, which
  `finalized` leaves. Its principle test checks that every phase has an exit
  the deadline never refuses, and that every phase without cancellation intent
  can reach `idle` without cancelling. That test caught `activating`.
  Behavioural tests drive each starting state past its deadline through every
  command.
- On the executor, a task whose deadline passes while the driver is already
  recovering it can be ended rather than cancelled. It then finishes with the
  problem that it stopped before its conversation began, and nothing is kept.
- `Coordinator.PastDeadline` and `Coordinator.SetClock` are exported, so home
  decides with the coordinator's clock and owners' tests can pass a deadline
  without waiting.

## Revisit conditions

- Journal migrations become available. A structure for "a pending destination
  may run" without cancellation intent could then replace the error.
- A placement can start a writer without `Activate`, so a prepared incarnation
  is no longer inert.
- `resume` can run on a remote placement directly. A late `move --remote` could
  then continue there instead of stopping.
- The home catalogue becomes a declared lifecycle. Retiring an idle execution
  would then become one of its transitions.

## Links

- Implementation: `internal/session/phase.go`, `internal/session/coordinator.go`
  (`abandon`, `settleLate`), `internal/session/finalize.go`,
  `internal/session/cancel.go` (`retainStopped`),
  `internal/session/home/stop.go` (`retire`, `endLate`, `settledStop`),
  `internal/session/home/catalog.go`, `internal/session/home/socket.go`,
  `internal/session/home/exits.go`, `internal/work/driver.go`,
  `cmd/sproozi/output.go`
- Tests: `internal/session/phase_test.go`
  (`TestEveryPhaseHasAnExitAfterTheDeadline`),
  `internal/session/abandoned_test.go`, `internal/session/deadline_test.go`,
  `internal/session/home/late_test.go`, `internal/work/interactive_test.go`,
  `cmd/sproozi/deadline_test.go`
- Reference: [stopping, removing and
  recovering](../reference/session-commands.md#stopping-removing-and-recovering)
- Related: [ADR 0018](0018-coordinator-owns-every-visit.md),
  [ADR 0034](0034-resume-continues-every-home-session.md),
  [ADR 0038](0038-exited-agents-and-one-recovery-path.md),
  [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md),
  [ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
  [ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md)
