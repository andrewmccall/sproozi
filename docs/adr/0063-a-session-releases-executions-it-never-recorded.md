# 0063: A session releases the executions it never recorded

Status: accepted
Date: 2026-11-16

## Problem and constraints

A home session's catalogue entry lists its executions and names the current
one. `resume` first retains a new execution in the execution journal, which
records the session it belongs to, then records it in the entry, and only
then prepares it. A resume interrupted between the two steps, for example
because the catalogue was full or the service stopped, leaves an execution
the entry does not list:

- commands on its ID were refused, because the session did not name it as
  current;
- `sproozi sessions` did not show it, because it belongs to a session;
- `rm` of the session removed only the executions the entry lists, so the
  journal outlived the session;
- it held one of the service's 256 execution slots for ever.

Constraints: the journal's format and transitions table
([ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md)) and
the catalogue's stored format stay as they are. Nothing may start work,
refund usage or cancel an execution the session recorded. Reads, such as
`status` and `sessions`, take no lock, so they must not write.

## Decision

An execution belongs to a session but is **unrecorded** when its journal
names the session and the session's entry neither lists it nor names it as
current. `releaseOrphans` in `internal/session/home/reconcile.go` handles
them, under the session's lock, which every command that creates an
execution for the session also holds:

- **When.** At the session's next `resume`, which logs a failure and goes on,
  and its next `rm`, which refuses on a failure so that no execution
  outlives its session. Reads do not release anything.
- **One that never ran**, with no visit and no writer, is cancelled through
  the execution journal and its journal is deleted. Its input is the
  checkpoint or workspace the session already keeps, so nothing is lost. A
  cancellation an earlier attempt recorded is repeated with its own
  operation, so an interrupted clean-up finishes on the next attempt.
- **One that ran** is kept, because it may hold work. No command of this
  release creates one. The session's `rm` refuses with a reply naming it;
  `cancel` of its ID stops it without touching the session's entry, and
  `rm` of its ID deletes it once settled. Both hold the session's lock and
  check under it that the session still does not record the execution, so
  neither takes one a resume is about to record. The session's `rm` then
  works.
- **A journal that cannot be read**, such as one a newer release wrote,
  cannot be shown to belong to the session. It is kept and logged once, so
  it never blocks another session's `rm`.

## Alternatives

- **Adopt it as the session's current execution.** This finishes the
  interrupted resume. It is rejected because a later resume may already have
  run and stopped: the unrecorded execution would then restore an older
  checkpoint than the one the session keeps. It would also turn a session
  the user sees as stopped into one that is starting.
- **Adopt it into the entry's list, not as current.** This makes it
  reachable, but it is still `preparing`, so `rm` of the session would refuse
  it, and it would still need cancelling to free its slot.
- **Sweep at start-up, or in the five-second exit sweep.** This frees slots
  without a command, but it needs every session's lock, or a try-lock that
  skips busy sessions, and would read every journal and call placements in
  the background, every five seconds for the sweep.
- **Release only on `rm`.** Smaller, but a session that is resumed again and
  again never frees the slot.
- **Record the execution in the entry before retaining it.** This removes
  the gap, but the entry would then name executions that may not exist, and
  loading it checks each operation's execution against the entry's list.

## Consequences

- An unrecorded execution counts towards the 256 executions until its
  session's next `resume` or `rm`.
- `resume` and `rm` list and read every journal the service keeps, at most
  256.
- Cancelling an execution that was never prepared calls the placement's
  `AbortPreparation`. Docker's returns at once when it has no record of the
  launch. This is not yet proven against a real Docker engine.
- `cancel` and `rm` accept the ID of an unrecorded execution; every other
  command on it but `status` is refused as before.
- An unreadable journal that is in fact the session's own unrecorded
  execution is not attributed to it: the session's `rm` succeeds and leaves
  the journal holding a slot, and `status` and `rm` of its ID cannot read it.
  Reading only its session field leniently would let the session's `rm`
  name it, but this release still could not cancel or delete it, so the user
  could not act on the refusal either. The log line names the file instead,
  for the release that wrote it or for the user to delete once nothing runs
  from it. It needs a newer release's journal, so only a downgrade reaches
  it.

## Revisit conditions

- `resume` records an execution in the entry before retaining it, which
  leaves nothing to release.
- A command or recovery path can leave an unrecorded execution that ran.
- The service gains an owner that sweeps sessions under their locks.
- The number of retained executions grows enough that reading every journal
  on `resume` and `rm` costs too much.

## Links

- Implementation: `internal/session/home/reconcile.go`,
  `internal/session/home/catalog.go` (`ResumeWithMaxDuration`,
  `resolveSession`), `internal/session/home/remove.go`
- Tests: `internal/session/home/reconcile_test.go`,
  `internal/session/home/orphan_test.go`
- Reference: [stopping, removing and recovering](../reference/session-commands.md#stopping-removing-and-recovering)
- Related: [ADR 0034](0034-resume-continues-every-home-session.md),
  [ADR 0037](0037-one-command-surface.md),
  [ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md)
