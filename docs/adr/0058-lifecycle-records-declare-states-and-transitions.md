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

The remote executor's retained record of a Kubernetes visit
(`internal/runtime/kubernetes/placement/storage.go`) adopts rule 2 only. It
stores no phase: the visit's phase is the session registration's. One
function, `check`, validates the record on every load and save, and beside the
registration on every read and every save once the registration exists. What a
contradicted record does to the operations that read it is
[ADR 0066](0066-a-contradicted-retained-record-refuses-work-and-never-blocks-an-end.md).

The gateway's record of a failed ChatGPT refresh
(`internal/modelauth/refresh.go`,
[ADR 0069](0069-a-failed-chatgpt-refresh-is-remembered.md)) adopts rules 1, 3
and 4: a typed state, a `(from, event) → to` table and one `apply` function.
It is held in memory only, so rule 2's check runs on every move rather than
on load and save, and it has no deadline column because no execution deadline
applies to it.

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

Corrected 2026-11-17: the remote executor's driver keeps no lifecycle record,
so it adopts none of rules 1 to 5; its decision is a table of its own,
described in [ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md).

Corrected 2026-11-16: the [supervisor](../../GLOSSARY.md#supervisor)
(`cmd/native-session`) adopts rules 1, 3, 4 and 5. Before, a typed state and
separate fields (`cause`, `quiescing`, `ended`, `gatedAt`, `turnless` and the
exit codes) together decided what each path could do.

- **Rule 1.** `lifecycle.go` holds one `lifecycle`: a phase with a closed
  vocabulary and the facts that belong to it (the cause, when input was first
  gated, the turnless latch and the exit codes). The phases are
  `unconfigured`, `prepared`, `active`, `gated`, `finishing`, `forcing` and
  `ended`. `finishing` means a graceful stop owns the harness's exit.
  `forcing` means the supervisor is killing the harness for a cause already
  decided. The window lasts from the kill until it is confirmed, at most
  three seconds, and through the sign-in save that follows, at most five
  more (`loginSaveWait`). In it status already reports `stopped`, and either
  the stop or the exit watcher may settle the exit.
  Status and every control reply still report the protocol's five states,
  derived by one function: `gated` and `finishing` report `stopping`;
  `forcing` and `ended` report `stopped`.
- **Rule 2** is not adopted: the supervisor stores nothing, so there is no
  load or save to validate.
- **Rules 3 and 4.** The table is below. One `apply`, called under the
  supervisor's mutex, is the only place the lifecycle changes. Every path
  that changes it calls `apply`: `configure`, `activate`, `gate` and
  `quiesce`, the turnless observation, the exit watcher, the stops for
  `SIGTERM`, the deadline and a failed graceful stop, and the graceful stop
  itself. `apply` refuses an undeclared change and leaves the lifecycle as it
  was. Two requests can answer as replays. `activate` of an active
  supervisor returns its PID before admission. `configure`, which the table
  admits only from `unconfigured`, compares the contracts in every other
  phase.
- **Rule 5.** Only `configure` and `activate` are refused after the deadline.
  `activate` is checked on entry and again just before the launch.
  `configure` is refused first by the launch contract's own validation. One
  owner, `deadline.go`, raises the `deadline` event once
  ([ADR 0064](0064-the-supervisor-alone-stops-a-harness-at-its-deadline.md)).
  Without a deadline nothing raises it, and every phase but `ended` can
  still end without it.

| Event | From → to | Refused after the deadline |
| --- | --- | --- |
| `configure` | `unconfigured` → `prepared` | yes |
| `activate` | `prepared` → `active` | yes |
| `gate` | `active`, `gated` → `gated`; `finishing` → `finishing` | no |
| `turnless` | `gated`, `finishing`, `forcing`, `ended` → unchanged | no |
| `finish` | `gated` → `finishing`; `forcing`, `ended` → unchanged | no |
| `finished` | `finishing`, `forcing`, `ended` → `ended` | no |
| `terminate`, `deadline`, `fail` | every phase but `ended` → `forcing`; `ended` → `ended` | no |
| `exited` | `active`, `gated`, `forcing`, `ended` → `ended`; `finishing` → `finishing` | no |

`TestSupervisorLifecycleRecord` drives every event through every state the
supervisor reaches: for Codex, Claude Code and Copilot, first and resumed
visits, with and without a deadline and with one that passes during the
events, each with a real sign-in sync. A state includes its cause, whether
input was gated and the turnless latch. The record holds the replies, the
states status reports, what reached the harness and what the sign-in sync
had recorded by then, the login store, the exit reports, the log lines and
the exit codes. Its 1020 steps, written from the supervisor before this
change, were identical after it. Outside the record, four things differ:

- a stopped `quiesce` reply's `exit.reason` is never `deadline`, which only
  the terminal's own kill gave;
- a `quiesce` waiting for a turn sees a passed deadline at its next 100 ms
  poll, where before it read the clock once a second;
- when the deadline is raised after the terminal has reaped the harness's
  own exit, the exit is the harness's own (`exited`), where before it was
  either; an own exit the terminal has not yet reaped can still be reported
  as `deadline`;
- in `forcing` the exit watcher may settle the exit, where before, once a
  graceful stop had been overtaken, only the stop did. Both settle the same
  exit and wait result unless the stop's three-second wait ran out first,
  in which case the stop settles it as before; whichever concludes does so
  after the one sign-in save. The cause, codes, report and save are the
  same.

Four changes followed, each a fix with its own test. `activate` no longer
starts a harness when the deadline passed while it prepared the launch. The
exit watcher no longer concludes an exit that a graceful stop settled, which
let the supervisor exit before the stopped reply was read. A forced stop that
overtakes a graceful end leaves the supervisor's exit code nonzero. A
`quiesce` whose Codex quit, or turnless kill, a forced stop overtook replies
`stopping` with an error naming the cause, which changed 12 recorded steps;
Claude Code and Copilot, ended by a kill after their completed turn, still
reply `stopped`.

Corrected 2026-11-17: the gateway certificate store's rotation record
(`internal/pki`, [ADR 0029](0029-local-provisioning-and-lifecycle.md) and
[ADR 0030](0030-kubernetes-release-assets-and-certificates.md)) adopts rules 1
to 4. The local service and the certificate controller share it. Before, its
stage was an untyped string beside the record, load checked only the version,
the serving generation and that something was trusted, and each mutator
assigned the serving generation, trust and stage inline.

- **Rule 1.** The [rotation phase](../../GLOSSARY.md#rotation-phase) is typed,
  with a closed vocabulary: `single`, `prepared` and `switched`.
- **Rule 2.** The record stores the phase as its rotation's stage, with no
  rotation meaning `single`. One function, `describe` in
  `internal/pki/record.go`, derives it from the record's structure: which
  generations exist, which are retired, which are trusted and in what order,
  which serves, and the leaf's file name. Load and save require the stored
  stage to equal the derived phase, and refuse with an error naming what is
  wrong. They also require each trusted generation's certificate, and each
  retired generation's kept certificate, to have the recorded fingerprint and
  expiry. They read no key: a key is checked by the step that uses it, and
  by `Ensure` after its leaf and trust work, which reports an unreadable or
  mismatched trusted key with its true cause. A missing key is the lost-key
  state ADR 0029 describes. `Initialize` reads the record without the files,
  and `ReplaceCA` without validating it, so replace
  still recovers an authority whose files were lost or changed. An explicit
  replace is also the way out of a record no phase describes, or whose
  certificates are not the ones it records, as it was before: it rebuilds the history as generations 1 to N, all retired, N the
  highest the record lists or a `ca-N` file belongs to, and moves from the
  `refused` pseudo-phase to `single` on generation N+1. Every other event
  refuses such a record and writes nothing. Replace refuses only a record it
  cannot parse, or one of another version.
- **Rules 3 and 4.** The table, in `internal/pki/transitions.go`, is below.
  Each mutator admits its event before its side effect (creating a
  generation's files) and one `apply` is the only place the record's
  generations, trust, serving generation and rotation change. Two tests guard
  it: one observes every `apply` and requires each operation to apply its own
  event once, and one reads the package's source and refuses an assignment to
  a structure field outside `apply`. Issuing a leaf
  is not an event: it changes only the leaf, in every phase.
- **Rule 5** does not apply: the record has no deadline, and no event is
  refused because time passed. `RotationDue` only starts a rotation.

| Event | From → to |
| --- | --- |
| `initialize` | none → `single` |
| `prepare` | `single` → `prepared` |
| `switch` | `prepared` → `switched` |
| `rollback` | `switched` → `prepared` |
| `retire` | `switched` → `single` |
| `replace` | `single`, `prepared`, `switched`, refused → `single` |

One idempotent pass destroys the key of every retired generation, both files
of a generation numbered after the record's last (an interrupted prepare's),
and every leaf file the record does not name. It removes only names the store
writes: `ca-<N>.pem`, `ca-<N>.key` and `leaf-<16 hex>` files. `Ensure`
runs it, so the certificate controller runs it on every reconcile and the local service at start and every
minute; `retire`, `replace` and every leaf issuance run it after their commit.
Before, a crash or failed removal between committing a retirement and removing
the key left the key for good, and the controller committed it to the
authority Secret. The pass never removes a trusted generation's key or the
named leaf. It runs under the store's lock, which every operation that writes
a generation or leaf holds until its commit. A removal that fails does not
stop `Ensure`: it renews the leaf and writes the trust bundle, then returns
the failure, as it returns a broken trusted key, marked `ErrOutstanding`.
The certificate controller commits and publishes that reconcile's work and
then fails it, so the log reports it every interval. The local service logs
it at start and on every check and carries on: it starts, reloads the renewed
leaf and advances a rotation.

Validation finds damage and mistakes; it cannot authenticate the record.
Whoever can write `state.json` can write every file beside it, and on
Kubernetes a one-key edit needs write access to the whole authority Secret,
so the store directory's permissions and the Secret's RBAC are what protect
it. An edit of `state.json` alone can, from a replaced store, un-retire
generation 1 and record `switched 1 to 2` with both trusted, after which
`Ensure` puts the retired certificate back in the trust bundle (its missing
key is the accepted lost-key state); and marking a generation retired makes
the next `Ensure` destroy its key without a `retire` and its drained check.
A blank retired entry (no fingerprint, no expiry), which replace writes only
for history it cannot recover, is accepted only while that generation's
certificate is gone or unreadable.

Every step that destroys a key, or stops trusting a generation, first proves
what it will depend on:

| Step | Destroys or stops trusting | Proves first |
| --- | --- | --- |
| `retire` | the outgoing generation: trust and key | the phase is `switched`, the caller asserted drainage, and the serving key reads and matches its recorded certificate |
| `replace` | trust in every earlier generation, every earlier key | it created the new authority, issued and committed a leaf signed by it; keys are destroyed only after that commit |
| destroy pass (`Ensure`, after every commit) | keys of retired generations, files of a generation numbered after the record's last, leaf files the record does not name | only the committed record: it never touches a trusted generation's key or the named leaf |
| issuing a leaf | the previous leaf | the new leaf was issued from the checked serving key and committed |

`commit` refuses a record larger than the 64 KiB load reads. A move that
starts something (`prepare`, `replace`, a change of serving names) is
refused, before it writes anything, unless the record leaves a fixed reserve
of 1024 bytes below that; everything that can follow before the store is
`single` again adds at most about 360 bytes (294 measured), so `switch`,
`rollback`, `retire` and renewal with unchanged names always commit. A
history that has grown too long therefore refuses the next rotation instead
of being written and then unreadable, or starting a rotation it cannot
finish. Replace on a refused record refuses, naming the stray `ca-N` file
or the generation id, when the history it would rebuild leaves less than
the reserve. Open follow-up: at that bound, no local command starts a fresh
authority; `sproozi init` refuses an installation whose authority state is
missing. An
interrupted replace of a refused record, run again, ends in a valid
`single` store with every earlier key destroyed, but numbered one higher
than an uninterrupted run, the interrupted attempt's certificate kept as a
retired generation; on Kubernetes the scratch store is discarded, so a
retried request gives the same number.

`testdata/store-outcomes.txt` and `testdata/reconcile-outcomes.txt`, recorded
from the code before this change, hold the outcome of every operation and
rotation request from every written state, every state an interruption leaves
and 45 corrupted records. The only differences are the changes above:
undescribed records and mismatched files are refused by everything but
replace; retired CA keys, uncommitted generations and unnamed leaf files are
destroyed; and replace of seven corrupted records changed, each a fix (three
it refused, two where it overwrote an existing CA's files, one where it left
a key, and one where it removed files outside the store; commit `3396e12`'s
message says six). A broken trusted key no longer refuses load: those rows
are again those recorded before the change, except that `Ensure` reports the
key. `Ensure` with the serving key lost now reports it, and five rows where a
broken serving key fails a renewal or switch now name the generation and
file. Records the code wrote before still load.

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
  `cmd/native-session/lifecycle.go`, `cmd/native-session/deadline.go`,
  `cmd/native-session/main.go`,
  `internal/runtime/kubernetes/placement/storage.go`,
  `internal/pki/record.go`, `internal/pki/transitions.go`,
  `internal/pki/store.go`
- Tests: `internal/session/phase_test.go`, `internal/session/rules_test.go`,
  `cmd/sproozi/deadline_test.go`, `internal/runtime/docker/transitions_test.go`,
  `internal/gateway/docker/transitions_test.go`,
  `internal/runtime/kubernetes/registry/transitions_test.go`,
  `internal/runtime/kubernetes/registry/outcomes_test.go`,
  `internal/session/home/phase_test.go`, `internal/session/home/reconcile_test.go`,
  `cmd/native-session/lifecycle_test.go`, `cmd/native-session/lifecycle_record_test.go`,
  `cmd/native-session/lifecycle_paths_test.go`,
  `internal/runtime/kubernetes/placement/retained_record_test.go`,
  `internal/pki/record_test.go`, `internal/pki/transitions_test.go`,
  `internal/pki/destroy_test.go`, `internal/pki/interrupt_test.go`,
  `internal/pki/outcomes_test.go`, `internal/certificates/outcomes_test.go`,
  `internal/certificates/interrupt_test.go`
  (Corrected 2026-11-16: the home catalogue entry's files added; the
  supervisor's files added. 2026-11-17: the Kubernetes retained record's
  files added; the certificate store's files added)
- Reference: [status and listing output](../reference/session-commands.md#status-and-listing-output)
- Related: [ADR 0018](0018-coordinator-owns-every-visit.md),
  [ADR 0038](0038-exited-agents-and-one-recovery-path.md),
  [ADR 0041](0041-gateway-state-namespace-and-retained-state-owners.md),
  [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md),
  [ADR 0066](0066-a-contradicted-retained-record-refuses-work-and-never-blocks-an-end.md)

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
