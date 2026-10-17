# 0018: The coordinator owns every visit of an execution

Status: accepted
Date: 2026-10-17

## Problem and constraints

A managed execution is a sequence of visits: a placement runs one writer until
it stops, then the next visit may start elsewhere. ADRs
[0004](0004-portable-sessions-and-bounded-executions.md),
[0009](0009-runtime-backends-and-sealed-accounting.md) and
[0010](0010-managed-session-authority-and-admission.md) require that a new
writer starts only after the previous one is proven stopped and its usage
sealed, and that unknown stop or usage keeps the allowance held.

The home coordinator enforced this for moves. The first visit did not go
through it. The home service ran that visit through its own seven-stage
journal, lock table and graceful-operation control, and imported the visit into
the coordinator only after stopping it for a first move. ADR
[0012](0012-retained-work-and-fresh-session-continuations.md) reused the same
journal for a resumed execution's first visit. As a result, stop, cancel and
recovery each had two implementations, cancellation interrupted graceful work
twice, and recovery had to decide which state machine held a session.

The first visit also changed its launch. A first visit from a Git workspace has
no native conversation ID until the client creates one. Docker and Kubernetes
placements then wrote that ID into the retained launch, and each layer kept a
copy of the original launch to accept either form on replay. The `Launch` type
was documented as immutable but was not.

## Decision

The coordinator owns every visit, including the first. `Coordinator.Create`
retains a new execution whose first visit has not started. Its input is a
captured Git workspace for a new conversation, or a committed checkpoint for a
resumed one. `Recover` prepares and activates that visit as it would any move
destination, and stop, move, cancel and recovery then use a single path. The
home service keeps workspace capture, retention limits, Project approval and the
catalogue of durable sessions. It holds a lock per catalogued session but no
execution journal, execution lock or graceful control. `Cancel` takes an
`admit` callback, so the coordinator interrupts graceful work before the home
service waits for the session lock.

A launch never changes after it is created. The genuine native conversation ID
of a first visit is recorded separately: `Stopped.ThreadID` in the placement
contract, `Record.ThreadID` once the first stop commits a checkpoint, `Thread`
in the Docker placement and Docker gateway records, and the registry's
`NativeUUID` for Kubernetes. A replayed request that has the ID written into
its launch is now a conflict. `session.SameLaunch` is the only launch-equality
function.

The same review made three related contracts explicit:

- `Placement.Stop` has three outcomes. Success fences the writer and seals its
  usage. An error wrapping `ErrUsageUnsealed` means the writer is fenced but its
  usage is unknown. Any other error means the stop itself is unknown. The
  coordinator records the second case as unknown usage, not unknown stop.
- `Record.Phase` is validated against the journal structure on every save and
  load: the active visit, its fence, the pending operation and the cancellation
  intent. Cancellation reports unknown stop or usage through the same phases as
  a move.
- Docker and Kubernetes share one graceful handoff, and the native control
  operations and states are defined once in `internal/harness`.

## Alternatives

- **Keep a home-owned first visit and share helpers.** This keeps the import
  step, so stop, cancel and recovery still have two state machines that must
  agree on the same safety rules. Shared helpers would reduce duplicated code
  but would not remove the parallel journal.
- **Synthesize a stopped visit at start.** Creating the coordinator record from
  a fabricated stopped visit avoids a new start state. It needs either a
  synthetic receipt or a native ID before the client creates one, and both
  break existing rules.
- **Bind the native ID into the launch, as before.** This avoids an extra field,
  but every layer must then compare two launch forms on replay, and an
  immutable-intent check cannot detect a changed launch.
- **Remove all catalogue locking from home.** Pull, resume and product-level
  operation bindings still update catalogue records that the coordinator does
  not own, so the catalogue lock remains.

## Tradeoffs and consequences

- Journals change format. The coordinator journal, Docker placement record and
  Docker gateway record are now version 2. Kubernetes placement records no
  longer have an `initial` field. Home no longer reads or creates the `initial`
  directory. Sproozi is an unreleased preview with no tags, so earlier records
  are not migrated. A session started before this change must be cancelled or
  discarded by the operator.
- A move or stop on an execution whose first visit has not activated returns
  "requires recovery" instead of starting that visit implicitly. Run `recover`,
  or retry `start` or `resume` with the same operation.
- A `start` retry is matched by session ID, operation ID and ignored-file
  selection. The workspace path is not retained, so a retry with a different
  path is no longer rejected. The first capture remains authoritative.
- `Coordinator.Inspect` reads the latest atomically written journal without
  waiting for an operation in progress. Status and cancellation therefore stay
  responsive while a move waits for a native turn.
- Remote work exports still name the native ID in the exported launch, because
  `work.Export` and home pull validation define that wire format. The work
  owner adds it when it exports a stopped first visit. The Kubernetes
  placement's own launch is unchanged.
- Status keeps `initialStage` as an alias of `phase` for existing clients.

## Remaining work

- `session.JournalLocks` is a generic keyed mutex in the domain package. Work,
  Docker and Kubernetes also use it. A later change should move it to a private
  storage package.
- The exported-continuation format (`work.Export.Stopped.Launch.ThreadID`)
  could carry the native ID separately, as `Stopped.ThreadID` does. That change
  affects home pull validation, which this change does not modify.
- Remote-first work still runs its first visit through the work owner, not the
  home coordinator. ADR 0012 places that work on a laptop-independent owner.

## Revisit when

- Sproozi publishes a release. Journal format changes then need explicit
  migration or versioned readers.
- A placement cannot report a first visit's native ID through `Stopped.ThreadID`,
  or a harness needs its launch changed after creation.
- Status reads need a consistent view across several journals, which the
  lock-free `Inspect` does not provide.
- Adopting existing native sessions ([ADR 0005](0005-controlled-docker-launch-before-session-adoption.md))
  needs a first visit that the coordinator does not prepare.

## Evidence

Implementation: [coordinator](../../internal/session/coordinator.go),
[cancellation](../../internal/session/cancel.go),
[journal rules](../../internal/session/rules.go),
[home service](../../internal/session/home/service.go),
[Docker placement](../../internal/runtime/docker/placement.go),
[Kubernetes placement](../../internal/runtime/kubernetes/placement/lifecycle.go)
and [work owner](../../internal/work/owner.go). Tests in
`internal/session`, `internal/session/home`, `internal/runtime/docker`,
`internal/runtime/kubernetes/placement` and `internal/work` cover these cases:
first visits from a workspace binding the genuine ID at their first stop,
rejection of a launch rewritten with that ID, cancellation of unreported and
rejected first activations without inspection, and responsiveness while a first
move is held. They also cover the three stop outcomes on both placements and
the profile transport. Live Docker and Kind round trips were not rerun for this
change.
