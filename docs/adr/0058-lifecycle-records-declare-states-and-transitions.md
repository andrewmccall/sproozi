# 0058: Lifecycle records declare their states and transitions

Status: accepted
Date: 2026-11-14

## Problem and constraints

Several trusted records step a writer through a lifecycle and persist where it
got to:

- the execution journal (`internal/session`, ADR 0018);
- the Docker placement record and the Docker gateway grant;
- the Kubernetes [session registration](../../GLOSSARY.md#session-registration);
- remote work (`internal/work`).

Each must survive a lost reply or a restart, and none may start a second
writer, refund unknown usage or resurrect cancelled work. Before this decision
they guarded their states in different ways.

The session registration already had typed phases, a from-phase check in every
mutator and per-phase invariants on read and write. The execution journal
checked every saved phase against the record's structure, but not the move
that produced it:

- About 23 `r.Phase =` assignments across `coordinator.go`, `cancel.go` and
  `finalize.go` were trusted.
- The rule for `cancelling` accepted any record with cancellation intent, so a
  forgotten assignment could still save.
- A recovery whose retried `Stop` failed after an earlier attempt had already
  fenced the writer tried to save `unknown-stop`. That save failed validation,
  so the journal kept `unknown-usage` but the caller received `unknown-stop`.

Callers outside the package also kept their own phase lists. The CLI's list of
phases whose deadline applies included `starting`, which nothing produces, and
omitted remote work's `pending` and `finalizing`.

The execution deadline added a second implicit rule. Starting or extending work
is refused once the deadline passes; stopping, accounting and cancelling are
not, so a stop the controller first sees after the deadline still completes
(ADR 0041). An execution may also have no deadline (ADR 0056). No record
stated which of its events the deadline refuses, so each new event got it right
only by copying an existing one.

Journals on users' machines are read with unknown fields refused, and there is
no migration step. A change must read existing journals.

## Decision

**A lifecycle record declares its states and transitions.** Each record that
adopts the pattern has:

1. **A typed phase** with a closed vocabulary, defined once beside the record.
   Callers in other packages use exported predicates rather than phase lists.
2. **Validation on load and save.** The stored phase must describe the
   record's structure. Where the structure determines the phase, one
   deterministic function derives it and validation requires stored equals
   derived.
3. **A `(from, event) → to` table.** Events are requests (create, move, stop,
   cancel) or outcomes of a call to another component (stopped, stop unproven,
   usage unsealed, prepared, activated). Each event lists the phases it may
   happen in and the one phase each leads to.
4. **One apply function** that moves the record by the table or refuses. Live
   operations and recovery both call it; recovery resumes with the same events
   as the live operation it continues. An undeclared transition is a
   programming error. The function refuses it, leaves the record unchanged and
   nothing is saved.
5. **A deadline column.** Each event says whether it is refused once the
   execution's deadline has passed. Events that start or extend work are
   refused: for the execution journal, `create`, `prepared` and `activated`.
   Events that stop, account for or cancel work are never refused. The check
   uses `internal/deadline`, so an execution without a deadline refuses
   nothing. It runs before the event's side effect: once the deadline has
   passed, the coordinator does not call `Prepare` or `Activate`. Recording an
   outcome that already happened, such as an activation whose reply was lost,
   is never refused.

**The execution journal adopts it now.** Its phase is derived from the
record's structure, except for one fact the structure cannot hold: whether
the last attempt to stop the writer proved nothing. That fact separates
`unknown-stop` from `stopping` or `cancelling`. `Phase` is still written, for
status compatibility and because ADR 0018 keeps `initialStage` as an alias of
`phase`. On load and save, the stored phase must equal `describe(record,
phase == unknown-stop)`. As a result:

- a writer proven stopped stays proven stopped. A retried `Stop` or `Abort`
  that fails leaves `unknown-usage`, not `unknown-stop`;
- cancelling an idle execution records `cancelled` at once, because nothing
  runs;
- cancelling a fenced writer whose usage is unsealed stays `unknown-usage`;
- `cancelling` means only that a writer may run or a pending destination may
  have started one.

A journal written before this decision may say `cancelling` for a record whose
structure now describes another phase. Load reads it as the derived phase.
Recovery carries out a cancellation from the structure alone, so nothing is
lost.

The table, in `internal/session/phase.go`:

| Event | From → to | Refused after the deadline |
| --- | --- | --- |
| `create` | none → `preparing` | yes |
| `move` | `idle` → `preparing`; `active` → `stopping` | no |
| `finalize` | `idle` → `idle`; `active` → `stopping` | no |
| `stopped` | `stopping`, `unknown-stop`, `unknown-usage` → `preparing` | no |
| `stop-unproven` | `stopping`, `unknown-stop`, `cancelling` → `unknown-stop`; `unknown-usage` → `unknown-usage` | no |
| `turn-pending` | `stopping`, `unknown-stop` → `stopping` | no |
| `usage-unsealed` | `stopping`, `unknown-stop`, `unknown-usage`, `cancelling` → `unknown-usage` | no |
| `prepared` | `preparing` → `activating` | yes |
| `activated` | `activating` → `active` | yes |
| `finalized` | `preparing` → `idle` | no |
| `cancel` | `idle` → `cancelled`; `preparing`, `activating`, `active`, `stopping`, `unknown-stop` → `cancelling`; `unknown-usage` → `unknown-usage` | no |
| `cancel-unsettled` | as `cancel`, but `idle` is refused: an owner that keeps a stopped execution's result ([ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md)) never cancels it | no |
| `cancelled` | `cancelling`, `unknown-stop`, `unknown-usage` → `cancelled` | no |

The exported predicates are `Live` (starting, running or stopping gracefully,
so the deadline applies), `Starting`, `Stopping`, `Unresolved` and `Settled`.
`sproozi sessions`, the service's refusals and `Removable` use them. The CLI
keeps remote work's own live phases in one named list until remote work runs
through the execution journal (the planned ADR 0057).

**Other records.** A parallel change brings the Docker placement record and
the Docker gateway grant to this pattern. The Kubernetes session registration
already follows it: typed phases, from-phase checks, compare-and-swap and
per-phase validation. Its guards may later move into a table without changing
behaviour. Remote work keeps its own phases until it runs through the
execution journal.

> **Correction, 2026-11-15.** Two statements in this paragraph were not
> true. Which records have a declared table today:
>
> - **With a table:** the execution journal, the Docker placement record
>   and the Docker gateway grant.
> - **Typed phases and guards, but no table yet:** the Kubernetes session
>   registration. It has from-phase checks, compare-and-swap and per-phase
>   validation, but no `(from, event) → to` table or deadline column, so it
>   does not yet follow the pattern.
> - **Remote work** no longer keeps its own phases: it runs through the
>   execution journal ([ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md)).
>
> The parallel change did not bring the Docker records to this pattern.
> Each kept a list of the phases every operation accepted, assigned target
> phases inline and checked the deadline inline; the grant validated only on
> load. Four checks ran before replay detection, against rule 5, so these
> replays were refused after the deadline: a Docker `Prepare` and `Activate`,
> and a grant `Initialize` and `Activate`. A Docker `Activate` replay of an
> active visit also rewrote it to `activating` and repeated the native
> activation, so a failed replay left it unattachable.
>
> Both Docker records now have rules 1 to 5. Each has an event type, a
> table beside the record, an `admit` step before each side effect and one
> `apply` function, the only place its phase changes. The grant validates on
> save as well as on load. Stored formats did not change.
>
> An operation whose outcome the record already holds replays it before
> admission and applies no event: `Prepare` of `prepared`, `Activate` of
> `active`, `Stop` of `stopped` and `Abort` of `stopped` or `aborted`, and
> for the grant `Initialize` of `inactive` or `active`, `Activate` of
> `active` and `Revoke` of `revoked`. A replay answers from the record
> alone. An `Activate` replay of an active Docker visit no longer
> re-verifies its containers or repeats the native activation, and one of an
> active grant no longer re-observes the container, re-reads its accounts or
> checks the deadline. A replay after the containers have died therefore
> succeeds instead of failing. Three things keep that safe:
>
> - an active record is written only after its side effects completed;
> - the placement reaches the grant's `Activate` only from `activating`,
>   after its own verification;
> - the placement's `Inspect` still verifies the containers, and the grant's
>   `Authenticate` and `Revalidate` observe the container on every request.
>
> The deadline column is consulted only by `admit`, which runs before a
> request's side effect. Outcome events (`activated`, `stopped`, `aborted`)
> and `abort-preparation` are applied without `admit`. So the Docker
> placement's `activated` row is not refused after the deadline: the
> request, `activate`, was admitted before the side effect, and refusing the
> outcome afterwards would refuse an activation that already happened. The
> execution journal has a single `activated` event that serves as both. It
> is admitted before the coordinator calls a placement's `Activate`, so its
> row says yes. The coordinator still records it without `admit` when
> `Inspect` reports the destination already active.
>
> The Kubernetes registration's `Store.Activate` checked the deadline
> before replay detection. It now replays an active registration first. The
> operation is still refused earlier after the deadline: the coordinator
> does not admit `activated`, and the placement's `Activate` refuses in its
> live-profile check before it reaches the store. Whether a late activation
> that already happened should be recordable through `Activate` is left to
> the execution journal's own work.
>
> The Docker placement record, in `internal/runtime/docker/transitions.go`.
> Its terminal phases are `stopped`, `aborted` and `preparation-aborting`;
> `Removing` and `Removed` are release flags on those, not phases. `Inspect`
> resumes an interrupted activation with `activate` and `activated`.
>
> | Event | From → to | Refused after the deadline |
> | --- | --- | --- |
> | `create` | none → `creating` | yes |
> | `prepared` | `creating` → `prepared` | yes |
> | `activate` | `prepared`, `activating` → `activating` | yes |
> | `activated` | `activating` → `active` | no |
> | `attach` | `active` → `active` | yes |
> | `stop` | `prepared`, `activating`, `active`, `stopping` → `stopping` | no |
> | `stopped` | `stopping` → `stopped` | no |
> | `abort` | `prepared`, `activating`, `active`, `stopping`, `aborting` → `aborting` | no |
> | `aborted` | `aborting` → `aborted` | no |
> | `abort-preparation` | `creating`, `preparation-aborting` → `preparation-aborting` | no |
>
> The Docker gateway grant, in `internal/gateway/docker/owner.go`. Its only
> terminal phase is `revoked`.
>
> | Event | From → to | Refused after the deadline |
> | --- | --- | --- |
> | `initialize` | none → `initializing` | yes |
> | `initialized` | `initializing` → `inactive` | yes |
> | `activate` | `inactive` → `active` | yes |
> | `revoke` | `initializing`, `inactive`, `active` → `revoked` | no |
> | `seal` | `revoked` → `revoked` | no |
> | `bind native` | `inactive`, `active`, `revoked` → unchanged | no |
>
> Two deadline checks stay outside the tables because they record no event:
> the grant refuses to authenticate after the deadline, and the placement's
> `Exited` stops an agent still running past it.

**Correction, 2026-11-15: two session registration guards changed.** Aborting
a Kubernetes preparation ([ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
section 13) can find a Pod the controller created after the registration
was cancelled. `BindPod` now also records that Pod on a `cancelled`
registration that has a bound run and no Pod, once, so the placement can
fence and seal that exact Pod. The record stays `cancelled` and grants
nothing. `Prepared` no longer treats a `cancelled` record that names the Pod
as a replay: it is a conflict, since such a record may name a Pod it never
restored into. Every other guard is unchanged. The registration's stored
format is unchanged.

**Correction, 2026-11-16: the session registration has a table.** The
Kubernetes session registration moves from "typed phases and guards, no table
yet" to "with a table", beside the execution journal and the two Docker
records, and now follows rules 1 to 5. Each mutator in
`internal/runtime/kubernetes/registry/registry.go` names an event, an `admit`
step checks it before the record changes, and one `apply` function is the
only place the phase changes. The inline deadline checks in `Create` and
`Activate` are the deadline column. Compare-and-swap, the per-phase
validation on read and write, the stored format and the exported API are
unchanged. No mutator's accepted phases, written phase or refusal changed: a
test runs every mutator from every stored state the registry accepts, before
and after the deadline and without one, and compares the outcomes with a
table recorded from the code before the change.

The table, in `internal/runtime/kubernetes/registry/transitions.go`. Its
terminal phases are `stopped` and `cancelled`.

| Event | From → to | Refused after the deadline |
| --- | --- | --- |
| `create` | none → `creating` | yes |
| `bind-run` | `creating` → `bound` | no |
| `bind-pod` | `bound` → `bound`; `cancelled` → `cancelled` | no |
| `prepared` | `bound` → `prepared` | no |
| `activate` | `prepared` → `active` | yes |
| `bind-native` | `active` → `active` | no |
| `stop` | `active` → `stopping` | no |
| `complete-stop` | `stopping` → `stopped` | no |
| `cancel` | `creating`, `bound`, `prepared`, `active`, `stopping` → `cancelled` | no |
| `complete-cancel` | `cancelled` → `cancelled` | no |

`create` starts an execution and `activate` grants it the gateway's
authority, so both start work. Every other event records an outcome the
remote executor already observed (a run created, a Pod scheduled, a restore
completed, a native UUID started), or stops, cancels or accounts for work.
`prepared` is the restore's outcome, not a request: the restore has
happened when it is recorded, and `prepared` grants nothing.

Some guards depend on more than the phase and stay in the mutators: `bind-pod`
needs no Pod yet and, on a `cancelled` registration, a bound run;
`prepared` needs no Pod or the same one; `bind-native` needs a first visit
from a Git workspace whose native UUID is unbound; `complete-cancel` needs a
bound run and Pod. `admit` checks the deadline before the phase, so an
`Activate` after the deadline reports the deadline from any phase except
`active`, as before.

These replay before admission and apply no event: `Create` of an existing
registration with the same intent, `BindRun` of the bound run, `BindPod` of
the bound Pod, `Prepared` of the Pod a `prepared` or later registration
restored into, `Activate` of `active`, `BindNative` of the bound UUID, `Stop`
of `stopping` or `stopped`, and `CompleteStop` of `stopped` with the same
receipt. `Cancel` of `stopped` or `cancelled` changes nothing: a stopped
registration is settled. `CompleteCancel` with the retained receipt replays
after admission, because only `cancelled` holds a cancellation receipt, and a
turnless stop's receipt would otherwise compare equal to one.

Two deadline checks stay outside the table because they record no event:
`ResolveRun` refuses an expired run unless its stop completed, and
`RequireActive` refuses an expired registration on every gateway request.

Corrected 2026-11-16: the home catalogue entry (`internal/session/home`)
adopts rule 1 only. Before, each caller worked out a session's state from the
entry's claim, current execution, last exit and source for itself, and status
reported a word from one of three vocabularies: `importing`, the stopped
words `retained` and `failed`, or the current execution's phase, including
`idle`.

- **Rule 1.** The phase is typed, with a closed vocabulary: `importing`,
  `retained`, `failed` and every execution phase except `idle`, which a
  session reports as `retained` or `failed`. One function,
  `derive(entry, record, pastDeadline)` in `internal/session/home/phase.go`,
  works it out from the entry and its current execution's journal record.
  An idle current execution, left when a stop's catalogue write failed after
  the journal recorded it, therefore reads as stopped.
- **Rule 2.** The entry stores no phase, so there is nothing to validate on
  load or save. Its stored format is unchanged.
- **Rules 3 to 5** are not adopted: the entry has no `(from, event) → to`
  table, no single apply function and no deadline column. In their place,
  one table, `nextSteps`, maps each derived state to the commands the
  service refuses and the one reply that names the commands that work, so a
  refusal and its advice cannot disagree. Writes to the entry (a claim, a new
  execution, retiring an idle one) are still made inline by the commands.

Reads derive without writing. A command holding the session's lock retires
an idle current execution, and releases the executions the entry never
recorded ([ADR 0063](0063-a-session-releases-executions-it-never-recorded.md)).
`cmd/sproozi/output.go` still works out for itself, against the CLI's wall
clock, that a starting session is overdue, rather than from `derive`.

Corrected 2026-11-16: the remote executor's driver (`internal/work`) declares
its decision as a table, and adopts none of rules 1 to 5. It keeps no record
of its own, so the rules have nothing to apply to. Before, its `step` was a
switch whose cases decided only in order. Now one table, `decisions` in
`internal/work/decision.go`, maps each situation to one action, `decide` looks
the action up, and `carryOut` takes it.

- **Rule 1** is not adopted. The driver adds no phase. It decides from the
  execution journal's typed phase, or "not received" when the coordinator
  does not hold the execution yet. Remote work's public phase is still
  derived by `derivePhase` in `internal/work/phase.go`, which this change
  did not touch.
- **Rule 2** is not adopted. The driver stores nothing, and the catalogue
  record stores no phase.
- **Rule 3** is not adopted. The table maps situations to actions, not
  phases to phases. Every phase change is still made by the execution
  journal, under its own table.
- **Rule 4** is not adopted, because there is no record to apply events to.
  In its place, `decide` is the only lookup and `carryOut` the only place
  the driver acts. `decide` refuses a situation that matches no row, or
  more than one, and the driver then does nothing.
- **Rule 5** is not adopted: there is no deadline column. The deadline is
  one input, `lateStart`, which only the rows for not received, `preparing`,
  `activating` and `idle` read. The journal's table still decides which
  events the deadline refuses.

A situation is the phase, the mode (task or interactive), whether the
coordinator holds a cancellation, whether the project is revoked, and
whether the deadline passed before the first visit started. Each of the 160
combinations matches exactly one row, so the order of the rows means nothing.
Some combinations cannot occur, such as a held cancellation of an execution
the coordinator has not received. They still have rows, and the test names
them. Two checks run before the table and stay outside it. A catalogue that
records a transfer, or a cancellation made before the coordinator received
the execution, ends the driver's work on the session at once. A journal that
cannot be read is retried after the backoff.

| Phase | Mode | Cancellation held | Project revoked | Late start | Action |
| --- | --- | --- | --- | --- | --- |
| `cancelled` | any | any | any | any | nothing |
| `idle` | interactive | any | any | any | nothing |
| `idle` | task | any | any | yes | end: deadline passed |
| `idle` | task | any | any | no | finish (write the task's tail) |
| `preparing`, `activating`, `active` | interactive | yes | any | any | nothing |
| `stopping`, `unknown-stop`, `unknown-usage`, `cancelling` | interactive | any | any | any | nothing |
| any started phase | task | yes | any | any | recover (with the backoff) |
| not received | any | any | yes | any | end: project revoked |
| `preparing`, `activating`, `active` | any | no | yes | any | end: project revoked |
| `stopping`, `unknown-stop`, `unknown-usage`, `cancelling` | task | no | yes | any | end: project revoked |
| not received | any | any | no | yes | end: deadline passed |
| not received | any | any | no | no | start |
| `preparing`, `activating` | task | no | no | yes | end: deadline passed |
| `preparing`, `activating` | task | no | no | no | recover the start |
| `preparing`, `activating` | interactive | no | no | any | recover the start |
| `active` | interactive | no | no | any | nothing |
| `active`, `stopping` | task | no | no | any | complete (the task's completion wait) |
| `unknown-stop`, `unknown-usage`, `cancelling` | task | no | no | any | recover (with the backoff) |

"Started phase" means every phase except not received, `idle` and
`cancelled`. "Nothing" means the session is settled, or its owner or
`recover` acts next (ADR 0057, section 8).

No outcome changed. A test drives the real coordinator from 900 starting
combinations: 15 states of the journal, counting no journal yet, both modes,
a member or revoked project, a
deadline that is ahead, passed or absent, and either three placement
behaviours or two catalogue tails. Seventy of these combinations cannot occur
and are recorded as unreachable. The test's recording,
`internal/work/testdata/driver-outcomes.txt`, was made from the code before
the change, and the code after the change reproduces it byte for byte. A
second test stops and cancels a session from every journal phase, both with
no deadline and with one that has passed, and checks that each settles.

## Alternatives

- **Check transitions in `save` by rereading the journal.** This catches an
  assignment that skips the helper, but it adds a read to every save and puts
  lifecycle rules in persistence. A deterministic derived phase catches the
  same mistake, because a stale phase no longer matches the structure. The
  table is enforced where the event is named.
- **Stop storing the phase and store a `stopUnproven` flag instead.** This
  changes the journal format, and older binaries refuse unknown fields. Status
  would also need the phase computed at every read. Storing the phase and
  validating it costs nothing and keeps the format.
- **Keep the permissive per-phase predicates and add only the table.** A
  forgotten `apply` would then still save whenever an old phase also fits the
  new structure, which is the original risk.
- **A generic state-machine package shared by all records.** Each table is
  about twenty lines and the records differ in what they persist. A shared
  framework would add a dependency between unrelated trust boundaries for
  little saving. The shared part is this pattern, not code.

## Consequences

- One test enumerates the table: every phase is reachable and only
  `cancelled` is terminal. Another test drives every stop outcome (sealed,
  unsealed, unknown) of `move`, `finalize` and `cancel`, and of their recovery,
  and asserts exactly one resulting phase each.
- A new event needs a table row, and that row must say whether the deadline
  refuses it.
- Phase-reporting changes callers can see: an idle cancellation reports
  `cancelled` without passing through `cancelling`, and cancelling a writer
  with unsealed usage stays `unknown-usage`. The end states are unchanged.
- The CLI shows a deadline for remote work in `pending` and `finalizing`, and
  for no phase called `starting`.

## Revisit conditions

- A release that needs journal migrations. A migration step would let the
  journal stop storing derivable phases and drop the legacy `cancelling`
  reading.
- Remote work moving onto the execution journal (planned ADR 0057), which
  removes the CLI's separate list of remote work's live phases.
- A record whose phase cannot be derived from its structure in a single pass.
  It would keep a stored phase validated against per-phase rules.

## Links

- Implementation: `internal/session/phase.go`, `internal/session/coordinator.go`,
  `internal/session/cancel.go`, `internal/session/finalize.go`,
  `internal/session/store.go`, `internal/session/remove.go`,
  `internal/session/home/exits.go`, `internal/session/home/remove.go`,
  `internal/session/home/phase.go`, `internal/session/home/reconcile.go`,
  `cmd/sproozi/output.go`, `internal/deadline`,
  `internal/runtime/docker/transitions.go`, `internal/gateway/docker/owner.go`,
  `internal/runtime/kubernetes/registry/transitions.go`,
  `internal/work/decision.go`, `internal/work/driver.go`
- Tests: `internal/session/phase_test.go`, `internal/session/rules_test.go`,
  `cmd/sproozi/deadline_test.go`, `internal/runtime/docker/transitions_test.go`,
  `internal/gateway/docker/transitions_test.go`,
  `internal/runtime/kubernetes/registry/transitions_test.go`,
  `internal/runtime/kubernetes/registry/outcomes_test.go`,
  `internal/session/home/phase_test.go`, `internal/session/home/reconcile_test.go`,
  `internal/work/decision_test.go`, `internal/work/driver_outcomes_test.go`
  (Corrected 2026-11-16: the home catalogue entry's files added, then the
  remote driver's)
- Reference: [status and listing output](../reference/session-commands.md#status-and-listing-output)
- Related: [ADR 0018](0018-coordinator-owns-every-visit.md),
  [ADR 0038](0038-exited-agents-and-one-recovery-path.md),
  [ADR 0041](0041-gateway-state-namespace-and-retained-state-owners.md),
  [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md)

## Later correction (2026-11-15)

The table had no exit from `preparing` or `activating` once the deadline
refused `prepared` and `activated`. A move that began after the deadline, or a
first visit interrupted before its activation and retried after it, could then
only be cancelled. [ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md)
adds the row:

| Event | From → to | Refused after the deadline |
| --- | --- | --- |
| `abandoned` | `preparing`, `activating` → `idle` | no |

Recover, stop and move take it after the deadline, once nothing can run at the
destination. The deadline check for `prepared` now runs before a move's launch
is created, and the check for `activated` runs whenever the destination is not
already active. A move of an idle execution after its deadline is refused
before anything is recorded. One test now checks that every phase has an exit
the deadline never refuses, and that every phase without cancellation intent
can reach `idle` without cancelling.
