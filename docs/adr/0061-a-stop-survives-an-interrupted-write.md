# 0061: A stop survives an interrupted write

Status: accepted
Date: 2026-11-16

## Problem and constraints

A stop ends with a kill more often than before. Since [ADR 0035's correction
of 2026-11-15](0035-bounded-graceful-stops-and-settled-input.md#correction-the-wait-after-the-quit-2026-11-15)
a Codex that ignores its quit is killed at the quit bound. Claude Code and
Copilot are always stopped by a kill. A kill can land while the harness is
writing. A login store on a stalled volume can also hold a write, or its
lock, for ever. A review of the quit bound found three places where an
interrupted write broke a stop or spread past it:

- **Capture.** A harness killed while it appended a row to its conversation
  file left a final line without its newline. Every adapter refused that file,
  so the stop failed on every retry: on Kubernetes after the recorded exit, and
  on Docker whenever the record had no exit, as after a lost reply. The visit's
  completed turns were lost although every complete row was valid.
- **The sign-in save.** The final sync of a login
  ([ADR 0049](0049-named-logins-share-a-live-store.md)) took the store's
  `flock` blocking, inside the stop and under the supervisor's action lock. A
  peer holding the lock on a stalled volume held the stop and the
  supervisor's exit for ever.
- **What is copied between a login file and the store.** The sync copied
  bytes either way without looking at them, and an empty file was a sign-out.
  A kill landing while a harness rewrote its login pushed half a login, or an
  empty file, to every session of that login, signing them all out. Once the
  save was bounded, a save given up part way through its write could leave the
  store itself empty or half written, and every other session copied that in.

[ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md) states the principle:
nothing should leave a session in a state it cannot leave. Constraints: no
stored-format change, no new field between supervisor and placement (older
executors must keep working), and never accept a truncated row or file while
its writer may still run.

## Decision

### 1. Capture keeps the rows a fenced writer finished

Codex's rollout, Claude Code's transcript and Copilot's events journal are all
append-only JSON lines. JSON escapes any newline inside a row, so a final line
without a newline is an append cut short, and the rows before it are exactly
the file at its last completed write. Capture runs only after the placement
has fenced every writer (ADR 0008). It keeps the complete rows, leaves the
unfinished one out and validates the rows kept as before
(`internal/harness/stopped_journal.go`). A cut inside the completed turn
leaves no completed turn and is refused. The live observers still refuse an
unfinished final row, because the writer may still run. The checkpoint holds
a prefix of the source file, so the stored format does not change.

A Codex stop whose forced kill cut a row still fails its first attempt: the
supervisor observes the turn again and refuses the row. Its message now says
that a row was left unfinished and that repeating the stop completes it. The
repeated stop captures the complete rows on both placements. Claude Code and
Copilot stops, which are not observed again, finish on the first attempt.

### 2. The login store's lock is bounded, and a skipped save is only logged

Each sync tries the store's lock without blocking and gives up after five
seconds. A stop's save as a whole, including a periodic sync still in flight,
waits at most five seconds; then the stop completes, because the harness is
already stopped and its checkpoint matters more. Once a save's bound has
passed, no lock is taken and nothing is written. The store keeps what it had,
and the supervisor writes `Skipped saving the harness's sign-in ...` to its
standard error. Nothing is reported to the placement.

### 3. Only a whole login, or a settled sign-out, crosses between file and store

The supervisor (`cmd/native-session/signin.go`) and the Claude Code AgentRun
helper (`claude-launch.py`) apply the same rules in both directions:

- **A whole login.** Every managed harness keeps its login as one JSON object:
  Codex's `auth.json`, Claude Code's `.credentials.json` and Copilot's
  `config.json` after its `//` comment header. Anything that does not parse
  as one is not copied, out of the harness or into it. A harness keeps its own
  login while the store holds no whole login, and its next change replaces
  the store. A session that starts then starts without a login, and its
  login file as it stands after the supervisor's own start-up writes is its
  starting point, not a change. That write is the workspace trust merged into
  Copilot's `config.json`, a whole object with no login; Codex and Claude Code
  have none. Only a change the harness makes after that, such as a sign-in,
  replaces the store. A session that starts with a whole or empty store
  shares the start-up write as before. A file left
  half written before launch is replaced by the store's login. No particular
  member is required: which members a client writes is the client's choice,
  and a renamed field would otherwise silently stop the store updating.
- **A sign-out.** Empty, missing, an object with no members, and Copilot's
  comment header alone all mean "not signed in". A sign-out crosses only when
  two consecutive syncs see it. Codex and Claude Code rewrite their login by
  truncating it first, so one sync can catch a file, or the store, empty for
  an instant. From the harness's file, a sign-out crosses only while it runs
  with input not yet gated for a stop. Once a stop has begun, a truncate cut
  short by the coming kill would look the same.
- **Nothing after a kill.** The terminal supervisor
  (`nativepty.Launch.BeforeKill`) records every kill in the login sync before
  sending it: a stop, a quit its caller gave up on, and the deadline. The exit
  watcher records a kill from outside once it sees it. The helper records a
  kill before forwarding a signal to Claude Code, and when a signal ended it.
  From then on nothing from the harness's file is copied out, not even a whole
  login, and a sync with nothing to save does not open the store.
- **No store write leaves a state a reader acts on.** The store is rewritten
  in place. Its first byte becomes `#`, the rest is written and cut to length,
  and the first byte is written last, with a sync between. Every write puts
  down all its bytes or fails: Go's `WriteAt` reports a short write as an
  error, and the helper loops on `os.pwrite`'s count, which can fall short
  without raising. A write that stops at any point leaves the old login, the
  new one, or a file that is not a whole login, never an empty store. A
  sign-out is a single truncate. The order of the last two steps, cutting to
  length and then writing the first byte, is not needed for safety and no
  test binds it: the reverse order leaves a whole new login followed by old
  bytes for a moment, which does not parse.

A sync that keeps failing or refusing, as for a client whose login format is
not a JSON object or a store another holder keeps locked, leaves the store as
it was. It is logged once when syncing stops, naming the reason, again if the
reason changes, and once when syncing resumes.

## Alternatives

- **Fall back to the previous checkpoint when a capture fails after a stop.**
  It converges too, but it discards valid completed turns, and a first visit
  has no previous checkpoint. Kept as the answer only where the state cannot
  prove a completed turn.
- **Repair the cut row.** Sproozi does not write native history.
- **Let the stop wait for the save.** That was the defect: a stalled volume
  held the stop for ever.
- **Report a skipped save to the placement.** A reply or exit-report field
  would be a wire change older executors refuse, and the stop's outcome does
  not depend on the save.
- **Replace the store by rename.** The lock is the store file's own `flock`.
  A rename would leave it on the old file while other sessions opened the new
  one unlocked. A Docker volume's directory also belongs to root.
- **Write, then truncate to length.** A write that stops part way over a
  shorter old login can leave valid JSON that is neither login, such as
  `{"t":"bb"}` from `{"t":"bbbbbbbb"}` written over `{"t":"aaaa"}`.
- **Require a known credential member.** That is more exact for each client,
  but Copilot's `config.json` also holds settings the supervisor itself writes,
  and a client that renamed its member would stop the store updating without
  failing. An object with no members counts as empty instead.
- **Save a whole login after a kill.** This keeps a refresh made in the
  second before the kill. But it relies on every client truncating or renaming
  when it rewrites its login, which has not been checked against the stock
  clients. One that overwrites in place could leave a whole login that is
  wrong.
- **Share a sign-out on one read.** A real sign-out would arrive up to two
  seconds sooner, but a rewrite caught after its truncate would sign every
  session out.
- **Let only a change from a start-up baseline the harness itself settles
  repair an unfinished store.** The end of the harness's own start-up writes
  cannot be observed, and Copilot rewrites its settings later anyway. The
  supervisor's own writes are known, so only they are excluded.

## Tradeoffs and limits

- A refresh in the last second before a kill, which no periodic sync copied,
  is lost. The store keeps a refresh token the session has since spent, so
  the next session of the login may have to sign in again. The same holds for
  a save skipped at its bound.
- Syncs run every second (`loginInterval`, the helper's `LOGIN_INTERVAL`),
  and a sign-out must be seen twice on each side: by two syncs of the session
  that signed out before it writes the store, and by two syncs of each other
  session before it copies it in. A real sign-out therefore reaches other
  sessions two to four seconds after it is made, where a sign-in or refresh
  takes up to two. A sign-out in the second before a stop, or one followed by
  the harness's own exit within the second, is not shared.
- A store left marked by a writer that died stays as it is until a session
  writes a whole login. Sessions keep their own login meanwhile, and a session
  started then starts without one. One exception follows ADR 0049's
  whole-file rule: Copilot's `config.json` also holds settings, so a Copilot
  session with no login that changes a setting writes a whole config with no
  login, which replaces the store as any change does. Whether stock Copilot
  rewrites the file at start without a user action has not been checked.
- A supervisor or launch helper from the previous release sharing the store
  takes the same lock, but after an abandoned write it finds a marked store
  and copies it into its harness unchecked. That harness then cannot parse
  its login until a writer of this release replaces the store. That is what
  the previous release did with an abandoned partial store, and no worse than
  what it did with an abandoned empty one, which signed it out. It can only
  follow an abandoned write.
- Activation's first sync is bounded only while another holder keeps the
  lock. An `open` or `read` on a stalled volume is not bounded, there or in
  the stop's save: the stop returns at its bound and leaves the save running.
- A kill from outside the supervisor, such as the kernel's out-of-memory
  killer, is recorded only once the harness is reaped. A periodic sync in
  between still trusts the file. With the rules above, it shares neither an
  empty nor a cut-short file. It could share a whole login the kill left
  wrong, which needs a client that overwrites in place without truncating.
  The helper's interactive `--login` mode has the same gap, because Claude
  Code takes the terminal's signals directly.
- The terminal supervisor's reap kills a process group left behind by its
  leader without recording it, since it runs no `BeforeKill`. That happens
  only after the harness exited by itself, so its own writes were complete.
- The helper's stop, like the supervisor's, waits at most its bound for the
  periodic sync in flight and its own save together, and leaves a save stuck
  on a stalled volume running. `--sign-in-wait` sets the bound.
- On Kubernetes, a visit with a recorded exit whose capture still fails, for
  example because a Codex turn genuinely changed, fails on every retry. Docker
  keeps the visit's input instead. That fallback is not part of this decision.

## Revisit conditions

- The stock clients' login write patterns are checked and all truncate or
  rename: saving a whole login after a kill would then be safe.
- A supported harness rewrites its conversation file in place, or writes a
  row across lines: the capture rule no longer holds.
- Executors accept a new reply field: a skipped save could then be reported.
- A client's login stops being one JSON object, or `{}` becomes a valid
  login.
- Out-of-memory kills of harnesses become common enough that the outside-kill
  window matters.

## Links

- Implementation: [captured journals](../../internal/harness/stopped_journal.go),
  [sign-in sync](../../cmd/native-session/signin.go),
  [kill hook](../../internal/nativepty/supervisor.go),
  [supervisor](../../cmd/native-session/main.go),
  [Claude Code launch helper](../../internal/harness/launchfiles/claude-launch.py)
- Tests: `internal/harness/stopped_journal_test.go`,
  `cmd/native-session/signin_store_test.go`,
  `cmd/native-session/signin_signout_test.go`,
  `cmd/native-session/signin_kill_test.go`,
  `cmd/native-session/signin_stall_test.go`,
  `cmd/native-session/signin_forced_test.go`,
  `cmd/native-session/signin_log_test.go`,
  `cmd/native-session/signin_unfinished_test.go`,
  `internal/nativepty/before_kill_test.go`,
  `internal/harness/claude_launch_test.go`,
  `internal/harness/claude_login_test.go`
- References: [native launch and control](../reference/native-control.md),
  [harness sign-in](../reference/agentruntime.md#harness-sign-in)
- Related: [ADR 0008](0008-selected-native-and-git-checkpoints.md),
  [ADR 0011](0011-native-session-compatibility-and-completion.md),
  [ADR 0035](0035-bounded-graceful-stops-and-settled-input.md),
  [ADR 0049](0049-named-logins-share-a-live-store.md),
  [ADR 0054](0054-claude-code-agentruns-launch-through-a-helper.md),
  [ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md)
