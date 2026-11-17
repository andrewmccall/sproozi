# 0068: A lost finish is proven by the helper's end

Status: accepted
Date: 2026-11-17

## Problem and constraints

A Kubernetes stop ends with two steps after the visit's writer is fenced, its
usage sealed, its checkpoint kept and its
[session registration](../../GLOSSARY.md#session-registration) recorded
`stopped` with that receipt: the placement runs `checkpoint finish` with an
exec into the Pod's `checkpoint` container, then releases the Pod's
writer-fence finalizer. `finish` asks the helper's `hold`, the container's
main process, to end, so the Pod can complete.

`hold` wrote its reply and returned at once. Its return ends the container,
and the container's end can kill the exec'd `finish` before its exit status
reaches the remote executor. The exec then failed, `Stop` returned
`placement: fixed command unavailable`, and the coordinator recorded
`unknown-stop` for a stop that had in fact completed. A proof run on a Kind
cluster ended this way once, with the helper's container reported `Completed`,
exit code 0, at the same second. The supervisor already guards its own reply
the same way (`quiesceReplyGrace`); the helper did not.

Nothing settles such an interactive session without its owner: the exit
sweep stops only active sessions, and an unsettled stop waits for `recover`
([ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
section 8). `recover` succeeds, because the replay of a stopped registration
(`finishStopped`) skips `finish` once the helper is not running.

The exec's own error was dropped (`commandFailed` kept only an exit code), so
the failure could not be told from any other.

`unknown-stop` must still mean what [ADR 0010](0010-managed-session-authority-and-admission.md)
asks: a stop is complete only on evidence. A lost acknowledgement may be
taken as done only when the effect it acknowledges can be observed.

## Decision

1. **The helper waits for the client that asked it to finish.** After it
   writes `finished`, `hold` half-closes its side of the connection, which
   ends the reply, and waits for the client to close, at most 10 seconds
   (`finishReplyGrace`), before it returns. This is the supervisor's
   `quiesceReplyGrace` shape. The client reads the reply to its end and then
   closes, so the helper ends only after `finish` has its answer. The
   request is now read by its length (`finish\n`), not to the end of the
   stream: the client used to half-close after writing it, which would have
   ended the helper's wait at once. The client and the helper are the same
   binary in the same image, so the protocol changes for both at once. The
   commands share nothing, so the grace is the helper's own constant.
2. **The placement proves a failed finish by observation.** When the exec of
   `finish` fails, in `Stop` or in the replay of a stopped registration
   (`finishStopped`, which `Stop` and `Abort` run), the placement reads the
   exact Pod again, every 100 milliseconds for at most 15 seconds
   (`helperEndWait`). The exec breaks when the helper's container ends, but
   the kubelet reports that end only at its next relist, about a second
   later, so a single read at once would usually still see the helper
   running. 15 seconds covers the helper's own 10-second reply grace and a
   relist; the caller's context ends the wait sooner, and the wait applies
   only after a failed finish, so a stop the owner waits on is held at most
   that long once. A reading that can never become proof (the helper ended
   with another code, which the Pod never restarts; a replaced Pod) ends the
   wait at once. The finish is done when the registration is `stopped`
   with a receipt, the retained record holds the writer's fence (which a
   stopped registration's receipt proves,
   [ADR 0066](0066-a-contradicted-retained-record-refuses-work-and-never-blocks-an-end.md)),
   and either:
   - the runtime reports the `checkpoint` container terminated with exit
     code 0 (a proven termination: its container ID, a finish time, not the
     kubelet's `ContainerStatusUnknown`). `hold` returns 0 only after it
     acknowledged a finish: its deadline, `SIGTERM` and every error exit 1,
     and the Pod never restarts a container; or
   - the Pod is gone. No helper is left to finish, and `finishStopped`
     already takes a gone Pod with a retained fence as finished. Its
     writer-fence finalizer holds the Pod until the stop releases it, so it
     is gone only when something else removed the finalizer.

   The stop then releases the writer-fence finalizer and returns the stop it
   recorded, and the placement logs `Treated the finish of the checkpoint
   helper in Pod NAMESPACE/NAME as done, because …: ERROR`. A helper still
   running when the wait ends, one that ended with another code, a
   termination the runtime did not report for that container, a replaced
   Pod, or a Pod that cannot be read within the wait proves nothing: the
   stop fails exactly as before.

   **A first stop and its replay can decide differently for the same
   reading.** For a helper that ended with code 137, 1 or 2 after a failed
   finish, the first `Stop` is unproven and the coordinator records
   `unknown-stop`; the replay of the now stopped registration
   (`finishStopped`) skips `finish` because the helper is not running, and
   ends the stop with the same digest, `finish` having run once. That is
   harmless: the writer's fence is proven separately, from the native
   container's end, before usage is sealed and the registration stopped, so
   neither answer can report a writer stopped that was not; the difference
   costs one `recover` for an interactive session or one backoff for a task.
   The replay's looser rule is kept deliberately: tightening it would
   strand, for good, a stop whose helper reached its deadline or was killed
   before `recover`, since nothing could then ever finish it.
3. **A failed fixed command keeps the exec's error, without its request.**
   `commandFailed` keeps the command's executable and operation, the
   container and the exec error's text, on one line and at most 512 bytes,
   in its message: `placement: fixed command unavailable: checkpoint finish
   in container checkpoint: command terminated with exit code 137`. The text
   comes from the exec client, the API server, the kubelet and the container
   runtime: an exit code, a broken stream, a refused connection with the API
   server's address, a container that is gone, a timeout. When the exec
   request itself fails, client-go reports the request's URL, whose query
   holds every argument of the command (the files a capture selects, its
   thread, the instruction digest); it folds that `*url.Error` into plain
   text (`error sending request: %v`). So a `*url.Error` in the chain keeps
   only its operation and inner error, and any URL or query left in the text
   is replaced by `request`: `Post request: dial tcp 10.0.0.1:6443: connect:
   connection refused`. The command's arguments and its output, which the
   sandbox can write, are not kept. The message reaches the executor's log
   through the driver; the placement protocol and the work API still answer
   only a code (`unavailable`). `commandFailed` does not wrap the exec
   error, so no caller's `errors.Is` check changes.

Only `finish` gains a proof. The other fixed commands of `Stop`, `Abort` and
`AbortPreparation` either have no observable effect or are retried safely:

| Fixed command | Where | A lost acknowledgement means | Proof by observation |
| --- | --- | --- | --- |
| supervisor `gate` | `Stop`, active visit | Input may or may not be gated. Nothing is recorded; a retry gates again. | None: the gate lives in the supervisor's memory. Stays unproven. |
| supervisor `status` | `Stop`, while a first visit's conversation is awaited, and the replay of a stopping registration | A read that returned nothing. A retry reads again. | Not needed: it changes nothing. Stays unproven. |
| supervisor `quiesce` | `Stop`, active visit | The turn may or may not have completed. | Already proven for a harness whose state proves completion, by the native container's end with code 0 and no exit report (ADR 0062); none for Claude Code, whose stopped container proves no turn. Unchanged. |
| helper `capture-stopped` | `Stop` | The checkpoint bytes are lost with the reply. | None: the bytes are the result. A retry captures again while the helper still runs. Unchanged. |
| helper `finish` | `Stop`; `finishStopped` in `Stop` and `Abort` of a stopped registration | The helper may or may not have ended. | The helper's proven exit 0, or a gone Pod, beside a stopped registration and a fenced writer. **This decision.** |

`Abort` of a visit that is not stopped and `AbortPreparation` run no fixed
command: they fence by deleting the Pod and observing its containers end.

## Alternatives

- **Fix only the helper.** It closes the race the run showed, but a lost
  stream or an API server blip during `finish` would still leave the stop
  unproven while the helper had ended, and the client closes a moment before
  its process exits, so a window remains.
- **Fix only the placement.** Every lost acknowledgement whose helper ended
  0 would settle, but the race would stay common enough to log on every
  such stop, and a `finish` whose exec failed for another reason would
  still depend on the Pod read.
- **Treat any failed finish of a stopped registration as done**, as
  `finishStopped` does for a helper that is not running. The stop would then
  report complete for a helper that is still running, holding the volume,
  or that ended for another reason, such as its deadline, and the placement
  could not tell a finish from a kill. The replay's looser rule is left as
  it was: tightening it would strand a stop whose helper reached its
  deadline before `recover`.
- **Report the fenced stop with an error**, so the coordinator records
  `unknown-usage` rather than `unknown-stop`. The usage is sealed and the
  stop completed; neither phase says so, and both still wait for `recover`.
- **Let the client keep the connection until its process exits**, so the
  helper sees the close only from an exiting process. Go may close an
  unreachable connection earlier, and the gain is the same window the
  placement's proof covers.
- **Wrap the exec error.** Callers test the chain for `context.Canceled` and
  other sentinels; an exec failing because its context ended would then
  change how the driver and the coordinator treat a stop. Keeping the text
  changes nothing but the message.

## Tradeoffs and consequences

- A stop whose finish acknowledgement is lost completes on the first
  attempt when the Pod shows the helper ended with code 0, or is gone,
  within the 15-second wait: the session shows its stopped state
  (`retained` or `failed` on the executor, `idle` in the execution journal)
  rather than `unknown-stop`, and the writer-fence finalizer is released at
  once. A kubelet slower than that to report the helper's end, or an API
  server that cannot be read meanwhile, leaves the first attempt unproven
  as before; its replay then completes the stop.
- Such a stop takes up to 15 seconds longer, once, while the Pod catches up.
  That comes out of the stop's 5-minute settle allowance
  (`stopSettleBound` in `internal/session/profile`), which also covers
  ending the native container (at most a minute), sealing usage and
  capturing the checkpoint; the stop's own context, bounded by
  `StopBound`, still ends the wait.
- A lost finish whose helper still runs, or whose Pod cannot be read, still
  leaves `unknown-stop`. An interactive session whose harness exited by
  itself then still waits for its owner's `recover`.
- A helper whose client hangs without closing keeps the sandbox up to 10
  seconds longer. The client's own deadline is one second.
- Failed fixed commands log the exec's error, so the next such failure can
  be told apart.

## Revisit conditions

- The exit sweep, or the driver, retries an interactive session's stop that
  it began itself; then a lost finish whose helper still runs would settle
  without the owner too.
- The helper gains other ways to exit 0, which would end the proof.
- The capture container may restart, or the Pod spec moves the helper out of
  its own container.

## Links

- Implementation: `cmd/checkpoint/helper.go` (`serve`, `finish`,
  `finishReplyGrace`), `internal/runtime/kubernetes/placement/lifecycle.go`
  (`finish`, `helperFinished`, `finishReading`, `helperEndWait`,
  `finishStopped`), `internal/runtime/kubernetes/placement/exec.go`
  (`commandFailed`, `execCause`)
- Tests: `cmd/checkpoint/hold_test.go`,
  `internal/runtime/kubernetes/placement/finish_lost_test.go`,
  `internal/runtime/kubernetes/placement/exec_cause_test.go`,
  `internal/runtime/kubernetes/placement/pod_record_test.go` (the
  `lost-finish` column of `testdata/pod-readings.txt`),
  `internal/runtime/docker/placement_test.go`
  (`TestDockerStopDoesNotDependOnFinishAcknowledgement`: the Docker stop
  ignores the result of the same `finish` and then stops the helper
  container itself, so it has no such race)
- Reference: [checkpoint helper](../../cmd/checkpoint/README.md),
  [remote executors](../reference/remote-executors.md)
- Related: [ADR 0010](0010-managed-session-authority-and-admission.md),
  [ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
  [ADR 0062](0062-one-exit-rule-and-exits-over-the-wire.md),
  [ADR 0066](0066-a-contradicted-retained-record-refuses-work-and-never-blocks-an-end.md)
