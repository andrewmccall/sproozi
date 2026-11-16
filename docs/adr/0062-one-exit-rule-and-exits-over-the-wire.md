# 0062: One exit rule for both placements, and exits over the wire

Status: accepted
Date: 2026-11-16

## Problem and constraints

A managed harness can end by itself: the user typed `/exit`, it crashed, or
its deadline passed. The visit must still be stopped and settled, and what it
keeps must not depend on where it ran. [ADR 0038](0038-exited-agents-and-one-recovery-path.md)
set the rule on Docker, and its Kubernetes clarification of 2026-11-14 said
the Kubernetes placement now "stops as Docker does" and "still never claims a
conversation from a stopped container". Review of neighbouring changes found
three places where that was not so:

- **A capture that failed after a recorded exit failed every retry.** The
  Kubernetes placement returned the capture error for a visit with a recorded
  exit, such as a Codex visit that exited mid-turn, on every retry, so the stop
  never finished and the session could only be cancelled. Docker keeps what
  the visit started from. [ADR 0061](0061-a-stop-survives-an-interrupted-write.md)
  listed this as a limit outside its decision.
- **A conversation could be claimed from a stopped container.** A first
  Claude Code visit whose stop bound its conversation and answered
  `turn_pending`, and whose harness then exited, took the Kubernetes stop's
  capture arm. Claude Code's state does not prove that its last turn
  completed ([ADR 0011](0011-native-session-compatibility-and-completion.md)),
  so that checkpoint could hold a turn cut short.
- **Home never noticed an exit on a cluster.** Home's exit watcher asks a
  placement through an optional `Exited`. Only Docker had it: the protocol
  client home uses to reach a remote executor had no such action. A visit home
  moved to a cluster stayed `active` after its harness ended, holding its Pod
  and an admission slot until someone stopped it.

Constraints: the Kubernetes stop's order (fence, seal, capture, record,
release) and its fencing and sealing stay as they are; no new registration
transition; no stored-format change; a newer home must work with an older
executor, and an older home with a newer one.

## Decision

### 1. One rule decides what a stopped visit keeps

`session.StopKeeps` decides, for both placements, from five facts the
placement holds once the writer is fenced: first or resumed visit, turnless,
whether the harness exited, whether its state proves completion
(`harness.ProvesCompletion`) and whether the visit has a conversation. It
returns what the stop keeps and, when it captures, what it keeps if the
capture fails.

| Inputs | Kept | Capture fails |
| --- | --- | --- |
| Not exited, not turnless | Captured checkpoint | The stop fails; its retry captures again. |
| Turnless (a first visit) | What it started from | No capture. |
| Exited, state proves completion, with a conversation | Captured checkpoint | What it started from. |
| Exited, state does not prove completion | What it started from | No capture. |
| Exited, no conversation | What it started from | No capture. |

"What it started from" is the checkpoint a resumed visit started from, or,
for a first visit, no checkpoint, so the session keeps its starting workspace.
The Docker placement records either as no checkpoint, and the session keeps
the one it had. The Kubernetes placement writes a resumed visit's input as
its output, as its registry receipt needs, and a first visit's receipt names
no checkpoint.

Both placements call the function. Docker's outcome does not change: a test
over every record shape compares it with the expression it replaced. The two
defects are rows of the table: an exited visit's failed capture keeps what it
started from, and a stopped Claude Code container is never captured.

### 2. A first visit that bound a conversation may keep no checkpoint

The Kubernetes registry accepted a stop receipt without a checkpoint only for
a first visit with no native UUID bound. A first Claude Code visit that bound
its conversation and then exited must now keep no checkpoint, so the receipt
rule is now: a resumed visit's receipt always names a checkpoint; a first
visit's may name none, whether or not it bound a native UUID; a first visit
with none bound never names one. The phase transition (`complete-stop`, from
stopping to stopped) is unchanged, and so are the fields. The placement
reports the bound thread with the stop, with the exit, as Docker does, and the
coordinator keeps the session's own thread and workspace because the stop has
an exit and no checkpoint.

### 3. An `exited` placement action

The placement protocol gains `exited`, carrying the launch and incarnation.
It is authorized, validated and never `expired`, like the other placement
actions, and is bounded by the connection's short timeout, since it reads one
Pod. The executor answers it from the Kubernetes placement's `Exited`,
`{"exited":true}` or no field; an executor whose placement cannot observe
exits answers `unsupported`. The protocol client and home's remote client
implement `session.ExitObserver`, so home's watcher stops such a visit
through the coordinator, which ends with what rule 1 keeps.

