# 0034: Resume continues every home session

Status: accepted
Date: 2026-10-20

## Problem and constraints

[ADR 0012](0012-retained-work-and-fresh-session-continuations.md) introduced
`resume` for sessions imported with `pull`: a new execution, with a fresh
grant, allowance and deadline, continues the retained conversation and
workspace. A session created with `start` had no such path. After `stop` its
only execution was idle, `resume` refused it, and `move --to local` could
restart it only before that execution's original deadline. Testers expected
`stop` then `resume` to work, and the local sessions guide already said so.

The owner decided that resume continues a session where it stopped, including
after the original deadline. The project is unreleased, so no older journal
needs a compatibility path. Execution limits from
[ADR 0004](0004-portable-sessions-and-bounded-executions.md) still hold: a
deadline and allowance belong to one execution and are never extended.

## Decision

Every home session is a catalogue record. `start` writes one, with source
`home`, before its first visit starts; its first execution shares the session
UUID. Imported sessions keep their remote source. Stop, cancel, attach, move
and recover address the catalogued session's current execution, as they
already did for imported sessions. A stop commits the session's last stopped
visit and checkpoint, and the session becomes `retained`.

`resume ID [-- ARGS]` on a retained session creates a new execution at the
local placement of its harness from its last checkpoint, with the same
harness conversation ID, a new allowance and a new deadline, so it works after
the previous execution's deadline. A session stopped with no conversation
([ADR 0032](0032-stopping-a-visit-with-no-native-conversation.md)) resumes from
the workspace its last execution started from, as a new conversation. A
retained session refuses `attach`, `move`, `cancel` and `recover` and names
`resume`. `move --to NAME` remains the explicit placement change for a running
session.

A start interrupted after its execution but before its catalogue record is
adopted by the next `start` retry or `resume`.

## Alternatives

- **Resume a start-created session by moving its idle execution.** This reuses
  the execution, so it ends at the original deadline and spends the original
  allowance, which is what testers found broken.
- **Adopt start-created sessions into the catalogue only at resume.** Smaller,
  but stop, status and listing would treat the two kinds of session
  differently until then.
- **Give the first execution its own UUID.** It would separate session and
  execution identities fully, but every command, output and test that uses the
  start UUID for both would change, with no behaviour gained.

## Consequences and limits

- `resume` always runs locally. A session stopped on Kubernetes resumes at
  home; `move --to NAME` sends it back.
- A cancelled session cannot be resumed, as for imported sessions; a new
  `start` begins again.
- Status of a retained session reports the stopped execution's deadline, which
  can be in the past; `resume` sets a new one.

Revisit if sessions need to resume on a remote placement directly, or if
executions need identities separate from their session's first UUID.

Implementation: [home service](../../internal/session/home/service.go),
[catalogue and resume](../../internal/session/home/catalog.go),
[stop](../../internal/session/home/stop.go) and
[coordinator workspace input](../../internal/session/store.go).
