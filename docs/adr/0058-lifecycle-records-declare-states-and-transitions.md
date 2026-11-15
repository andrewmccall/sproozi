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
  `cmd/sproozi/output.go`, `internal/deadline`
- Tests: `internal/session/phase_test.go`, `internal/session/rules_test.go`,
  `cmd/sproozi/deadline_test.go`
- Reference: [status and listing output](../reference/session-commands.md#status-and-listing-output)
- Related: [ADR 0018](0018-coordinator-owns-every-visit.md),
  [ADR 0038](0038-exited-agents-and-one-recovery-path.md),
  [ADR 0041](0041-gateway-state-namespace-and-retained-state-owners.md),
  [ADR 0056](0056-whoever-starts-work-sets-its-deadline.md)
