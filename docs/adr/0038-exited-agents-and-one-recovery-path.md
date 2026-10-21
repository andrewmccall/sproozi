# 0038: Exited agents, resumed stops and one recovery path

Status: accepted
Date: 2026-10-20

## Problem and constraints

Testers found three lifecycle failures after
[ADR 0035](0035-bounded-graceful-stops-and-settled-input.md):

- An agent that exited by itself, such as Codex rejecting an unknown argument,
  left its session `active`. Nothing watched the container, and `stop` of a
  first visit with no bound conversation ID failed with `initial native UUID
  remains unknown`, leaving `unknown-stop`. A Claude container that stopped
  without the graceful acknowledgement refused every stop.
- A resumed Claude Code session stopped without a new prompt waited two
  minutes for "Claude Code to report its conversation": no hook ran, so the
  supervisor never learned the conversation ID it had restored.
- A stuck stop told the user to repeat it; repeating it with a new operation
  said "previous stop requires recovery"; `recover` said to repeat. After
  `cancel`, `resume` said to stop first and `stop` said the session was
  cancelled.

ADR 0011's rule still holds: a stopped process never proves a completed
Claude turn, so no conversation is claimed from one.

## Decision

- **Notice exits.** When its harness exits before any stop, the supervisor
  writes one bounded report line to its output: exit code, the last terminal
  lines as plain text, and, for a completed Codex turn, its conversation ID.
  The service checks active sessions every five seconds, and on `attach`. An
  agent container that stopped by itself is stopped through the ordinary stop
  path. The Docker placement records the exit, fences the container and seals
  usage. It captures a checkpoint only for Codex with a reported or bound
  conversation that validates; otherwise the session keeps its previous
  checkpoint, or the workspace it started from. The visit retains the exit.
  A nonzero code makes the stopped session `failed`; code 0 leaves it
  `retained`. `status` shows the code and output, `resume` continues from the
  kept state, and `rm` removes it.
- **A resumed visit with no prompt keeps its conversation.** If no prompt was
  submitted in a resumed Claude visit and gated input has settled, the
  supervisor validates the restored transcript of the selected conversation
  and stops at once, keeping it.
- **One recovery path.** A stop request while a stop is pending continues
  that stop, whatever its operation. Every message for a stuck stop names
  `sproozi stop ID`; a stuck move names `sproozi recover ID`. A cancelled
  session refuses `stop`, `resume`, `move` and `attach` with a message that
  names `sproozi rm ID`. `resume` of a running session names `attach`, and
  `attach` refuses a session that is not running before printing `Attached`.

## Alternatives

- **Show the exit only on demand.** Checking on `status` alone leaves `ls` and
  `service stop` wrong until someone looks, and still needs the same stop.
- **Cancel exited sessions.** Cancellation is final, so a typo in an agent
  argument would lose the session's workspace and conversation.
- **Read the exit from Docker logs of the harness itself.** The harness writes
  to its terminal, not the container's output; only the supervisor holds that
  output.
- **Keep refusing a second stop.** Operation IDs protect against replaying a
  different intent; two stops of the same execution are the same intent.

## Consequences and limits

- The exit report is workload output: a diagnostic only. A Codex report can
  name a conversation, which the placement accepts as it accepts the
  supervisor's status, and still validates before keeping it.
- A Claude session whose agent exits by itself keeps no new turns from that
  visit.
- Exits are noticed within about five seconds while the service runs, and at
  the next start of the service.
- Codex's exit output is its terminal screen, so a TUI that quits normally
  leaves screen fragments; the CLI shows output only for nonzero codes.

Revisit if a harness reports exits through a supported lifecycle channel, or
if remote placements need the same exit handling.

Implementation: [supervisor](../../cmd/native-session/main.go),
[Claude lifecycle](../../cmd/native-session/claude.go),
[exit report](../../internal/harness/native_control.go),
[Docker placement](../../internal/runtime/docker/placement.go),
[exit watcher and messages](../../internal/session/home/exits.go),
[stop continuation](../../internal/session/finalize.go) and
[status output](../../cmd/sproozi/output.go).
