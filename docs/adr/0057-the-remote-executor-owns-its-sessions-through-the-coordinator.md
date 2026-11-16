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
| `active` | A task's completion wait (step 4 below) that the coordinator holds as `stopping`: the task's turn is still running. |
| `stopping` | A task's completion wait whose writer stopped, while the coordinator holds it as `preparing` and finishes it. |
| `preparing`, `activating`, `active`, `stopping`, `unknown-stop`, `unknown-usage`, `cancelling` | Otherwise, the coordinator's phase, reported as it is. |
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
   `ErrNativeTurnPending` the stop stays pending, the journal stays
   `stopping`, and the driver calls `Finalize` again. Defect 3 is gone.
   This stop is the driver's completion wait, not a stop anyone asked for,
   so the public phase reports it as `active`, with no problem, until the
   writer stops; then `stopping` until the task tail is written. Only the
   view says so: the journal's `stopping` is true (a finalization is
   pending, and repeating it continues it), and on the executor only the
   driver and the coordinator read it. A caller cannot stop a task, so
   every other stop or cancellation is reported as the journal holds it.
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
bind completion to the task's own prompt in the same way (section 11).

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
  completion bound to the task prompt and `FinalText` for Claude. Sections
  10 and 11 record both. One harness
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

### 9. The CLI for interactive sessions

Added with the CLI; it follows section 8.

- **The CLI talks to the executor directly**, over the port-forwarded owner
  transport `status`, `cancel` and `recover --remote` already use, not
  through the local service. The laptop keeps no record of the session, so
  any machine that reaches the cluster can attach, and no service needs to
  run.
- **`run --remote` without a task is interactive.** The intent says so in
  `mode`; the CLI never infers it on the executor's behalf. `PATH` is the
  local `run` operand: given, the CLI captures it with
  `workspace.CaptureLocal`, the local run's capture and refusals, within the
  executor's `maxCheckpointBytes`, and uploads it. Absent, the session starts
  from the approved repository; it never defaults to the current directory.
  `--repository` or `--project` is still required, for association.
- **Attach reuses the local terminal code** through `home.AttachStream`, the
  frame conversion with the transport factored out, and one
  `attachTerminal` in the CLI for raw mode, resizing and the detach line.
