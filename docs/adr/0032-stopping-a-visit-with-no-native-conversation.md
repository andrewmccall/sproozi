# 0032: Stopping a visit that has no native conversation

Status: accepted
Date: 2026-10-20

## Problem and constraints

`sproozi stop` waits for a completed native turn
([ADR 0004](0004-portable-sessions-and-bounded-executions.md),
[ADR 0011](0011-native-session-compatibility-and-completion.md)). A first
visit binds its harness conversation ID only when it observes one
([ADR 0018](0018-coordinator-owns-every-visit.md)). A session that never had a
turn therefore had nothing to wait for:

- Codex writes its rollout journal only when a turn starts. A session started
  with `--detach` and never attached has no journal, so
  `harness.QuiesceNative` polled for a thread until the execution deadline.
- Claude Code whose only turn ends on a provider error never fires `Stop`, so
  the supervisor's quiesce loop waited the same way.
- Cancelling the hung stop then fenced the writer, but the Docker gateway owner
  refused to seal usage for a first visit with no bound conversation ID. The
  execution stayed in `unknown-usage` with an active visit: `recover` exited 0
  with no change, a second `cancel` reported an intent conflict, and
  `service stop` refused because the session was still active.

The completed-turn boundary must still hold for real conversations: a turn in
progress is waited for, never interrupted.

## Decision

A gated first interactive visit with no native conversation to keep is
**turnless**. The supervisor decides it, once input has been gated and quiet
for two seconds, when the launch has no selected thread and no task, and:

- Codex has no file under its `sessions` journal directory; or
- Claude Code never reported `UserPromptSubmit`, or every submitted turn ended
  with the `StopFailure` hook before any turn completed.

The decision is latched. The supervisor's status reports `turnless`;
`QuiesceNative` then skips binding and asks for quiesce, which stops the
harness process group (a controlled stop, not typed input: a turnless Codex may
be showing a startup prompt) and replies `stopped` with `turnless` and no
thread. Only that reply may end quiesce without a thread.

The Docker placement records the visit as turnless, revokes and fences it,
seals its usage and captures nothing. The coordinator accepts a stop with no
checkpoint only when the execution has never kept one and the stop names no
thread. The visit is retained with its receipt and no checkpoint, and the
execution goes idle with its retained Git workspace input. A later visit of
the same execution from that workspace has no thread, like the first one; the
first stop that finds a conversation binds it. Journal validation allows such
workspace visits only before the first kept checkpoint.
[ADR 0034](0034-resume-continues-every-home-session.md) makes `resume` the way
to continue a stopped session: for a turnless one it starts a new execution
from the same workspace.

The Docker gateway owner now seals a first visit whose conversation ID was
never bound. The receipt then refuses any later binding. This also lets a
cancellation of such a visit finish instead of staying in `unknown-usage`.

`recover` no longer reports success while a cancellation stays unresolved: it
returns an error naming what it waits for (usage not finalized, or the stop not
yet proved). A `cancel` with a new operation on an execution that already has
one tells the user to run `recover`.

## Alternatives

- **Time out the wait and force-stop.** A deadline shorter than the
  execution's would bound the hang but would also interrupt real long turns,
  breaking the completed-turn boundary.
- **Capture the workspace without a conversation.** A workspace-only
  checkpoint would keep edits made during a failed turn. It needs a new
  checkpoint form, helper capture command and restore path on both placements.
  A turnless visit made no completed turn, so its workspace is the one it
  started from, except for tools a failed Claude turn ran before its error.
- **Treat a missing journal as "unknown" and make users cancel.** With the seal
  fix cancellation now works, but `stop` would still hang for the most common
  first-use case.
- **Detect any idle harness from gateway traffic.** Tools can run without model
  calls, so silence at the gateway does not prove a turn ended.

## Consequences and limits

- A prompt typed but not submitted before the stop is discarded, as before.
- Changes made by tools during a failed-only Claude turn are not kept: the
  next visit starts from the retained workspace.
- A conversation whose later turn failed after an earlier one completed is not
  turnless: its transcript cannot be checkpointed, so `stop` still waits and
  `cancel` is the way out. A Codex turn that failed with a provider error is
  likewise not detected; Codex records no failure hook to observe.
- A stopped turnless session resumes from its starting workspace. A move to
  Kubernetes, which needs a checkpoint, is refused for an execution with no
  kept conversation. A move to Kubernetes requested while a
  turnless first visit is still running stops it, then cannot prepare the
  destination; `cancel` ends that execution, and a new `start` begins again
  from the same directory, since the visit kept no work.
- [ADR 0035](0035-bounded-graceful-stops-and-settled-input.md) bounds each wait
  for a turn and keeps terminal input that submits nothing from blocking it.
- Kubernetes visits always carry a task or a checkpoint, so they are never
  turnless; their placement rejects a turnless reply.

Revisit if a harness gains a supported lifecycle signal for failed turns, if
users need edits from failed turns kept, or if Kubernetes visits start without
a task.

Implementation: [native supervisor](../../cmd/native-session/main.go),
[Claude lifecycle](../../cmd/native-session/claude.go),
[shared handoff](../../internal/harness/native_control.go),
[Docker placement](../../internal/runtime/docker/placement.go),
[Docker gateway owner](../../internal/gateway/docker/owner.go),
[coordinator](../../internal/session/coordinator.go) and
[journal rules](../../internal/session/rules.go).
