# 0064: The supervisor alone stops a harness at its deadline

Status: accepted
Date: 2026-11-17

## Problem and constraints

A managed harness runs on a terminal (`internal/nativepty`) inside the
[supervisor](../../GLOSSARY.md#supervisor) (`cmd/native-session`). Until this
decision three mechanisms acted on the execution's deadline:

- `serve`'s watcher, which read the wall clock once a second and stopped the
  harness with the cause `deadline`;
- the terminal, which `activate` handed the deadline. `nativepty.Start`
  refused a passed one, and the terminal killed its process group at the
  deadline on its own once-a-second clock reading. Its exit carried the
  reason `deadline`, which the exit watcher reported as the cause `deadline`;
- `quiesce`'s own deadline context while it waited for a turn.

They raced. Which one acted first decided the exit's reason, and so whether
Claude Code's and Copilot's stop accepted a kill after a completed turn
(their settle refuses any reason but `forced-stop`), and whether a harness
that exited by itself near the deadline was reported as `exited` or
`deadline`. [ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md)
asks for one deadline column and one event; three enforcers made that
untestable.

An execution may have no deadline ([ADR 0056](0056-whoever-starts-work-sets-its-deadline.md)),
and no state may wait on one to end ([ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md)).

## Decision

**The supervisor is the only thing that acts on the execution's deadline.**
`cmd/native-session/deadline.go` holds it and is its only reader. It raises
one event, `deadline`, once (`expire`, behind a `sync.Once`):

- from `watchDeadline`, which `serve` runs and which reads the wall clock
  once a second (`deadline.Deadline.Done`);
- from a `quiesce` that finds the deadline passed: at each 100 ms poll
  while it waits for a turn, and when a quit, a forced stop or a turnless
  kill did not finish.

Raising it stops the harness with the cause `deadline`, unless the harness
already exited while nothing was forcing or finishing it: that exit is its
own ([ADR 0038](0038-exited-agents-and-one-recovery-path.md)) and the exit
watcher settles it. "Already exited" means the terminal has reaped the exit.
A harness that exits by itself in the instant before that, while the
deadline is being raised, is still reported with the cause `deadline`.

`activate` refuses after the deadline on entry, and checks again just
before it starts the terminal, because preparing a launch can take seconds
(the gateway forwarder, client installs, a login store wait of up to five
seconds, the adapter's preparation). The second refusal gives the reply the
terminal gave when it refused a passed deadline.

The terminal gets no deadline (`terminalLaunch`). `internal/nativepty` is
unchanged and still supports one for other callers.

## Alternatives

- **Keep both enforcers.** The terminal's kill needed no supervisor lock: it
  takes only the terminal's own locks, so it would stop the harness even if
  the supervisor's mutex were held for ever or its watcher never started. It
  was rejected because the race above decided causes and replies, and
  because a lifecycle with two sources for one event cannot say which
  event happened.
- **Terminal only.** One enforcer without a lock, but it can stop only a
  launched harness. A supervisor still unconfigured or prepared at its
  deadline must also stop, and the cause would again be inferred from an
  exit reason rather than decided as an event.

## Tradeoffs

The kill at the deadline now goes through the supervisor's `stop`, which
takes its mutex briefly. `activate` holds that mutex while it prepares a
launch, so a deadline passing then is acted on when `activate` returns; it
then refuses the launch. The deadline can still pass in the instant between
that re-check and the terminal's start: a harness started then runs until
the watcher's next clock reading, at most about a second, and is stopped
with the cause `deadline`. A defect
that held the mutex while the harness runs, or a `serve` that did not start
the watcher, would leave the harness running past its deadline with no
second enforcer to stop it.

## What guards the single enforcer

- `TestServeStopsTheHarnessAtItsDeadline` runs `serve`, activates a harness
  through its socket and asserts it is killed at the deadline with an exit
  report of cause `deadline` and a nonzero code. It fails if `serve` does
  not start the watcher.
- `TestDeadlineDuringActivationStartsNoHarness` makes the preparation
  outlast the deadline and asserts no terminal starts;
  `TestActivateAfterTheDeadlineIsRefused` covers the refusal on entry.
- `TestExecutionDeadlineIsReportedAsDeadline`,
  `TestQuiesceFindingAnOwnExitAndThePassedDeadlineReportsTheExit` and
  `TestDeadlineRaisedAfterAnOwnExitLeavesItTheHarnessesOwn` cover the
  event's outcome, and the lifecycle record's late world lets the watcher
  stop the harness.

## Consequences

- A stopped `quiesce` reply's `exit.reason` is never `deadline`.
- A forced stop, the deadline's included, that kills Codex while
  `quiesce` is asking it to quit makes `quiesce` reply `stopping` with an
  error naming the cause, as the exit report does.
- Claude Code and Copilot keep more work. They are ended by a kill once
  their turn is observed complete, and with the terminal's kill gone a
  deadline that lands after the turn completed arrives through the
  supervisor's stop, whose kill ends them as their own would: the stop is
  acknowledged and the turn kept. When the terminal's own kill landed first,
  its reason, `deadline`, made their stop fail and the turn was lost. This
  is a change of behaviour, toward keeping work.
- [ADR 0061](0061-a-stop-survives-an-interrupted-write.md)'s kill record
  still holds: the deadline's kill is now a stop, which runs the terminal's
  `BeforeKill` like any other.

## Revisit conditions

- The supervisor gains a path that holds its mutex across unbounded work
  while the harness runs.
- A placement needs to tell a kill at the deadline from another forced stop
  by the exit reason rather than the report's cause.
- Another caller of `internal/nativepty` runs a harness without a
  supervisor that owns its deadline.

## Links

- Implementation: `cmd/native-session/deadline.go`,
  `cmd/native-session/main.go` (`serve`, `activate`, `quiesce`,
  `terminalLaunch`), `cmd/native-session/lifecycle.go`
- Tests: `cmd/native-session/serve_deadline_test.go`,
  `cmd/native-session/activate_deadline_test.go`,
  `cmd/native-session/deadline_exit_test.go`,
  `cmd/native-session/exit_test.go`,
  `cmd/native-session/lifecycle_record_test.go`
- Reference: [native launch and control](../reference/native-control.md)
- Related: [ADR 0038](0038-exited-agents-and-one-recovery-path.md),
  [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md),
  [ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md),
  [ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md),
  [ADR 0061](0061-a-stop-survives-an-interrupted-write.md),
  [ADR 0062](0062-one-exit-rule-and-exits-over-the-wire.md)
