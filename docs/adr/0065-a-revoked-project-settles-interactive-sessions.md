# 0065: A revoked project settles interactive sessions

Status: accepted
Date: 2026-11-17

## Problem and constraints

[ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
section 8, decided that the remote executor's driver only starts an
interactive session: its owner stops it, and an unsettled stop or
cancellation waits for `recover`. Section 2 decided that a revoked project
is cancelled through the coordinator, whether found at startup or while
driving, but the driver applied that to an interactive session only while
it was starting or active.

An interactive session whose project no longer admits this installation,
and whose execution is `stopping`, `unknown-stop`, `unknown-usage` or
`cancelling`, therefore never settled:

- the driver left it for `recover`;
- every owner action on it, `recover`, `cancel`, `stop` and `status`
  included, is refused as denied, because the executor checks the
  session's project before the action;
- in `unknown-stop` a writer may still be running, and in every one of
  these phases the execution keeps its allocation encumbered.

The executor reads projects and their members from its configuration when
it starts, and does not reload them. A revocation therefore takes effect
only when the executor restarts. The stuck state arises when an executor
restarts without this installation in a project while an owner's stop of
one of its interactive sessions is waiting for the turn, or its stop or
cancellation is unproven.

## Decision

Once an interactive session's project is revoked, the driver settles it as
it settles a task's (ADR 0057, section 2):

- a cancellation the coordinator already holds is carried out, by recovery
  with the task's backoff, and is never replaced;
- otherwise the session is cancelled through the coordinator with the
  name-based `project-revoked` operation, recording the problem "its
  project no longer admits this installation".

An interactive session whose project still admits this installation waits
for `recover` exactly as section 8 decided. The driver still never asks for
an interactive session's completion and never writes a task's tail.

The rows are in `decisions` in `internal/work/decision.go`.

## Alternatives

- **Leave it waiting for `recover`.** This keeps section 8 unchanged, but
  `recover` is denied once the project is revoked, so nothing settles the
  session, a writer may keep running, and the allocation stays held.
- **Let a revoked owner still `recover` or `stop` its own session.** This
  keeps the owner in control of how the session ends, and could keep the
  graceful stop's checkpoint. It also lets an installation the project no
  longer admits act on that project's sessions, which revocation exists to
  prevent, and it still leaves the session stuck if the owner never acts.
- **Settle only `unknown-stop` and `unknown-usage`, and leave `stopping`.**
  A pending stop would keep waiting for the turn and might keep its
  checkpoint. But a `stopping` session is still a running writer that its
  owner can no longer reach, and nothing would end it if the turn never
  completes.

## Consequences

- A revoked owner's pending stop is not finished: the cancellation
  interrupts the graceful stop, the writer is stopped by force, and the work
  since the session's last checkpoint is lost.
- The cancellation is final. Admitting the installation to the project
  again later does not bring the session back, since a cancelled session
  cannot be resumed. Tasks, and interactive sessions that were starting or
  active, already behaved this way.
- Only sealed usage is deducted; usage that cannot be sealed stays
  encumbered, as for any cancellation.
- Because revocation is read at start-up, this is found only when the
  executor starts and resumes its unsettled sessions.

## Revisit conditions

- The executor reloads project membership while running, so revocation can
  be found while a session is driven.
- A revoked owner should be able to end its own session gracefully, for
  example to keep the checkpoint of a pending stop.
- Revocation should suspend work rather than end it, so that re-admitting
  the project resumes it.

## Links

- Partly supersedes [ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
  section 8: an unsettled stop or cancellation waits for `recover` only
  while the session's project admits this installation.
- Code: `internal/work/decision.go`, `internal/work/driver.go`,
  `internal/work/project.go` (`access`).
- Tests: `internal/work/revoked_test.go`
  (`TestRevokedUnsettledSessionIsSettled`,
  `TestDriverTakesNoTaskActionForAnInteractiveSession`),
  `internal/work/testdata/driver-outcomes.txt`.
