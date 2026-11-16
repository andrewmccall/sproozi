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
stopped state is refused. `session.FallsBack` decides which capture failures
that applies to (rule 2).

| Inputs | Kept | Stopped state refused | Any other capture failure |
| --- | --- | --- | --- |
| Not exited, not turnless | Captured checkpoint | The stop fails | The stop fails |
| Turnless (a first visit) | What it started from | No capture | No capture |
| Exited, state proves completion, with a conversation | Captured checkpoint | What it started from | The stop fails |
| Exited, state does not prove completion | What it started from | No capture | No capture |
| Exited, no conversation | What it started from | No capture | No capture |

A stop that fails is retried and captures again. "What it started from" is
the checkpoint a resumed visit started from, or, for a first visit, no
checkpoint, so the session keeps its starting workspace. The Docker placement
records either as no checkpoint, and the session keeps the one it had. The
Kubernetes placement writes a resumed visit's input as its output, as its
registry receipt needs, and a first visit's receipt names no checkpoint.

Both placements call the function. When Docker captures does not change: a
test over every record shape compares it with the expression it replaced.
Which failures fall back does change (rule 2). The two defects are rows of
the table: an exited visit whose stopped state is refused, such as a Codex
turn left unfinished, keeps what it started from, and a stopped Claude Code
container is never captured.

### 2. Only a refused stopped state falls back

The first version of this record fell back on any capture failure of an
exited harness, as Docker did. Review found that this discards a
conversation a retry could keep: a capture cut off by the stop's bound or an
abort, or a helper or exec failure, kept what the visit started from, and on
Kubernetes a resumed visit retained its input before its receipt, so every
retry kept it.

A stop now falls back only when the capture's failure is
`session.ErrUncapturable` and the stop's context has not ended. The writer is
fenced, so a refusal of its state is the same on every capture, and a retry
would decide the same. That is why the Kubernetes placement may retain a
resumed visit's fallback before its receipt is recorded. Both placements
return `session.ErrUncapturable` in two cases:

- **The helper refused the stopped state.** `capture-stopped` now says why it
  failed, with exit codes defined once in `internal/checkpoint` (`exit_codes.go`)
  and imported by the helper and both placements:

  | Code | Name | Meaning |
  | --- | --- | --- |
  | 0 | | The checkpoint is on standard output. |
  | 65 | `ExitStateRefused` (sysexits `EX_DATAERR`) | The fenced state does not make a checkpoint: no completed turn, as when the harness left its turn unfinished; another conversation; a failed continuity check; a state over the checkpoint bound; a conversation file that is absent, not a plain file or unreadable because of its own permissions; a native directory that fails the safety checks. The harness capture functions mark these with `harness.ErrStateRefused`. |
  | 74 | `ExitCaptureFailed` (sysexits `EX_IOERR`) | The capture could not be carried out: an I/O error from the volume, a Git failure, anything environmental. Repeating it may succeed. |
  | 1 | | Every failure of a helper older than the codes, and every failure of the helper's other commands. |

  The codes differ from 1, from a shell's 126 and 127, and from a signal's
  128+n. `checkpoint.HelperExitCode` reads the code from a Docker exec
  (`ExitCode`) or a Kubernetes exec (`ExitStatus`).
- **The bytes the helper returned do not validate** in the placement's own
  checks: `checkpoint.Inspect`, their harness and thread, and
  `checkpoint.ValidateContinuity`.

How each placement reads a failed capture:

| Helper result or error | Docker | Kubernetes |
| --- | --- | --- |
| 65, state refused | Falls back | Falls back |
| The placement's own checks of the returned bytes fail | Falls back | Falls back |
| 74, could not be carried out | Stop fails; retry captures | Stop fails; retry captures |
| Any other code, a signal or a killed helper (such as 137 or -1) | Stop fails | Stop fails |
| Exec, Docker or transport error with no exit code | Stop fails | Stop fails |
| 1 without the helper's failure line: the docker CLI's own failure, such as a daemon it cannot reach | Stop fails | Stop fails |
| The stop's context ended (its bound, an abort) | Stop fails | Stop fails |
| A failed write of the kept checkpoint | Stop fails | Stop fails |
| 1 with the helper's failure line, a helper older than the codes | Falls back, as Docker did before this record | Stop fails, as Kubernetes did before this record |

