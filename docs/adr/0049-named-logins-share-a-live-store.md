# 0049: Named logins share a live store

Status: accepted
Date: 2026-10-24

Extends [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md),
replacing how a harness's own sign-in is stored and reaches each session.

## Problem and constraints

ADR 0047 kept a harness's own sign-in outside its sessions: locally in one
file of a Docker volume, mounted at the login file's path; on Kubernetes in a
claim the supervisor copied into the session at launch and back at exit. Two
problems followed:

- On Kubernetes each session worked on a private copy. A refresh in one
  session didn't reach another, and the last to exit overwrote the store, so
  a session could be left holding a rotated, spent refresh token.
- Copilot replaces its `config.json` by rename, which a single-file mount
  refuses ([ADR 0048](0048-github-copilot-cli-is-a-managed-harness.md)), so
  the Docker mount couldn't serve it.

People also want more than one account per harness, such as work and personal.

Constraints: a login never enters a checkpoint, so a moved session signs in
again at its destination; Sproozi never copies a sign-in from the person's
machine; a read-only Secret holding a non-refreshing token keeps working;
whether a provider's terms allow a mechanism stays the person's choice.

## Decision

- **A login store is a directory** mounted at `/etc/sproozi/sign-in` on both
  backends: a Docker volume or a Kubernetes claim, holding the harness's login
  file.
- **The supervisor keeps the two copies in step while the harness runs.** It
  copies the store into the harness's home before launch, then every second
  compares both under an exclusive `flock` on the stored file. A change in the
  harness's file (a sign-in, refresh or sign-out) is written to the store in
  place; a change another session made to the store is written into the
  harness's file through a temporary file and rename. The harness's change
  wins when both changed, as the last write on one machine would. A final
  sync runs before the supervisor exits. Empty and missing both mean "not
  signed in". A store the supervisor can't write, such as a Secret, only flows
  in. A link may resolve only inside the sign-in directory, as a Secret's keys
  do, and the file is bounded at 1 MiB.
- **Logins have names.** `--login NAME` names one of a harness's logins;
  `default` is used when none is named. Names are per harness, at most 24
  lowercase letters, digits and hyphens, since they become volume and claim
  names. A session records its login, so `resume` and moves use the login of
  the same name at the destination.
- **Local:** one volume per installation, harness placement and login,
  `<network>-<placement>-login-<name>`. `sproozi init --harness H --sign-in
  --login NAME` records the login for the service; `sproozi run --login NAME`
  refuses a login that isn't set up, naming the ones that are, and refuses any
  login for a harness whose credential the gateway holds. `sproozi ls` shows a
  `LOGIN` column.
- **Kubernetes:** `clientConfig.signIn.claims` replaces `claimName`. Each
  login has the claim `sproozi-login-<harness>-<login>` in `sproozi-agents`.
  **The controller creates a missing claim** before the Pod, with the
  runtime's storage class and access mode, and never deletes it. A managed
  session mounts its registration's login; an AgentRun has no session and
  uses `default`.
- **Gateway-held keys and tokens stay one per harness.** Naming them would
  need a credential and a gateway listener per name.

## Evidence

- `cmd/native-session/signin_test.go`: two sessions sharing one store each
  survive a refresh by either (`r1`, `r2`, `r3` rotations); a third session
  starts with the current sign-in; a sign-out reaches the other session; two
  logins stay separate; a read-only store only flows in; a link out of the
  directory and an oversized file are refused; stop saves the last change once.
- `internal/runtime/docker/login_test.go`, `internal/session/home/login_test.go`,
  `internal/session/coordinator_test.go`, `internal/localsetup` and
  `internal/runtime/kubernetes` tests: per-login volumes and claims, a move
  keeping its login, refusals naming existing logins, claim creation once.
- Not yet verified: that stock Codex, Claude Code and Copilot re-read their
  login file when another process changes it, rather than keeping the token
  they loaded. A client that keeps its own copy can still fail a refresh after
  another session rotated the refresh token, as it would on one machine.

## Alternatives

- **Keep copy-in and write-back at exit.** Simplest, but sessions diverge and
  the last to exit wins.
- **Mount the login file directly**, as Docker did. Copilot can't use it, and
  on Kubernetes the session folder is restored after the Pod starts, so a
  mount there would break the restore's ownership checks.
- **Link the login file into the store.** Claude Code replaces the link with a
  regular file on refresh (ADR 0047), and Copilot replaces its file too, so
  the store would miss every refresh.
- **Import the person's existing logins.** Rejected: Sproozi never copies a
  sign-in from the person's machine, and each placement signs in fresh.
- **The remote executor creates claims.** The controller already writes the
  sandbox objects in `sproozi-agents` (ADR 0023) and creates the Pod that
  needs the claim; the executor would need new rights there.
- **Name gateway-held keys too.** Not needed for several accounts of a harness
  that signs in itself; revisit if people need several keys per harness.

## Consequences and limits

- A sign-in reaches the other sessions of its login within about a second.
- Sessions of one login on different Kubernetes nodes need a storage class
  that supports `ReadWriteMany`; the default `ReadWriteOnce` shares a claim
  only on one node.
- Deleting a login's volume or claim signs that login out everywhere.
- Docker no longer needs Engine 26 for `volume-subpath`.

