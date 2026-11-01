# Native terminal ownership

`nativepty.Start` launches the trusted native executable with an explicit
environment, absolute workspace, terminal size and absolute deadline. Linux and
macOS use a real controlling PTY and an independent native process group. The
supervisor keeps draining output whether a client is connected or disconnected.

One `Attachment` holds the input lease. Closing it leaves the process running.
Reattachment supplies the previous output cursor or zero for the retained tail;
bounded replay reports a gap when older bytes have been discarded. A slow output
reader cannot stall the native process. Input writes are nonblocking and limited
to 4096 bytes; retain an unwritten suffix when `ErrInputBusy` is returned. Terminal
size changes propagate to the native PTY.

`GateInput` serializes with admitted writes and rejects subsequent client input
without interrupting the active turn. The caller observes the harness's
completed turn and resolves pending tools before calling
`QuitAfterCompletedTurn`. That method clears an unsubmitted composer draft and
sends native `/quit` with a distinct Enter key event. It does not parse terminal
text or native history and cannot establish the completed-turn precondition.
Deadline expiry and explicit stop forcibly terminate the owned process group.
The deadline is wall-clock time, read every second, so a host that slept past
it ends the process within a second of waking (`AtDeadline`).

The package controls the process it actually launches. If that process is
`docker start --attach`, its exit describes the Docker client, not the agent
inside the container. Use this package at the native-process boundary, or pair
it with runtime-owned engine termination and observation. No transport listener
or writable control socket is exposed by the package.

Unix process groups do not include descendants that call `setsid`, change groups
or double-fork. `Wait` confirms the observed group stopped, treating Linux
zombies as non-executing records. It is not a fence for every native writer. A
move must independently stop and inspect the actual Docker container or observe
Kubernetes agent-container termination before capture. Fresh authority, admission,
accounting and checkpoint integrity remain the caller's responsibilities.

The process and replay survive client disconnects, not supervisor restart. Runtime
recovery must reconcile its actual execution resources after supervisor loss.
Terminal bytes can contain sensitive output; the caller authorizes every
attachment and decides whether replay may be persisted.