"Falls back" applies only to an exited harness whose stop would otherwise
capture (rule 1); a graceful stop fails on every capture failure.

Code 1 cannot be told apart, and the helper's image can be older than the
service or executor that runs it: a local installation keeps its sandbox
image across CLI upgrades, and a cluster's runtime image is the operator's.
So for code 1 each placement keeps what it did before this record, which is
no worse than before on either: Docker keeps what the visit started from for
an exited visit, and Kubernetes fails the stop, which a newer image fixes.
The docker CLI also exits 1 for its own failures, so Docker counts code 1 as
the helper's only when the error carries the line every helper release writes
to its standard error before it fails, `Checkpoint operation failed:`
(`checkpoint.HelperFailurePrefix`); the engine's error includes the command's
standard error. Kubernetes runs the helper with no shell in between and fails
the stop for code 1 anyway.

### 3. A lost quiesce reply is a graceful stop

A home-requested stop of a Codex visit could finish its handoff, end the
native container and lose its reply. Kubernetes then never recorded the visit
as quiet, and its retry recorded an exit from the container's code. Docker
treats the same event as graceful: when the state proves completion and the
agent container is seen to stop within about ten seconds, the stop goes on
with no exit.

The Kubernetes stop now does the same. After an unacknowledged quiesce of a
harness whose state proves completion, it waits up to ten seconds
(`lostReplyWait`) for the native container to end. Then, and on any retry
that finds the container ended, it treats the ending as this placement's own
graceful stop when the supervisor exited 0 and its last output lines (the
last 50 within 64 KiB, through `pods/log`) hold no exit report. The
supervisor writes a report for every other ending (ADR 0038). This needs no
new stored field: the evidence is the supervisor's own ending. Nothing that
marks the quiesce as sent is recorded, and the registration stays `active`
until the stop is quiet. Output that cannot be read, a nonzero code, or any
report, is an exit, as before.

The last lines are enough. The report is the last line the supervisor writes
before it exits, and the harness's terminal output never reaches the
container's log: it goes to the supervisor's terminal, which keeps it for the
report's `output` field. If a report were still missed, a Codex visit that
exited by itself with code 0 would be classed as a graceful stop, and that is
saved. A completed turn is then captured, which its state allows; an
unfinished one is refused, and since a graceful stop does not fall back, the
stop fails on every retry until it is cancelled. A Claude Code ending is
never classed as graceful.

The Docker placement makes the same decision on a retry. Before, it decided
only in the call that lost the reply, so a retry after that call ended first
recorded an exit for a graceful stop. A stop that finds the agent container
already stopped is now graceful under the same conditions: the state proves
completion, the supervisor exited 0, and its last 50 output lines, read
through `docker logs`, hold no exit report.

A Claude Code container never proves its turn (ADR 0011). A lost reply there
is an exit, and the visit keeps only what it started from, on both
placements.

### 4. The discard is logged

When a stop falls back, both placements log `Discarded the captured state of
stopped visit ALLOCATION of execution EXECUTION and kept the checkpoint (or
workspace) it started from: ERROR`. The Docker placement writes this to the
service's log and the Kubernetes placement to the executor's standard error.
The stopped visit cannot carry the fact to the user without a new field:
`session.Exit` holds the harness's code, cause, deadline and its own
terminal output, and `status` prints only those. It would need a field on
`session.Exit`, or on the stopped visit, such as `kept: "start"` with the
reason, carried on the wire and in the journal and the catalogue, and printed
by `status`. Older homes' strict decoders refuse an unknown field, so it would
need the same compatibility handling as rule 6.

### 5. A first visit that bound a conversation may keep no checkpoint

The Kubernetes registry accepted a stop receipt without a checkpoint only for
a first visit with no native UUID bound. A first Claude Code visit that bound
its conversation and then exited must now keep no checkpoint, so the receipt
rule is now: a resumed visit's receipt always names a checkpoint; a first
visit's may name none, whether or not it bound a native UUID; a first visit
with none bound never names one. The phase transition (`complete-stop`, from
stopping to stopped) is unchanged, and so are the fields. The placement
reports the bound thread with the stop, with the exit, as Docker does, and the
coordinator keeps the session's own thread and workspace because the stop has
an exit and no checkpoint. The registry's recorded-outcomes table holds the
new state and its replay conflicts.

### 6. An `exited` placement action

