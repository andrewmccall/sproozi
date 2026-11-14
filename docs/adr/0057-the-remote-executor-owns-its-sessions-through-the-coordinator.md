# 0057: The remote executor owns its sessions through the coordinator

Status: accepted
Date: 2026-11-14

This record calls `session.Coordinator` **the coordinator**, as
[ADR 0018](0018-coordinator-owns-every-visit.md) does. The glossary's
[Coordinator](../../GLOSSARY.md#coordinator) is Hermes, a persistent assistant;
public documentation keeps saying "owner" and "executor" and never uses
"coordinator" for the execution journal.

## Problem and constraints

ADR 0018 gave every visit of a home execution to one state machine, the
coordinator, because two state machines for the same safety rules had
diverged. It left remote work out: "Remote-first work still runs its first
visit through the work owner, not the home coordinator" (0018, Remaining
work). That second machine lived in `internal/work`, and it had the problems
0018 removed at home.

**The work owner kept its own execution lifecycle.**

- It had ten stored phases and a `Progress` field. After any error it saved
  `unknown` and copied the old phase into `Progress`; `recover` copied it
  back.
- It called the placement directly (`CreateInitial`, `Activate`, `Stop`, and
  `Inspect`/`Abort` on cancellation), so the coordinator's rules never saw
  those calls.
- Reading a record checked identity and intent but never phase against
  structure. The coordinator checks phase on every load and save.
- It merged an unproven stop and unsealed usage into one `unknown`, with one
  message, "writer stop and usage remain unconfirmed".

**Defects that followed.** Each now has a regression test that failed before
this change (`internal/work/regression_test.go`).

1. *Cancel lost updates.* `Cancel` read the record and saved `cancelling`
   before it took the job lock, and `save` refused only a write that would
   drop a cancellation. A stop that succeeded in between lost its retained
   result: the session became `cancelled` after a completed task, and its
   checkpoint file was left with nothing pointing to it.
2. *An unstarted task that expired could not settle.* It failed with
   "unstarted task expired", became `unknown` with `Progress: pending`, and
   `recover` sent it round the same loop. Only `cancel` ended it.
3. *A task longer than the supervisor's quiesce wait became `unknown`.* The
   owner's `Stop` was its completion wait; after `NativeQuiesceWait` (two
   minutes) the placement returned `ErrNativeTurnPending`, which the owner
   recorded as an unknown stop until someone ran `recover`.

**Two placement gaps** stopped the coordinator from simply replacing the
owner: the Kubernetes placement started a first visit from a Git workspace
only through its separate `CreateInitial`, and a Kubernetes first visit
stopped before any turn was an error. Both are fixed on this branch before
this record (the placement's `Prepare` accepts a fresh launch, and a turnless
first visit stops as on Docker). The placement protocol also gained the
`turn_pending` outcome, so a remote stop still waiting for the turn is not
mislabelled as unproven ([remote executors](../reference/remote-executors.md)).

**The owner wants more than tasks**, recorded here as direction for later
steps: interactive sessions on the cluster for Claude Code as well as Codex;
starting work, attaching, then closing the laptop; and eventually the
coordinator itself on the cluster, with the laptop service optional.

**Constraints that remain.**

- Pull stays a one-way ownership transfer
  ([ADR 0012](0012-retained-work-and-fresh-session-continuations.md),
  [0019](0019-pull-operations-and-work-owner-names.md)). One principal stands
  for one home.
- The executor stays one process with locked directories on a private volume,
  reached through a context and authorized by TokenReview and RBAC
  ([ADR 0036](0036-remote-executors-through-kubeconfig-contexts.md)). It serves
  one placement and one harness.
- Hermes keeps `sproozi_run`, `sproozi_status`, `sproozi_cancel` and
  `sproozi_sessions`. The work token never gets ownership actions.
- Sproozi has releases. Under the release policy a minor release may break
  session journals with a `BREAKING CHANGE:` footer and operator steps
  ([ADR 0044](0044-the-api-stays-alpha-with-a-written-policy.md)).
