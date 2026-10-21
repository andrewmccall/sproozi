# 0035: Bounded graceful stops and settled terminal input

Status: accepted
Date: 2026-10-20

Extended: [ADR 0038](0038-exited-agents-and-one-recovery-path.md) makes a
repeated stop continue the pending one and names `sproozi stop ID` in every
message for a stuck stop.

## Problem and constraints

`stop` and `move` wait for a completed native turn
([ADR 0004](0004-portable-sessions-and-bounded-executions.md),
[ADR 0011](0011-native-session-compatibility-and-completion.md)). Testers saw
Claude Code stops wait for many minutes after a turn that had finished, with no
output, until they cancelled. Two causes were found live:

- Claude Code 2.1.302 writes `api-request-blob` request caches with role
  `system` from a conversation's second model request. The transcript
  validator accepted only `user` and `assistant`, so it rejected every such
  transcript, and the supervisor could never see a completed turn.
- The completion barrier counted every accepted terminal write as new input
  after the last `Stop` hook. A real terminal writes without the user typing a
  prompt: focus reports when the window loses focus, and replies to the
  queries Claude Code prints, including those replayed on attach. Any of
  these blocked the stop until the execution deadline.

The barrier exists because a prompt queued during a turn is submitted after
that turn's `Stop`, so an earlier completion must not be reused while such a
prompt could follow.

## Decision

- Accept role `system` in Claude request caches. They stay outside the
  primary chain and change neither its leaf nor launch configuration.
- Keep the input barrier but let it settle: once input is gated, if neither a
  terminal write nor a lifecycle hook has arrived for three seconds, input
  accepted after the completed turn no longer blocks it. A prompt that input
  submitted, or one queued during the turn, reports `UserPromptSubmit` within
  that time and clears the completion as before.
- Bound each graceful wait to two minutes: the wait for a first visit's
  conversation ID and the supervisor's quiesce. When it passes, the supervisor
  leaves the harness running with input gated and reports what it waits for,
  in fixed words. The stop fails with `ErrNativeTurnPending`, the execution
  stays `stopping`, and the message says to repeat the command or run
  `sproozi cancel ID`. Repeating it waits again from the same state.
- The CLI prints, after 30 seconds of `stop` or `move`, that it waits for the
  current turn and how to force-stop instead.

## Alternatives

- **Count only writes that contain Enter.** It ignores focus reports and query
  replies but still blocks after Enter on an empty prompt, which submits
  nothing and reports no hook.
- **Drop the barrier.** A prompt queued during the turn could then be lost by
  a stop that accepted the earlier completion.
- **Force-stop when the bound passes.** That would interrupt long genuine
  turns, which ADR 0004 waits for; the user chooses with `cancel`.
- **Show live progress from the placement.** The placement holds its operation
  lock while it waits, so status cannot ask the supervisor without new
  concurrency; a bounded reply gives the same information.

## Consequences and limits

- A prompt whose hook arrives more than three seconds after the last input or
  hook could be lost by a stop. Claude Code reports it synchronously before
  its model call.
- A turn longer than two minutes needs the stop repeated. The session keeps
  running with input paused meanwhile, and `attach` refuses it until the stop
  finishes or is cancelled.
- The request-cache rule follows Claude Code 2.1.302 and needs review with
  each client update; an unknown record still blocks the stop, now with a
  bounded, explained wait.

Revisit if Claude Code offers a lifecycle signal for queued prompts, or if
users need status to show what a running stop waits for.

Implementation: [native supervisor](../../cmd/native-session/main.go),
[Claude lifecycle](../../cmd/native-session/claude.go),
[Claude transcript rules](../../internal/harness/claude_state.go),
[shared handoff](../../internal/harness/native_control.go),
[coordinator](../../internal/session/coordinator.go) and
[CLI](../../cmd/sproozi/session.go).