The placement protocol gains `exited`, carrying the launch and incarnation.
It is authorized, validated and never `expired`, like the other placement
actions, and is bounded by the connection's short timeout, since it reads one
Pod. The executor answers it from the Kubernetes placement's `Exited`,
`{"exited":true}` or no field; an executor whose placement cannot observe
exits answers `unsupported`. `Exited` only tries the visit's lock: a visit
another operation holds, such as a stop in progress, is being acted on and
reports no exit, so no request waits behind a stop. The protocol client and
home's remote client implement `session.ExitObserver`, so home's watcher
stops such a visit through the coordinator, which ends with what rule 1
keeps.

Home's watcher asks each session's placement in its own goroutine, at most
one check per session at a time, and bounds each question to ten seconds, so
an unreachable executor delays no other session's check. A failing check is
logged once each time its reason changes, and once when checks succeed again.

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
  loses a conversation, but a capture that cannot succeed leaves a stop that
  never finishes, which ADR 0060's principle forbids. It is what Kubernetes
  still does for a helper older than the result codes.
- **Fall back on any capture failure of an exited harness, as Docker did and
  this record first decided.** Every exited stop finishes, but a capture cut
  off by the stop's bound or an abort, or a helper or exec failure, drops a
  conversation that a retry would keep, and nothing says so.
- **Treat any failed helper command as a state refusal.** A helper crash, its
  timeout, an exec failure and a full volume look the same, so it is the
  previous alternative again. The result codes tell them apart.
- **Report the reason on the helper's standard error.** The Kubernetes
  placement discards the helper's text, Docker keeps 4 KiB of it inside an
  error, and matching text is fragile; an exit code is part of every exec's
  result.
- **Treat code 1 the same on both placements.** One rule would be simpler, but
  either choice makes one placement worse than before against an older image:
  falling back would let Kubernetes drop a conversation an environmental
  failure hid, and failing would leave Docker's exited stops unable to finish.
- **Record that the quiesce was sent.** A local flag saved before the quiesce
  would mark a lost reply directly, but it is a new stored field, and the
  supervisor's own ending already shows a graceful stop.
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

- A fallback, once taken, is kept: an exited visit whose stopped state is
  refused keeps what it started from, and a retry does not capture again. The
  state is fenced, so a capture would be refused the same way. The user sees
  only the exit; the log names the discard.
- Before this record, Docker kept what an exited visit started from on any
  capture failure. With a helper of this release it now fails the stop, and
  its retry captures, for: code 74; a failed verification of the helper
  container; a helper container that is not running; the docker CLI's own
  failure, code 1 without the helper's line; codes 126 and 127; a killed
  helper (137, or -1 when the stop's context killed the exec); an ended
  context; and a failed retain of the captured checkpoint.
- Against a helper image older than the result codes, nothing improves:
  Docker keeps what an exited visit started from for any helper failure, as
  before, and Kubernetes fails the stop of an exited visit whose state the
  helper refused on every retry, as before, until the image is updated; the
  session can be cancelled.
- What the helper classifies as refused is a list: the conversation file's
  own checks; a conversation file that is absent, not a plain file, or
  unreadable because of its own permissions or type; a native directory that
  fails the safety checks; a state over the bound; and a failed continuity
  check. One read error can be either, so its kind decides: not found,
  permission, not a directory and a link loop are the state; an I/O error
  from the volume is the environment. A workspace that Git refuses for its
  own content (other than its size) is reported as could not be carried out,
  so it fails the stop rather than risk discarding a conversation.
- A lost-reply stop is told from an exit by the supervisor's ending. If the
  supervisor's output cannot be read, as when `pods/log` is denied, the ending
  counts as an exit, as before.
- A first Claude Code visit that exited after its stop bound a conversation
  keeps none, and its next visit starts again from the workspace. A Claude
  Code stop whose reply was lost keeps only what the visit started from.
- Rolling back after this release stopped a first visit that way meets a
  registration whose receipt names a native UUID and no checkpoint, which an
  older release refuses to read. An older executor reports that visit's stop
  as failing. An older controller marks its run `Failed` with reason
  `SessionBindingRejected` instead of ending it as stopped. An older gateway
  fails closed for it, which denies nothing that should run, since the visit
  has stopped. Only such a visit is affected, and only until the components
  are updated again. Registrations the older release wrote all still load.
- Home asks each executor about each of its active sessions every five
  seconds, through the port-forward. A check against an unreachable executor
  holds that executor's client while it tries to connect, so a user's own
  stop or attach to that executor can wait up to the ten-second bound behind
  it. Other executors and Docker are not delayed.
- Home moves only resumed visits to a cluster, so home's watcher never meets a
  first visit there; first visits on a cluster belong to the executor's own
  owner, whose sweep already used the placement's `Exited`.
- Some capture failures are environmental but permanent: a workspace too
  large for the helper container's memory, so the helper is killed (137) on
  every attempt, or a workspace Git cannot bundle, which the helper reports
  as 74. The stop of such an exited visit fails on every retry. The user sees
  the session `unknown-stop` at home, or `stopping` on an executor, with the
  stop's failure each time they run `stop` or `recover`. The only command that
  ends it is `cancel`, which keeps no checkpoint from the visit and leaves the
  session cancelled, so it cannot be resumed, although the checkpoint the
  visit started from is still good. ADR 0060 says no state may have only an
  exit that destroys the session; this one still does (revisit conditions).

## Revisit when

- The owner decides how to end a stop that cannot capture, so that a session
  whose capture fails for ever keeps what the visit started from instead of
  being cancelled. The smallest decision: a stop option, such as
  `sproozi stop ID --keep-start`, that the session's owner may give (the home
  user for a home session, the executor's home principal for a remote one,
  through the existing stop action with one more request field), only for a
  visit whose writer is already fenced and whose usage is sealed. It records
  the choice in the execution's journal and the placement's record before it
  acts, finishes the stop with what the visit started from, logs it as a
  discard naming who asked, and shows it in `status`. It needs the owner to
  decide who may ask, whether a graceful stop may use it, and the wire and
  stored field.