- [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md): whoever starts
  work sets its deadline. Work the owner starts may have none; work Hermes
  starts should have one.

## Decision

### 1. The executor owns its sessions through the coordinator

The remote executor opens a coordinator over `<directory>/sessions`, whose
only placement is its in-process Kubernetes placement under its
`placementName`. It is the [owner](../../GLOSSARY.md#owner) of every session
it starts. It drives each execution only through the coordinator's `Create`,
`Recover`, `Finalize`, `CancelUnsettled`, `Inspect` and `Checkpoint`, plus
`Workspace` for a resumed interactive session. No lifecycle step in
`internal/work` calls a placement method any more, and the placement's
`CreateInitial` is removed. The one exception changes no lifecycle state: an
interactive session's `attach` and exit check call the placement's `Attach`
and `Exited` on the visit the coordinator records as active (section 8).

`internal/work` becomes the executor's **session catalogue**, the counterpart
of home's catalogue. Each record (format version 2) keeps only what the
coordinator does not:

| Kept in the work record | Kept in the coordinator journal |
| --- | --- |
| The submission intent, the effective deadline choice and their digest; the session ID derived from the principal and `requestId` | Every launch after the first, every allocation, epoch, incarnation, visit and sealed receipt, the remaining allowance, the deadline and the committed checkpoint |
| `first`: the first execution's epoch-1 launch, fixed at submission so that a retried `Create` sends the same launch | The retained workspace the first visit started from |
| The project association and repository source | Phase, validated on every load and save |
| `executions` (the first is the session ID) and `current` | Pending operation and cancellation intent |
| A catalogue-only cancellation, for a session the coordinator never received | |
| The task tail: `finished`, the bounded untrusted `result`, `resultTruncated` and `checkpointInfo`, written once after the coordinator commits the stop | |
| The transfer tail: `transfer` (the pull operation) and `pulled` | |
| An owner-level `problem` | |

The record loses `phase`, `progress`, `launch`, `incarnation`, `stopped`, the
private `.input` file and the `.checkpoint` file. Reading a record checks its
own invariants: the version, the intent digest, the session ID, a first launch
bound to this executor, a non-empty `executions` list whose first entry is the
ID, and `current` in that list. The first launch is the one difference from
the draft of this record, which dropped every launch: without it a restart
between `Create` and the catalogue's next write could not repeat `Create` with
the same allocation. It holds no host path.

**The public phase is derived, never stored.** It is the coordinator's phase
of the current execution, combined with the two tails:

| Derived phase | When |
| --- | --- |
| `pulled`, then `exporting` | The transfer tail is set; it wins over everything else. |
| `preparing` | The coordinator has not received the execution yet: the input is being captured. |
| `cancelled` | The coordinator's phase, or a cancellation the catalogue recorded before the coordinator received the execution. |
| `preparing`, `activating`, `active`, `stopping`, `unknown-stop`, `unknown-usage`, `cancelling` | The coordinator's phase, reported as it is. |
| `stopping` | The coordinator is `idle` but the task tail is not written yet. |
| `retained` | `idle`, the task tail written, a checkpoint committed and no failed exit. |
| `failed` | `idle` and the harness exited with a failure, or, for a task, nothing was kept. An interactive session that kept no conversation is `retained`, since `resume` restarts it from its workspace. |

This is home's vocabulary, so a row reads the same whichever owner lists it,
and the CLI needs no separate list of remote live phases. `unknown`,
`finalizing` and `pending` disappear.

### 2. How a task runs

One background driver per session, in this order:

1. **Capture the input.** A repository snapshot, an empty workspace with
   fetch instructions, or a `workspaceFrom` revision. Nothing is kept until
   the coordinator retains it, so a restart before `Create` captures again.
2. **`Create(first, workspace, "")`**, under the catalogue's lock, and only
   when the coordinator does not hold the execution yet.
3. **`Recover`** prepares and activates the first visit.
4. **`Finalize(task-complete)`**, a name-based operation UUID, so restarts
   repeat the same operation. The stop's graceful handoff succeeds only once
   the supervisor has seen the task's own completed turn. On
   `ErrNativeTurnPending` the stop stays pending, the phase stays `stopping`,
   the problem names what it waits for, and the driver calls `Finalize`
   again. Defect 3 is gone.
5. **Write the task tail** from the committed checkpoint
   (`checkpoint.FinalText`). The phase is then `retained`.

Failures:

- **A deadline that passes before activation** is final. The driver cancels
  with a name-based operation and records "the deadline passed before it
  started"; before `Create`, the catalogue records that cancellation itself.
  `recover` does not start it again. Defect 2 is gone.
- **`unknown-stop` and `unknown-usage`** stay as the coordinator records them,
  and the driver retries `Recover` with a capped backoff (one second doubling
  to a minute) while the executor runs. Recovery never makes up a stop or a
  refund, so this is safe, and Hermes is unattended. `recover` retries at once.
  Interactive sessions wait for `recover`, also after a restart (section 8).
- **A failed preparation or activation** records the outcome as the problem
  and waits for `recover`, as before.
- **A revoked project** is cancelled through the coordinator with a
  name-based operation, whether found at startup or while driving.

**Cancel goes through the coordinator** as
`CancelUnsettled(ctx, execution, op, admit)`. The coordinator first
interrupts the driver's graceful `Finalize`, so cancellation never waits for a
turn. `admit` takes the catalogue's lock, refuses a transferred session, and
settles a session the coordinator never received in the catalogue alone. The
work layer writes nothing outside that lock. `CancelUnsettled` is a new event
in the coordinator's table ([ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md)):
`cancel` without the `idle` row. It decides under the execution's lock, so a
stop that commits while the cancellation waits wins, the result is kept and
the cancellation is refused as a conflict. Defect 1 is gone.

**Completion stays the supervisor's to observe.** For Codex, the stop succeeds
once the supervisor finds a completed turn of the submitted task. Claude tasks
(a later step) bind completion to the task's own prompt in the same way.

### 3. Deadlines

The executor gains `workTokenMaxDuration`, required when `workTokenFile` is
set and within `maxDuration` when that is set. It caps only work submitted
with the work token: such work without a choice gets that deadline, and
`none` or a longer one is denied. The executor's own `maxDuration` stays
optional, so work its owner submits may have no deadline. The intent digest
covers the effective choice (a duration, or `none`), so a retry under another
cap is a different intent.

### 4. Retention

The executor gains `retainedBytes`, at least twice `maxCheckpointBytes` as at
home, defaulting to 1 GiB or twice the checkpoint limit when that is larger.
A submission is refused once the bytes kept in `work/` and `sessions/`, plus
one checkpoint for every unsettled session and for the new one, would exceed
it. The 1024-record cap stays.

### 5. Pull

The wire format and home's side are unchanged. `begin` and `claim` build the
export from the coordinator: the last visit's launch, incarnation and sealed
receipt, the execution's thread and the committed checkpoint. Export needs the
execution `idle` with nothing pending, no cancellation and a checkpoint, and
the task tail written. The transfer tail lives in the work record and is
checked under the catalogue's lock. After a pull, home's coordinator owns new
executions and the executor's journal is history.

### 6. Migration

The work record format changes from version 1 to 2 in a minor release, with a
`BREAKING CHANGE:` footer. The executor converts nothing.

- **Stopped version 1 records stay readable**: `retained`, `exporting`,
  `pulled` and `cancelled` list, export and claim from the old record and its
  `.checkpoint` file, so retained work can still be pulled. The reader is
  `internal/work/legacy.go`; remove it one minor release later.
- **The executor refuses to start with a live version 1 record** (`pending`,
  `preparing`, `active`, `finalizing`, `cancelling` or `unknown`). The error
  names each record and says to finish or cancel it with the previous release
  first.
- The coordinator directory `sessions/` is new and starts empty.

### 7. Later steps, and the direction they keep open

Not built here, each with its own change:

- **The CLI for interactive sessions** (`run --remote` without a task;
  `stop`, `resume` and `attach` with `--remote`), and its capture of a local
  `PATH` to upload. The executor's side is section 8.
- **Claude Code on the cluster**: interactive first, then tasks, with
  completion bound to the task prompt and `FinalText` for Claude. Task
  submissions for any harness but Codex stay refused until then. One harness
  per executor stays; Codex and Claude on one cluster means two executors.
- **The coordinator on the cluster, the laptop service optional.** One
  coordinator implementation serves both hosts. Two protocols keep fixed
  levels: the placement protocol carries launches and incarnations and only an
  owner speaks it; the owner protocol (`/work/*` and home's control socket)
  carries session IDs, operation UUIDs and intent. The CLI and Hermes send
  session and operation IDs, never launch details. Journals hold no host paths
  or laptop identifiers, name placements rather than address them, and store
  profiles by digest. Handing a running session's ownership from home to the
  executor, the mirror of `pull`, needs its own decision.

### 8. Interactive sessions owned by the executor (server side)

Added after this record was accepted; it follows its direction.

- **Mode is part of the intent.** `Submit.mode` is `task` (or empty) or
  `interactive`, and the intent digest covers it, so the executor never
  infers interactivity from a missing task. Only the home principal submits
  an interactive session, cancels one, or takes its owner actions; the work
  token gets `403`, and its MCP `sproozi_run` keeps the task-only schema.
- **The driver only starts an interactive session.** It captures the input,
  creates and activates the first visit, and stops: there is no completion
  driver, and an unsettled stop or cancellation waits for `recover`, which
  retries it once. The task's backoff applies to tasks only.
- **Owner actions bind operations.** The record gains `operations`, a map
  from operation UUID to its action (`stop` or `resume`) and execution, as
  home's catalogue has; `read` checks that each names one of the session's
  executions and that only an interactive session has more than one
  execution. `stop` is `Coordinator.Finalize` with the caller's operation on
  its bound execution. `resume` creates a new execution with `Session = ID`,
  from the last committed checkpoint (or the retained starting workspace
  after a turnless stop), the stored profile digest, the full allowance and a
  fresh deadline choice within the executor's cap; it refuses a session that
  is running, cancelled or transferred. Its execution ID is derived from the
  session and operation, so a resume interrupted between `Create` and the
  catalogue's save adopts the execution it created instead of starting
  another.
- **Attach names the session.** `/work/attach` reads a JSON envelope with
  only the session ID, then the placement protocol's terminal frames
  (`profile.NewFrameReader`, `EnvelopeLine`), and calls the in-process
  placement's `Attach` on the coordinator's active visit. The caller never
  sends a launch or incarnation.
- **An exit sweep** runs every 15 seconds, slower than home's 5 because each
  check reads a registration, Run and Pod from the API server. The Kubernetes
  placement gains `Exited`, and its stop of an ended native container reads
  the supervisor's exit report through `pods/log`, binds a reported Codex
  thread, or stops turnless. The sweep stops such a session with a
  name-based operation; it then shows `failed` with its exit, or `retained`.
- **An uploaded workspace** is a second input, decided by the owner. The
  executor validates it as a placement validates a moved workspace (archive
  entries, then a bounded restore that verifies Git objects and installs no
  configuration), keeps it privately as `work/<id>.workspace` until
  `Coordinator.Create` retains it, then deletes it. The intent records its
  digest, never its bytes.

## Alternatives

- **Keep work separate and fix it in place.** Check phase on read, take the
  lock before `Cancel` saves, handle `ErrNativeTurnPending`, end expired
  tasks, and split the two unknowns. Smallest, and each fix is local, but it
  keeps two lifecycles that must follow the same safety rules, which is what
  0018 removed at home. Every later rule (bounded stops, optional deadlines)
  would be done twice, and interactive sessions would need a third copy of
  stop, resume and attach. Rejected.
- **A third owner type** for interactive executor sessions. It leaves tasks
  untouched but adds a third catalogue and lifecycle that `pull` and
  `sessions` must understand. Rejected.
- **Always make the laptop the owner** (`run`, then `move --remote`). No new
  owner logic, but `attach`, `stop` and exit handling would need the laptop's
  service, so the laptop could not be optional, and ADR 0012 already rejected
  that for unattended dispatch. Kept for sessions started locally.
- **An AgentSession CRD.** ADR 0012 rejected it; nothing here changes that.
  Revisit with high availability.
- **Re-host only the driver and keep the stored phase.** Less change, but a
  stored phase can disagree with the journal. Rejected; the phase is derived.
- **Decide the cancel race in the work layer.** `admit` could refuse an idle
  execution, but it runs before the coordinator takes the execution's lock, so
  a stop committing in between would still be cancelled. Deciding under that
  lock needs the coordinator, hence the new event.
- **One `maxDuration` for everyone, or a second executor for Hermes.** One cap
  cannot limit Hermes without limiting the owner; a second executor doubles
  the deployment. Rejected for `workTokenMaxDuration`.
- **Pull or discard before upgrading, instead of a version 1 reader.**
  Simpler code, but it makes the upgrade lose retained work the operator has
  not pulled yet. Rejected.

## Consequences

- One lifecycle and its tests cover home and executor executions: the same
  phases, stop outcomes, cancellation order and recovery messages.
- **The public phase vocabulary for remote work changes.** `unknown` becomes
  `unknown-stop` or `unknown-usage`, `finalizing` becomes `stopping`,
  `pending` becomes `preparing`, and `activating` and `failed` can appear.
  `retained`, `exporting`, `pulled`, `cancelling` and `cancelled` stay. A
  stop still waiting for the task's turn is `stopping` with a problem. Hermes
  prompts and skills that match the old words need updating in the same
  release. The MCP tool schemas do not change.
- The executor keeps a second locked directory, `sessions/`, next to `work/`
  on the same volume, bounded by `retainedBytes`.
- Executor configuration with `workTokenFile` now needs
  `workTokenMaxDuration`; the executor names the field at startup.
- This replaces [ADR 0032](0032-stopping-a-visit-with-no-native-conversation.md)'s
  consequence that Kubernetes visits always carry a task or a checkpoint and
  so are never turnless: a Kubernetes first visit with no task now stops
  turnless as on Docker.
- It extends [ADR 0018](0018-coordinator-owns-every-visit.md) to the remote
  executor and closes its "remote-first work" remaining-work item.
- Not proven by unit tests: the executor on a real cluster after this change,
  which `make test-e2e` and the portable-workflows Kind walkthrough cover.

## Revisit when

- Several principals or homes need one executor, or one executor must serve
  several harnesses.
- An executor needs high availability; the locked directories and the
  in-process placement assume one replica.
- Ownership must move from home to the executor.
- A harness cannot report completion through its supervisor's graceful stop,
  so repeating `Finalize` is no longer enough to observe a task.
- Releases need in-place upgrades across minor versions; the legacy reader
  then becomes a versioned reader.

## Links

- Extends [ADR 0018](0018-coordinator-owns-every-visit.md); refines
  [ADR 0012](0012-retained-work-and-fresh-session-continuations.md): the
  catalogue keeps intent and transfer, the coordinator keeps execution state.
- Replaces the Kubernetes consequence of
  [ADR 0032](0032-stopping-a-visit-with-no-native-conversation.md).
- Adds the `cancel-unsettled` event to
  [ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md)'s table.
- Applies [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md) to work
  the executor runs for Hermes.
- Keeps [ADR 0019](0019-pull-operations-and-work-owner-names.md) and
  [ADR 0036](0036-remote-executors-through-kubeconfig-contexts.md) unchanged.
- Code: `internal/work/` (`catalogue.go`, `driver.go`, `interactive.go`, `export.go`,
  `legacy.go`, `phase.go`), `internal/session/cancel.go`,
  `internal/session/phase.go`, `cmd/session-profile/main.go`,
  `internal/runtime/kubernetes/placement/placement.go`,
  `cmd/sproozi/output.go`.
- Docs: [session commands](../reference/session-commands.md),
  [remote executors](../reference/remote-executors.md),
  [portable workflows](../guides/portable-workflows.md),
  [Hermes with remote work](../guides/retained-work-kubernetes.md) and the
  [executor README](../../cmd/session-profile/README.md).