- **Stop repeats its operation** while the executor answers `stopping` or the
  request times out, printing the waiting notice once. Each resume is a new
  operation, since the executor binds operations to one action and execution.
  (Correction, 2026-11-16: the executor ran `stop` on the request, so a
  request that timed out after 45 seconds cut the stop rather than leaving it
  running. It now carries `stop` and `recover` on after the request ends, the
  CLI waits as long as a stop may take and prints the notice after 30
  seconds; see [ADR 0036's amendment](0036-remote-executors-through-kubeconfig-contexts.md#amendment-a-stop-outlives-its-request-2026-11-16).)
- **Commands for a cluster's session need `--remote`, `--context` or
  `--namespace`.** The cluster's catalogue does not record which context
  reaches it, and guessing from `sessions` would contact clusters for every
  local command. The local "no session" error names the flags instead.

### 10. Claude Code interactive sessions on the cluster

Added with Claude Code executors; it follows section 7.

- **An executor's harness is `codex` or `claude-code`.** A Claude Code
  executor first ran interactive sessions only and answered a task
  `unsupported`; section 11 turns its tasks on. One harness per executor stays, so the example
  adds a second executor in its own namespace, `sproozi-claude-sessions`,
  whose roles outside it are named `sproozi-claude-work`. The CLI names the
  executor's harness, from its description, and the requested one when it
  refuses.
- **A first visit starts Claude Code fresh**, as a local Docker session
  does: the supervisor writes onboarding as complete and the workspace as
  trusted in the launch-generated `.claude.json`, so nothing waits at a
  dialog. The placement binds the conversation Claude Code reported through
  its hooks during the graceful stop (`BindNative`), as for a moved
  checkpoint's later visit.
- **An exited Claude Code visit stops.** The exit sweep stops a session
  whose agent exited, and the Kubernetes placement refused every stop of a
  Claude Code visit whose container had ended, so `/exit` would have left
  the session `unknown-stop`. The placement now follows
  [ADR 0038](0038-exited-agents-and-one-recovery-path.md) as Docker does: a
  first visit stops turnless, keeping the workspace it started from, and a
  resumed visit's stop names the checkpoint it started from. Neither claims a
  conversation from a stopped container (ADR 0011).

### 11. Claude tasks on the cluster

Added once the Claude Code supervisor ran tasks (ADR 0011, later
clarification). A Claude Code executor now accepts a task as a Codex one
does; it answers `unsupported` only for another harness's submission, and the
work token reaches it under the same `workTokenMaxDuration`.

- **Completion binds to the task's prompt.** The supervisor passes the task
  as the launch's one positional prompt, and counts only the `Stop` of the
  turn whose `UserPromptSubmit` prompt is the task. Until then the graceful
  stop reports the turn pending and the driver repeats `Finalize`, as for
  Codex. A task launch never resumes a conversation and is never turnless.
- **The result is Claude's final reply**: the text of the reply at the
  transcript leaf the checkpoint selected, bounded like Codex's, with Claude
  Code's `(no content)` read as empty.
- **Nothing else changes in the owner.** The same phases, deadlines,
  retention and `pull` apply. The CLI's refusal of a task for the executor's
  own harness now says an executor older than this release may run none.
- **Not yet proven on a cluster.** Unit tests cover the owner with a fake
  placement and the supervisor with captured hooks;
  `make verify-claude-remote-kind` is the Kind proof and has not been
  recorded.

### 12. Retention and removal

Added after a review found that the executor's catalogue only grew. Nothing
removed a session, so once it held 1024 records or `retainedBytes` was
reached every submission failed as `unavailable` ("restore connectivity"),
and the only remedy was deleting files on the volume. `retainedBytes`
(section 4) was checked by a submission only: a `resume` and an interactive
session's stops added checkpoints unchecked.

- **The retention invariant.** Only admission checks the bound, and
  admission reserves what the admitted execution may still come to keep, at
  most `maxCheckpointBytes` each: a submission its input workspace (snapshot,
  revision or upload, which the coordinator retains when it starts) and its
  checkpoint; a resume its checkpoint, since its input is the session's own
  checkpoint or workspace, which the coordinator keeps once by digest. An
  admission is refused unless what is kept, plus the reservation of every
  session not yet settled (two before the coordinator has its execution, one
  after), plus its own, fits. A stop, a checkpoint commit, a cancellation and
  a task's tail never check the bound: they write what was reserved, and
  refusing them would lose work. Records and journals are bounded metadata
  and are not reserved. Section 4 reserved one checkpoint for a submission,
  so its input was never covered; it now reserves two.
- **What the bound does not cover.** Removal marks (below) are not counted.
  Scratch space is not reserved: an upload's validation before admission, and
  a task's capture of a revision or an empty workspace after it, each write
  up to `maxCheckpointBytes` under `work/` and delete it when done. A
  concurrent admission counts that scratch as kept, so it only refuses work
  sooner, but the volume may briefly hold that much beyond `retainedBytes`.
  Admitting before validating an upload would reserve room for input that may
  be refused, and capture runs in the driver after admission by design, so
  both stay as they are.
- **The smallest bound.** At `retainedBytes` equal to twice
  `maxCheckpointBytes`, the smallest allowed, a submission is admitted only
  when the executor keeps nothing at all, so it keeps one session. A resume
  there is admitted only when that session's workspace, checkpoint, journal
  and record fit in one `maxCheckpointBytes`, so a large session can be
  removed but never resumed. The minimum is unchanged, since raising it would
  stop executors configured at it from starting; the owner decides.
- **A full executor says so.** The owner protocol gains `sessions_full` and
  `retention_full`, both HTTP 507, whose fixed messages say which bound was
  reached and name `sproozi rm ID --remote` and `retainedBytes`. A session at
  its cap of 256 executions or 1024 bound operations answers
  `executions_full` or `operations_full`, also 507, where it answered
  `unavailable`. The CLI prints them without a retry hint, and Hermes reads
  the same text from `sproozi_run`. The placement protocol does not use them.
- **The owner action `remove`**, home principal only, behind
  `sproozi rm ID --remote`. It applies home's rule, `session.Removable` on
  every execution through `Coordinator.Remove`, which also deletes the
  journals and the content no remaining journal names, plus the catalogue's
  own: `exporting` is refused, since its claim is still to come, and so is a
  session that an unstarted session starts from (`workspaceFrom`), whose
  start reads its checkpoint; removal and submission are serialized under the
  retention lock so one cannot slip between the other's check and save. A
  `retained`, `failed`, `cancelled` or `pulled` session is removed; a version
  1 record goes with its `.checkpoint` file. The owner protocol carries codes
  only, so the executor answers `conflict` and the CLI explains it from the
  session's phase in local `rm`'s words, which `session.RemovalRefusal` holds
  for both; a running task is told to wait or cancel, since it takes no stop.
- **Removal leaves a mark**, `work/<id>.removed`. The mark makes `remove`
  replay-safe without an operation UUID, since a session has only one
  removal: repeating it answers removed. It also keeps the ID from starting
  again: a submission that repeats the removed session's `requestId`, such as
  a late retry from Hermes, answers `conflict` instead of running the task
  twice or, for a pulled session, creating a second owner of an ID home owns.
  Marks count towards neither the 1024 records nor `retainedBytes`, so an
  executor at the smallest bound can admit again after a removal. The record
  format stays version 2.
- **Removal survives a crash.** The mark is written durably first and lists
  the session's executions; then the journals go (the coordinator syncs its
  directory), then the record and any kept upload (the catalogue syncs its
  directory), and last the mark shrinks to the session ID. `Open` finishes
  every removal whose mark still lists executions, or whose record is still
  there, before it starts any session, so a record whose journals are gone is
  never started again and no unlisted journal keeps counting towards the
  bound.
- **A pulled record is removed only once home holds the pull.** The record is
  the one-way claim's tombstone: it keeps the ID from running here again,
  which the mark now does, and it answers a repeated `claim` for a home whose
  claim reply was lost. Home saves the import before it claims and records
  the claim after the reply, so only that repeat needs the record, and the
  executor cannot see whether home recorded it. So `remove` of a pulled
  session needs `pullFinished`, which the CLI sends only after this machine's
  service reports the session as its own (ownership `home`); with the service
  stopped, the session unknown here or its pull unfinished, `rm --remote`
  refuses and says to start the service, that this machine does not hold the
  session, or to finish with `sproozi pull ID`. The executor refuses a
  request without it, so no other CLI path skips the check. **Limitation:** a
  pulled session can be removed from the cluster only while the machine that
  pulled it still holds it, so the reference says to remove the cluster's
  record before the local one; once the local copy is gone the cluster's
  record stays. Whether an override is wanted is the owner's decision.
- **A removal `Open` cannot finish does not stop the executor.** `Open`
  logs it and starts; `remove` retries it. A session with a mark, finished or
  not, is left out of the catalogue's listing and reads as missing to every
  action and driver, so it is never listed, driven or started again. A
  record that cannot be read refuses every removal, since it might start
  from the session being removed.
- **Removal does not wait for other sessions.** `Coordinator.Remove` took
  the coordinator's lock exclusively. That lock kept its clean-up of
  content stored once by digest (workspaces, checkpoints, profiles) from
  deleting content an operation had written but not yet named in its saved
  journal; it also kept journals from being deleted under an operation. But
  every operation holds the coordinator's lock shared for its whole run,
  including a graceful stop that waits up to two minutes for a native turn,
  and a running task's driver repeats that stop at once, so removal answered
  busy whenever any task ran: exactly when `retention_full` sends the user to
  remove. Now removal takes only the removed executions' own locks, in ID
  order, waiting at most five seconds for an operation on one of them. Each
  operation records the content it writes, under a short shared content
  lock, and forgets it when it releases its execution, after the journal
  that names the content is saved or not; the clean-up takes the content
  lock exclusively and treats recorded content as named. The content lock is
  never held across a placement call. An execution's lock and the
  coordinator's shared lock still are: `Finalize`, `Recover` and
  cancellation hold them across the native-turn wait, which is why removal
  waits up to five seconds for an operation on an execution it removes, and
  answers busy after that; operations on other executions no longer matter.
  The order is the catalogue's session lock,
  then the retention lock, then the coordinator's shared lock, then an
  execution's lock, then the content lock; `CancelUnsettled` still runs
  `admit`, which takes the catalogue's session lock, before it takes the
  execution's lock (ADR 0018), so the order holds. Home's `rm` uses the same
  `Coordinator.Remove` and gains the same behaviour.

Alternatives rejected:

- **Release the coordinator's shared lock during the native stop wait.**
  Smaller, but the lock then no longer means "no operation is in progress",
  and every placement call that waits would need the same treatment, now and
  in each future placement.
- **Defer clean-up until the executor is idle.** Capacity would not be freed
  while tasks run, which is when it is needed.
- **Allow removing a pulled record on the user's word**, as the first draft
  of this section did with a sentence in the reference. A lost claim reply
  then strands home's import with nothing to activate it.
- **Bind `remove` to an operation UUID**, as `stop` and `resume` are. The
  binding would have to outlive the record it binds to; the mark already
  answers a repeat, and a session has one removal, so an operation adds no
  distinction.
- **Remove without a mark.** Simpler, and nothing grows, but a repeated
  submission would start a removed task again and a repeated removal could
  not tell "removed" from "never existed".
- **Check `retainedBytes` at every commit**, including stops. A full volume
  would then refuse a stop and lose the agent's work; the reservation keeps
  the bound without that.
- **Keep reporting `unavailable`.** It tells the user to restore
  connectivity and retry with the same operation, which never helps.

Revisit when marks become numerous enough to matter (each finished one is
about 50 bytes), when home can confirm a claim to the executor so a pulled
record can be removed without asking home's service, when the smallest bound
should hold more than one session, or when the executor's catalogue merges
with home's.

### 13. Aborting a Kubernetes preparation that has no Pod (2026-11-15)

Added after a review of states that only a deadline used to end, under the
principle recorded with the journal's exit for late preparations.

**Problem.** Cancelling an execution whose pending visit never reported an
incarnation asks its placement to fence the preparation
(`session.PreparationAborter`), or else inspects it and needs an
incarnation to `Abort`. The Kubernetes placement had no `AbortPreparation`,
and its `Inspect` of a `creating` or `bound` registration has no Pod, so
there was nothing to abort. A visit whose AgentRun waited under a
concurrency cap ([ADR 0027](0027-configurable-concurrent-admission.md)),
was admitted with no Pod yet, or failed before its Pod stayed
`unknown-stop` and kept its registration. Before ADR 0056 the deadline ended
it; work with no deadline kept it for ever. The same gap existed for a
home-owned visit on a cluster, since the placement protocol had no such
action.

**Decision.** The Kubernetes placement implements `AbortPreparation`.
Success means no writer for the launch exists and none can start, which is
what the coordinator records as `cancelled` with no receipt. The steps, each
idempotent so a retry after a crash finishes the same abort, and what each
proves:

0. **Only a preparation is aborted.** Success reports no usage, so a
   registration that may have had gateway authority is a conflict:
   `active`, `stopping`, `stopped`, or `cancelled` with a receipt that
   sealed any usage, such as `Abort` leaves for an active visit. A
   `cancelled` record is accepted only as this abort's own replay or with a
   receipt whose every seal is zero, and the abort checks its own seal the
   same way. `creating`, `bound` and `prepared` never had authority, and
   once the tombstone (step 1) is written nothing activates the visit, so a
   replay never meets one that ran in between. A registration that is gone
   is also a conflict, on both lookup paths: nothing in Sproozi deletes
   one, so an administrator did, and its run and Pod may remain with
   nothing to fence them by. Found by run name, a gone registration counts
   as never created only when no run exists at that name, because the
   executor creates the run after the registration. That leaves one
   accepted gap: if the executor crashed between creating the registration
   and saving its reference, and an administrator then deleted both the
   registration and the run, the run's Pod can remain, `Terminating` under
   its writer-fence finalizer, and the abort reports success without
   fencing it. Closing that needs a list of Pods by the run-name label
   (`sproozi.com/agentrun`) in `sproozi-agents`. The executor's Role there
   grants `get`, `patch` and `delete` on Pods but not `list`, and this
   change adds no RBAC.
1. **A local tombstone** (`preparationAborted` in the executor's record of
   the launch, written before anything in the cluster changes, even when
   the executor never saw the launch) makes every later `Prepare` of the
   launch a conflict and stops the placement creating its registration.
   `Activate`, `Stop`, `Abort`, `Attach` and `Inspect` of a tombstoned
   launch are conflicts too, whatever reference it holds. A
   launch the executor never saw also claims its session epoch, as
   `Prepare` does, so no other allocation prepares that epoch afterwards.
   Within the executor, `Prepare` and `AbortPreparation` hold the same lock,
   so none is in flight.
2. **The registration is cancelled by compare-and-swap.** A registration
   lost between its creation and the executor's save is found by its run
   name. Once `cancelled` it is never bound, prepared or activated, and the
   gateway refuses its run (`ResolveRun` answers `ErrSessionCancelled`).
3. **A run never bound to the registration never gets a Pod.** Every version
   of the registration names no run, so the controller resolves any run of
   that name as pending or cancelled and never admits it. The executor still
   sets `spec.cancel` on it, and `ResolveRun` now reports such a registration
   as `SessionCancelled` rather than a rejected binding.
4. **A bound run is cancelled, then the executor waits for the controller.**
   The `spec.cancel` patch is optimistic, so a controller status write in
   between conflicts; the executor reads the run again and retries a few
   times. A reconcile that read the registration before step 2 may still create the
   Pod, so an absent Pod is not yet proof. The executor waits until a live
   read shows the run gone, terminal, or with its sandbox name recorded,
   and only then reads the Pod. The controller now creates a sandbox Pod
   only after a live read shows the run neither cancelled, terminal nor
   deleted. Reconciles of one run never overlap. So a reconcile that could
   still create the Pod either started after the run ended, and its live
   read refuses, or finished before the executor's observation, and its Pod
   already exists.
5. **A Pod that exists is fenced as `Abort` fences one**
   ([ADR 0010](0010-managed-session-authority-and-admission.md)). The
   registration records it (`BindPod` now accepts a cancelled record with a
   bound run and no Pod, see [ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md)).
   The Pod is deleted by UID, and the writer is fenced on an observed
   real termination of the native container, never on
   `ContainerStatusUnknown`. Accounts are sealed when they exist, then the
   writer-fence finalizer is released. One new form of evidence: a Pod that
   is being deleted, has no node and never started a container is fenced,
   because the API server refuses to bind a Pod that is being deleted.

Anything the executor cannot prove is an error, which the coordinator
records as `unknown-stop` and `recover` retries: the controller has not
ended the run yet, or a Pod's container was never seen to stop. One call
spends at most 30 seconds (`abortPreparationWait`) after it takes the
launch's lock. That is under the home's 45-second request timeout
(`profile.Connect`), so a home normally receives the executor's
`unavailable` answer rather than its own timeout. Two cases fall outside
that:
- The bound starts after the lock, so time spent waiting for another
  operation on the same launch adds to it. Lock wait plus 30 seconds can
  then exceed the home's 45, and the home sees its own timeout. The next
  `recover` repeats the same request.
- The bound does not cover a Pod's termination grace. It starts before the
  Pod is deleted, and a container that uses its whole grace, by default 30
  seconds from the deletion, is killed and reported after the bound has
  run out. A template with a longer `terminationGracePeriodSeconds` always
  takes more than one attempt. The next `recover` finds the tombstone, the
  reference and the cancelled record, deletes the Pod again
  (idempotently) and observes the termination. A healthy cancellation
  finishes in seconds, because the supervisor handles `SIGTERM`.

The bound applies to this action only.

**Protocol.** `/v1/abort-preparation` takes the launch alone, is authorized
like the other placement actions
([ADR 0036](0036-remote-executors-through-kubeconfig-contexts.md)) and is
never refused after the deadline. The home-side client implements
`PreparationAborter` always. An executor from before this action answers
`404 invalid_request`, and a placement that cannot abort answers
`unsupported`. In both cases the client does what the coordinator did
before: an absent visit is fenced, a prepared or already stopped one is
aborted, and one still preparing is an error that says the executor is
older. An active visit, or one in an unknown state, as a stopping
Kubernetes visit inspects, is refused before any abort, for the reason in
step 0. So is an abort whose receipt sealed any usage, since a visit that
inspects as prepared or stopped may still have run. An older home never
sends the action. See [remote executors](../reference/remote-executors.md#cancelling-a-preparation).

**Alternatives rejected.**

- *Treat an absent Pod as proof at once.* Simple, but a reconcile in flight
  can create the Pod a moment later; that is the race step 4 closes.
- *Delete the AgentRun as the fence.* Deletion runs through the
  controller's finalizer and release, so it needs the same wait. It also
  loses the run's truthful terminal reason.
- *Trust a late Pod as inert*, because its harness waits for activation and
  the gateway refuses its run. Probably true, but ADR 0010 asks for physical
  evidence of every Pod a cancellation leaves behind, not an argument about
  what the Pod may do.
- *Advertise the action in `describe`.* An older home decodes the
  description strictly and would refuse the new field, so this would break
  older homes. Detecting `404` on the action breaks no one.

**Tradeoffs.** A cancellation of a bound run without a Pod now waits for the
controller to reconcile the run, normally under a second. With the
controller down it stays `unknown-stop` until `recover`. The older-executor
fallback reports a prepared visit whose usage could not be sealed as
`unknown-stop` rather than `unknown-usage`, since `AbortPreparation` has no
usage outcome; `recover` finishes it either way. An executor from before
this change cannot read a launch record that has the tombstone. That
matters only on a downgrade, and only for aborted launches. A visit
cancelled by an older executor's `Abort` whose usage was never sealed is
refused by step 0 after an upgrade, and stays `unknown-stop`.

**Accepted limit: leader election is a lease, not a fence.** Step 4 relies
on reconciles of one run never overlapping. That holds within one
controller process. Across a leader change it does not: a leader that
passed its live read and then froze for longer than its lease could create
the Pod after a new leader had ended the run and the executor had found no
Pod. Such a Pod has no gateway authority, since the registration is
cancelled. Its writer-fence finalizer holds it until the executor or an
administrator removes it. `Abort` has the same hazard today.

**Revisit when** the controller can create a Pod without that lease
premise, for example from more than one active replica, or when a
cluster-side admission rule can refuse a Pod for a cancelled registration,
which would close the limit and spare the executor its wait; or when
`PreparationAborter` gains a usage outcome.

Code: `internal/runtime/kubernetes/placement/abort_preparation.go`,
`lifecycle.go` (`fenceCancelled`), `internal/runtime/kubernetes/registry`
(`Lookup`, `BindPod`, `ResolveRun`), `internal/controller/execution.go`
(`ensureSandbox`), `internal/session/profile` (`client.go`, `server.go`) and
`internal/session/remote/client.go`. Tests:
`internal/runtime/kubernetes/placement/abort_preparation_test.go`, including
the coordinator's cancel of a queued destination, which reached
`unknown-stop` before this change; `internal/session/profile/abort_preparation_test.go`;
`TestReconcilerStaleCacheDoesNotCreateSandboxForEndedRun` and
`TestReconcilerUnboundCancelledManagedSessionEndsAsCancelled`. A reconcile
that is past its live read when `spec.cancel` lands is simulated by a patch
hook in the placement tests, not run against the real controller: envtest
has no kubelet to report the late Pod's termination, and pausing a real
reconcile at that point needs a hook in the controller that only a test
would use.

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
- **A completion wait in the coordinator** that is not a stop: an event or
  placement observation that keeps the journal `active` until the task's
  turn completes, then stops. The journal would then say what the view says,
  but it needs a new row in ADR 0058's table, a placement call that waits
  without gating input, and a second path to the same graceful stop. The
  driver's `Finalize` already waits correctly, and the journal's `stopping`
  is true of it. Rejected for deriving the public phase; revisit if a caller
  must stop a task, or a reader other than the driver and coordinator acts
  on a task's journal phase.
- **One `maxDuration` for everyone, or a second executor for Hermes.** One cap
  cannot limit Hermes without limiting the owner; a second executor doubles
  the deployment. Rejected for `workTokenMaxDuration`.
- **Route remote commands through the local service**, as `move` and `pull`
  do. The service would remember the executor, so the flags could be
  dropped, but the laptop's service would have to run for every attach.
  Rejected for interactive sessions; revisit if remembering the executor
  matters more than the laptop being optional.
- **Resolve a session ID to its cluster without flags**, by asking the
  default cluster when this machine does not know the ID. It covers only the
  `sproozi` context, and makes every unknown local ID reach a cluster.
  Rejected for now.
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
  task whose turn is running is `active` with no problem, and `stopping` once
  its turn completed, so `stopping` always means a stop in progress. Hermes
  prompts and skills that match the old words need updating in the same
  release. The MCP tool schemas do not change.
- The executor keeps a second locked directory, `sessions/`, next to `work/`
  on the same volume, bounded by `retainedBytes` with the exceptions section
  12 states: removal marks, and transient scratch space.
- **Upgrade note (section 12).** A resume is now admission-checked, where it
  was not, and a submission reserves two checkpoints instead of one, so an
  executor near its bound can refuse work the previous release accepted. For
  example, with the largest checkpoint limit, 64 MiB, and the default 1 GiB
  bound, an empty executor admits eight concurrent unstarted submissions
  instead of sixteen.
  Make room with `sproozi rm ID --remote` or raise `retainedBytes`.
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
  `legacy.go`, `phase.go`, `remove.go`), `internal/session/cancel.go`, `internal/session/remove.go`,
  `internal/session/phase.go`, `cmd/session-profile/main.go`,
  `internal/runtime/kubernetes/placement/placement.go` and `lifecycle.go`,
  `cmd/sproozi/output.go` and `session.go`; the Claude Code executor
  example in `examples/portable-workflows/kubernetes/claude/`.
- Docs: [session commands](../reference/session-commands.md),
  [remote executors](../reference/remote-executors.md),
  [portable workflows](../guides/portable-workflows.md),
  [Hermes with remote work](../guides/retained-work-kubernetes.md) and the
  [executor README](../../cmd/session-profile/README.md).

## Later correction (2026-11-15)

"A deadline that passes before activation is final" now holds for tasks, and
for sessions the coordinator never received. An interactive session's
execution that is still starting when its deadline passes is recovered
instead. The coordinator ends that start, and the session is `retained` with
the problem `the deadline passed before it started`, keeping the checkpoint
its earlier execution committed. `resume --remote` continues it. See
[ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md).