Compatibility. A home of this release reads an older executor's `404
invalid_request`, or `unsupported`, as no exit, as it saw before, without an
error, and its connection stops asking; a new connection, made after a failure
or before its token expires, asks again, so an updated executor is noticed.
An older home never sends the action, and no other answer carries the field.

## Alternatives

- **Two rules with an agreement test.** Keep Docker's `needsCapture` and a
  Kubernetes switch, and run one table against both. It catches a
  disagreement only for rows someone wrote; one function cannot disagree.
  The function needs no backend: it lives in `internal/session`, beside the
  coordinator that reads its outcome.
- **Fail the stop after an exited capture fails, as Kubernetes did.** It never
  loses a conversation, but a capture that cannot succeed, such as a Codex turn
  left unfinished, leaves a stop that never finishes, which ADR 0060's
  principle forbids.
- **Capture a bound Claude Code conversation from a stopped container.** It
  keeps more, but the transcript may end mid-turn, and ADR 0011 refuses to
  claim a turn that nothing proved complete.
- **Record the exit in the registration.** The registry could then accept a
  receipt with no checkpoint only for an exited first visit. That changes the
  stored format, which older executors refuse; the receipt rule is narrower
  in effect, since only a first visit is affected and its workspace is the
  execution's state either way.
- **Have the executor's own exit sweep stop home-owned visits.** The executor
  does not own them: home's coordinator does
  ([ADR 0018](0018-coordinator-owns-every-visit.md)), and a stop the executor
  made by itself would leave home's journal `active` until its next stop.
- **Fold the exit into `inspect`.** An active observation would carry a new
  state or field that an older home's strict decoder refuses.

## Tradeoffs and consequences

- A Codex visit that exited and whose capture failed once keeps what it
  started from, even if a retry could have captured it: on Kubernetes the
  kept input is written before the receipt, and Docker never retried either.
  A capture failure from the helper or the disk is not distinguished from one
  the conversation caused.
- A first Claude Code visit that exited after its stop bound a conversation
  keeps none, and its next visit starts again from the workspace.
- An executor rolled back to an older release cannot load a registration this
  release stopped that way, because its receipt names a native UUID and no
  checkpoint; it reports that visit's stop as failing. Only such a visit is
  affected, and only until the executor is updated again. Registrations the
  older release wrote all still load.
- Home asks each executor once per session every five seconds while the
  session is active, through the port-forward. A stop in progress on the
  executor holds the visit's lock, so an `exited` during it waits, up to the
  connection's timeout; home asks only about visits it holds as active.
- Home moves only resumed visits to a cluster, so home's watcher never meets a
  first visit there; first visits on a cluster belong to the executor's own
  owner, whose sweep already used the placement's `Exited`.

## Revisit when

- A harness's state can prove a completed turn of a stopped Claude Code
  container: the "state does not prove completion" row goes away for it.
- Capture failures need telling apart, so that a transient one is retried
  instead of falling back.
- Registrations gain a version that can carry an exit, which would let the
  receipt rule name it.
- The protocol gains a push channel from executor to home, which would replace
  polling `exited`.

## Links

- Implementation: [shared rule](../../internal/session/kept.go),
  [Kubernetes stop](../../internal/runtime/kubernetes/placement/lifecycle.go),
  [registry receipt](../../internal/runtime/kubernetes/registry/registry.go),
  [Docker stop](../../internal/runtime/docker/transitions.go),
  [protocol client](../../internal/session/profile/client.go),
  [protocol server](../../internal/session/profile/server.go),
  [remote client](../../internal/session/remote/client.go),
  [exit watcher](../../internal/session/home/exits.go)
- Tests: `internal/session/kept_test.go`, `internal/runtime/docker/kept_test.go`,
  `internal/runtime/kubernetes/placement/exited_test.go`,
  `internal/runtime/kubernetes/placement/coordinator_test.go`,
  `internal/runtime/kubernetes/placement/remote_exited_test.go`,
  `internal/runtime/kubernetes/registry/initial_test.go`,
  `internal/session/profile/exited_test.go`
- References: [what a stopped visit keeps](../reference/native-control.md#what-a-stopped-visit-keeps),
  [noticing an exit](../reference/remote-executors.md#noticing-an-exit),
  [session commands](../reference/session-commands.md)
- Related: [ADR 0011](0011-native-session-compatibility-and-completion.md),
  [ADR 0032](0032-stopping-a-visit-with-no-native-conversation.md),
  [ADR 0036](0036-remote-executors-through-kubeconfig-contexts.md),
  [ADR 0038](0038-exited-agents-and-one-recovery-path.md),
  [ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md),
  [ADR 0061](0061-a-stop-survives-an-interrupted-write.md)