- Images older than the capture helper's result codes are no longer
  supported: the legacy mapping of code 1 goes, and code 1 then fails the
  stop on both placements.
- A user needs to see a discard in `status`: `session.Exit` or the stopped
  visit then needs a field for it (rule 4).
- A harness's state can prove a completed turn of a stopped Claude Code
  container: the "state does not prove completion" row goes away for it.
- Registrations gain a version that can carry an exit, which would let the
  receipt rule name it.
- The protocol gains a push channel from executor to home, which would replace
  polling `exited`.

## Links

- Implementation: [shared rule](../../internal/session/kept.go),
  [helper result codes](../../internal/checkpoint/exit_codes.go),
  [refused states](../../internal/harness/state_refused.go),
  [checkpoint helper](../../cmd/checkpoint/main.go),
  [Kubernetes stop](../../internal/runtime/kubernetes/placement/lifecycle.go),
  [registry receipt](../../internal/runtime/kubernetes/registry/registry.go),
  [Docker stop](../../internal/runtime/docker/transitions.go),
  [protocol client](../../internal/session/profile/client.go),
  [protocol server](../../internal/session/profile/server.go),
  [remote client](../../internal/session/remote/client.go),
  [exit watcher](../../internal/session/home/exits.go)
- Tests: `internal/session/kept_test.go`, `internal/runtime/docker/kept_test.go`,
  `cmd/checkpoint/capture_result_test.go`,
  `internal/runtime/docker/helper_result_test.go`,
  `internal/runtime/kubernetes/placement/helper_result_test.go`,
  `internal/runtime/kubernetes/placement/exited_test.go`,
  `internal/runtime/kubernetes/placement/coordinator_test.go`,
  `internal/runtime/kubernetes/placement/remote_exited_test.go`,
  `internal/runtime/kubernetes/registry/initial_test.go`,
  `internal/runtime/kubernetes/registry/outcomes_test.go`,
  `internal/session/profile/exited_test.go`,
  `internal/session/home/exits_test.go`,
  `internal/session/home/exits_remote_test.go`
- References: [what a stopped visit keeps](../reference/native-control.md#what-a-stopped-visit-keeps),
  [noticing an exit](../reference/remote-executors.md#noticing-an-exit),
  [session commands](../reference/session-commands.md)
- Related: [ADR 0011](0011-native-session-compatibility-and-completion.md),
  [ADR 0032](0032-stopping-a-visit-with-no-native-conversation.md),
  [ADR 0036](0036-remote-executors-through-kubeconfig-contexts.md),
  [ADR 0038](0038-exited-agents-and-one-recovery-path.md),
  [ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md),
  [ADR 0061](0061-a-stop-survives-an-interrupted-write.md)
