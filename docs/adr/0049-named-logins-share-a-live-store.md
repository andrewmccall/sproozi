# 0049: Named logins share a live store

Status: accepted
Date: 2026-10-24

Extends [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md),
replacing how a harness's own sign-in is stored and reaches each session.

Updated 2026-11-24: this record is the one place that decides how a login is
named, stored, shared, refreshed and saved. ADR 0047's section on where a
sign-in lives was removed, and the parts of it that still held moved here, each
with a dated line; ADR 0047 keeps whose credentials reach the provider and how
the gateway forwards them.

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
  Corrected 2026-11-16: the lock's wait and the final sync are bounded, and
  only a whole login, or a sign-out two syncs see while the harness runs,
  is copied either way; nothing from a killed harness
  ([ADR 0061](0061-a-stop-survives-an-interrupted-write.md)).
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

### How a login store is prepared and delivered

Moved 2026-11-24 from ADR 0047's "Where a harness's own sign-in lives",
unchanged but for its heading, one connecting sentence and the dated
corrections below.

- The helper image's `checkpoint init-login`, run as root
  with only `CAP_CHOWN` and no network, creates an empty mode-0600 file owned by
  uid 1000 and leaves the volume's directory with root, so later runs can
  inspect it.

  Corrected 2026-11-24: the directory stays with root at mode 0711 and the
  agent mounts it as the login store, not one file of it
  (`initLogin` in `cmd/checkpoint/login.go`, `EnsureLoginVolume` in
  `internal/runtime/docker/managed_engine.go`).
- A Secret holds a credential the person obtained through
  the provider's own flow, such as `claude setup-token`; the controller mounts it
  read-only at `/etc/sproozi/sign-in/<key>` and, with `env`, sets it as that
  variable.

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
- Claude Code 2.1.302: a linked `.credentials.json` is replaced by a regular
  file on refresh, so a link into a volume loses every refresh; a mounted
  file is written in place; an empty file reads as "Not logged in"; and every
  refresh returns a new refresh token. (Moved 2026-11-24 from ADR 0047's
  Evidence, unchanged.)
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
  regular file on refresh (see Evidence), and Copilot replaces its file too, so
  the store would miss every refresh.
- **Import the person's existing logins.** Rejected: Sproozi never copies a
  sign-in from the person's machine, and each placement signs in fresh.
- **The remote executor creates claims.** The controller already writes the
  sandbox objects in `sproozi-agents` (ADR 0023) and creates the Pod that
  needs the claim; the executor would need new rights there.
- **Persist the whole harness home.** Simpler mounts, but it carries hooks,
  settings and instructions from one session into the next. (Moved
  2026-11-24 from ADR 0047's alternatives, unchanged.)
- **The first Kubernetes run writes its login into a ConfigMap.** Sandboxes
  have no Kubernetes credentials, by design, and ConfigMaps aren't for
  secrets. (Moved 2026-11-24 from ADR 0047's alternatives, unchanged.)
- **Name gateway-held keys too.** Not needed for several accounts of a harness
  that signs in itself; revisit if people need several keys per harness.

## Consequences and limits

- A sign-in reaches the other sessions of its login within about a second.
  Corrected 2026-11-16: within about two seconds, and a sign-out within
  about four ([ADR 0061](0061-a-stop-survives-an-interrupted-write.md)).
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
[Docker login volume and helper](../../internal/runtime/docker/managed_engine.go),
[login file preparation](../../cmd/checkpoint/login.go),
[Kubernetes claims](../../internal/runtime/kubernetes/sandbox.go),
[service](../../internal/session/home/service.go),
[local setup](../../internal/localsetup/install.go),
[AgentRuntime reference](../reference/agentruntime.md#harness-sign-in),
[local sessions](../guides/local-sessions.md).