Revisit if a client keeps its own copy of a refreshing token and loses it to
another session, if a provider offers per-session tokens, or if people need
several gateway-held keys per harness.

## Implementation

[sync](../../cmd/native-session/signin.go),
[login names](../../internal/harness/login.go),
[Docker volume](../../internal/runtime/docker/placement.go),
[Kubernetes claims](../../internal/runtime/kubernetes/sandbox.go),
[service](../../internal/session/home/service.go),
[local setup](../../internal/localsetup/install.go),
[AgentRuntime reference](../reference/agentruntime.md#harness-sign-in),
[local sessions](../guides/local-sessions.md).

## Correction: a stop never waits on the login store (2026-11-16)

Every sync took the store's `flock` blocking, with no bound. The final sync
runs in the stop, while the supervisor holds its action lock, so a peer
holding the lock on a stalled volume held the stop, every repeated stop and
the supervisor's exit for ever, past the bounds of
[ADR 0035](0035-bounded-graceful-stops-and-settled-input.md) and against
[ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md).

Each sync now tries the lock without blocking and gives up after five
seconds. The stop's save as a whole, including any periodic sync still in
flight, waits at most those five seconds; then the stop goes on, because the
harness is already stopped and its checkpoint matters more than the save.
The store keeps what it had, and the supervisor writes `Skipped saving the
harness's sign-in ...` to its standard error, which the container's output
keeps. Nothing new is reported to the placement: the stop's outcome does not
depend on the save, and a new reply field would need a change on the wire
that older executors would refuse. A missed save can leave the store with a
refresh token the stopped session has since spent, so the next session of
that login may have to sign in again. The first sync, which runs under the
action lock at activation, has the same bound and fails the activation with
`harness sign-in is unavailable`, as any other store error does. With these
five seconds a stop still fits inside the five minutes the control client
waits for its reply.

## Correction: only a whole login, and no sign-out from a stop (2026-11-16)

The sync copied the harness's login file to the store byte for byte, and an
empty file was a sign-out. Only Copilot is known to replace its file by
rename ([ADR 0048](0048-github-copilot-cli-is-a-managed-harness.md)); a
client that truncates and rewrites it leaves it empty and then cut short for
a moment. A kill landing
then, which Claude Code and Copilot meet on every stop and Codex at the quit
bound or the deadline, left a half-written login that the stop's final sync
pushed to every session of the login, or an empty one that signed them all
out. The periodic sync could read the same moment while the harness ran.

A login file is now copied to the store only when:

- **It is a whole login.** Every managed harness keeps its login as one JSON
  object: Codex's `auth.json`, Claude Code's `.credentials.json` and
  Copilot's `config.json` after its header of `//` comment lines. A file cut
  short does not parse; one rewritten in place over a longer file without
  truncating leaves trailing bytes and does not parse either. Any other
  content stays in the session, and the sync says so on standard error. A
  file left half written before launch is replaced by the store's login, or
  removed when the store has none.
- **A sign-out is made while the harness runs.** Empty and missing still mean
  "not signed in", and a sign-out the harness makes while it runs reaches the
  other sessions as before. Once a stop has gated input, or at the final
  sync, an empty or missing file is not copied out: a truncate-and-rewrite
  cut short by the coming kill looks exactly like a sign-out, so no harness
  lets the two be told apart, and the safe side is to keep the login. A
  sign-out in the second before a stop, or one the harness makes and then
  exits by itself within the second, therefore does not reach the other
  sessions.
- **The supervisor has not killed the harness.** It records the kill before
  sending it: the kill that ends Claude Code and Copilot, Codex's forced stop
  at the quit bound, a turnless stop, and a stop at the deadline, on `SIGTERM`
  or after a failed stop. A passed deadline counts the same, and so does an
  exit by a signal from anyone else once the supervisor sees it. From then on nothing from the file reaches the
  store, not even a whole login. Each sync checks the record after reading
  the file, so a read that saw what the kill left always sees the record. A
  sync that had nothing to save is silent; one that leaves a change unsaved
  logs `Skipped saving the harness's sign-in ...`. A refresh
  in the last second before the kill, which the periodic sync had not yet
  copied, is therefore lost, and the next session of the login may have to
  sign in again. The alternative, saving a whole login after a kill, relies
  on every client truncating or renaming when it rewrites, which has not been
  checked against the stock clients.

The AgentRun launch helper for Claude Code (`claude-launch.py`) applies the
same three rules: it records a kill before it forwards a signal to Claude
Code, and when a signal ended it. It still takes the store's lock without a
bound; a stalled store holds an AgentRun until its Pod is deleted, not a
supervisor's stop.

A periodic sync can still read an empty file in the instant a running
harness truncates it before rewriting, and share it as a sign-out for about
a second, until the next sync shares the rewritten login. Telling those
apart would need the empty file to persist over more than one sync. Revisit
if a client is found to rewrite its login in place without truncating, or
one signs out by writing an empty file during shutdown.

Evidence: `cmd/native-session/signin_forced_test.go` (a half-written login,
Copilot's commented `config.json`, a sign-out while stopping, a refresh while
stopping, a kill and a deadline, and Claude Code and Copilot stops whose kill
left an empty file) and `internal/harness/claude_launch_test.go` (a login
left cut short or removed at exit).
