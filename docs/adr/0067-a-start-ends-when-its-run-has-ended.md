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
- the same held when the run failed or was deleted while `Prepare` already
  waited, or its registration was cancelled meanwhile, which nothing in the
  executor does while `Prepare` holds the launch's lock: only an
  administrator, or another executor's abort of a launch whose local record
  was lost;
- with a deadline, the same task waited until the deadline, showed "Work owner
  unavailable; … recover this session", and the next recover ended it "the
  deadline passed before it started": the right end, the wrong reason;
- an interactive session showed the unavailable problem after 2 minutes, and
  every recover waited 2 more.

Related gaps made recovery worse:

- `Inspect` of a bound start whose run had no sandbox, queued or ended,
  failed with "exact admitted run unavailable", so a recover never called
  `Prepare` again.
- `AbortPreparation` refused a registration cancelled, as above, before any
  Pod was bound, as "not a preparation".
- A recover after the bound run was deleted, as its retention TTL does, made
  `ensureRun` create a second AgentRun at the same name, which `BindRun`
  refused; every later recover answered `conflict`, and the orphan run could
  later be admitted. This was older than this record.
- Once `AbortPreparation` has saved the launch's tombstone, `Inspect` and
  `Prepare` answered `conflict` for that launch, so an end the coordinator
  could not prove the first time was never attempted again by `recover`.

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
start's wait checks this; a stop's wait is unchanged. The wait reads the run
once per 100-millisecond poll, for the Pod and the refusal alike, and the
registration once a second.

So that every recover reaches the refusal:

- `Inspect` of a bound start whose run has no sandbox reports `preparing`, so
  a recover calls `Prepare`.
- `Prepare` of a registration that already bound a run checks that run before
  anything else, and only a registration with no run creates one: a bound run
  that is gone refuses the start and is never created again.
- A launch whose preparation `AbortPreparation` tombstoned is `preparing` to
  `Inspect`, and its `Prepare` refuses it. The reason is how its run ended
  when the run is still terminal, otherwise "the preparation of AgentRun NAME
  was aborted before its sandbox started". Another launch for the same
  allocation still answers `conflict`.
- `AbortPreparation` fences a registration cancelled with no Pod ever bound:
  it was never prepared or active, so it never had authority.

### The coordinator ends the start

When `Prepare` returns `StartRefused`, the coordinator takes ADR 0060's
`abandoned` event, after the same proof that nothing runs at the destination:
`AbortPreparation` where the placement fences preparations, otherwise
`Inspect` and an abort of any incarnation reported. The execution is `idle`
with its last checkpoint. The error is `StartRefused` marked ended, carrying
the reason; it and `ErrDeadlinePassed` both match `ErrStartEnded`, which a
stop that settles a pending start also accepts. If the proof fails, nothing
is recorded and the error is `ErrEndUnproven` wrapping the refusal; the next
recover is refused again and repeats the proof.

Any other `Prepare` failure leaves the start `preparing` for recover, as
before.

The journal keeps no reason. A replay of the ended operation reports
`Coordinator.StartEnded`: `ErrDeadlinePassed` once the deadline has passed,
otherwise `StartRefused` with no reason.

### The remote executor's driver

The driver records the problem `it can never start: REASON` when `Recover`
reports a refusal, deadline or none. The decision table gains a situation bit,
`refused`, and an action, `end-refused`. Only an idle execution whose first
visit ended before it started has `refused`, and never with `lateStart`: the
coordinator ends a start for its deadline only once the deadline has passed,
so before it, or with none, the placement refused it, and past it the
refusal problem the driver recorded decides.

- a task whose start was refused is cancelled through `end()` with operation
  `start-refused`, keeping that problem, as `end-expired` and `end-revoked`
  end one. A task whose problem was not recorded gets `it can never start:
  its placement refused it`.
- an interactive session stays `retained` with the problem, and `resume
  --remote` continues it, as for a start its deadline ended.

While the end is unproven, the problem adds "the executor has not yet shown
that nothing runs there and tries again" for a task, which the driver
recovers again after its backoff, and ", so recover or cancel this session"
for an interactive session, which waits for its owner. A later failure of
that recovery replaces the problem with its own, as any failed recovery's
does. An interactive session whose ended start has no recorded problem shows
`it can never start: its placement refused it`, or the deadline's problem
once the deadline has passed.

### Over the placement protocol

A refused `prepare` answers `refused` with HTTP 409 and a `reason` field,
which only that code carries; the executor answers it to no other action. The home's client reads it as `StartRefused`,
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
What remains:

- Past its deadline, a start the placement refused is told from one the
  deadline ended only by the refusal problem the driver recorded. If the
  executor stopped between the coordinator's commit and saving it, and the
  deadline has passed since, the session is ended as "the deadline passed
  before it started". Before the deadline, or with none, the coordinator's
  clock decides, and the problem says only "its placement refused it".
- The journal keeps no reason, so a replay of an ended start reports the
  deadline once it has passed and a reasonless refusal before it. A recover
  of a tombstoned launch says again what became of the run the registration
  bound (deleted, replaced, ended or cancelled), or that the registration
  was deleted, read now; when it can read none of these, only that the
  preparation was aborted. A run an administrator replaced is reported as
  replaced, never by how the replacement ended.
- For a run an administrator replaced, or a registration one deleted, the
  start is refused but its end stays unproven: nothing proves that no Pod
  of the bound run remains. The session stays `preparing`, as ADR 0060
  records for those cases, and `cancel` meets the same gap. The placement
  states the cause alone (`session.UnprovenEnd`): "AgentRun … now has UID …;
  an administrator replaced it, so confirm no Pod labelled … remains", or
  "the session registration … was deleted, so its Pod cannot be fenced; an
  administrator must confirm no Pod labelled … remains". The driver's
  problem carries it after the refusal, the executor logs it once per
  session and cause, and the driver recovers a task again on its backoff,
  capped at a minute, at about four API calls a try, until an administrator
  acts; an interactive session waits for its owner's `recover`. A deleted
  registration's tombstone is saved too, so each recover repeats the check.
- `Inspect` of a launch whose preparation was aborted reports `preparing`
  while its registration is not cancelled, cannot be read, or is cancelled
  with its run not yet ended or its Pod still present; `absent` once the run
  ended and no Pod remains, which is also what a coordinator whose commit of
  the end was lost meets, and ends through `Prepare`; and conflict, as
  before, for a registration that may have had authority.
- A task whose deadline has passed before the driver steps it is ended as
  expired without asking the placement, as before.
- The wait reads the run once per 100-millisecond poll, as it read it for the
  Pod before, and the registration once a second.
- `driver-outcomes.txt` gained the `preparing-start-refused` and
  `idle-start-refused` states. Later, 24 lines of `idle-start-ended` task
  cases on the early clock changed from expired to refused: those states are
  built past the deadline and stepped before it. A wall clock stepped back
  across the deadline reaches them, and then mislabels only the end, its
  cancel operation's name and its problem, never the end state.

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
