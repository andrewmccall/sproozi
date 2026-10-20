# One user profile, mounted live in Docker and stored by digest for Kubernetes

Status: accepted
Date: 2026-10-20

## Problem and constraints

Milestone M2 of the [managed harness configuration
PRD](../explanations/future-architecture/managed-harness-configuration-and-trust.md)
lets users bring their own instructions and skills into managed sessions. The
PRD proposes global and project layers, import commands and per-execution
configuration digests. For the current demo the owner chose less: one default
profile, edited like a home directory, with all Sproozi files under
`~/.sproozi`.

Constraints found in the code:

- Approved instructions are bound by digest from template to registry to Codex
  continuity ([ADR 0025](0025-managed-harness-integration-and-diagnose.md)).
  User text must not change that digest.
- Docker agent containers had a verified mount contract: the workspace volume
  and nothing else.
- Every route to Kubernetes starts at the home: `move` from Docker, or `run`
  from the CLI. `resume` is local only.
- Launch metadata is bounded well below a useful profile size: 64 KiB in
  placement storage, 32 KiB on the work owner's endpoint.
- Agent Pods run in `sproozi-agents`; the executor may create ConfigMaps only in
  its run namespace, and a Pod can mount ConfigMaps only from its own
  namespace. The controller already builds each run's immutable contract
  ConfigMap in `sproozi-agents` and deletes it on release.
- With every setting source disabled, Claude Code loads skills only from
  `--plugin-dir` plugins; Codex loads `$CODEX_HOME/skills` and one
  `$CODEX_HOME/AGENTS.md`.

## Decision

**One profile.** `~/.sproozi/profile`, beside `~/.sproozi/config.json`, holds
an optional `AGENTS.md` and `skills/<name>/SKILL.md` with supporting files.
The home takes its location from the configuration file's location, never from
its contents. `sproozi init` creates it. Sproozi never reads `~/.codex` or
`~/.claude`.

**One set of rules.** `internal/agentprofile` reads the live folder or a
reproducible tar.gz archive with the same validation: regular files only, no
`sproozi-access` skill, 512 files, 4 MiB, 64 KiB of instructions, and a
compressed archive of at most 640 KiB so it fits a ConfigMap. Its digest is
`sha256:` over the archive.

**Rendering.** Profile instructions follow the Sproozi instructions: in
`$CODEX_HOME/AGENTS.md` for Codex, in `--append-system-prompt` for Claude Code.
Profile skills sit beside `sproozi-access` for Codex and form a `profile`
plugin for Claude Code. Each launch rebuilds them. Approved instructions and
their digest are unchanged.

**Docker sees the live folder.** The agent container mounts the profile folder
read-only at `/etc/sproozi/profile`. Adoption accepts exactly the workspace
volume and that mount.

**Kubernetes visits get the archive taken when the visit was launched.** A move
archives the profile into the home's content-addressed store as it creates the
launch and names it by digest; `run` sends the archive with the submission.
The executor verifies it and stores it once as an immutable ConfigMap named by
digest in its run namespace. The registration records the digest. The
controller re-reads and re-verifies that object, copies it into the run's
immutable contract and mounts it for the agent only when the registration
names a profile. Retries and recovery reuse the journaled launch and so the
same archive.

**Executors accept profiles within limits.** A profile is guidance, like the
Sproozi integration; the gateway enforces policy whatever it says. Executors
need no allowlist.

**`sproozi doctor`** validates the configuration and profile, warns past three
quarters of the archive limit and fails above it. `run` and `move` refuse a
profile over the limit.

**Naming.** "Profile" now means the user's profile. The `session-profile`
program is a remote executor in documentation, matching `--executor`.

## Alternatives

- **Global and project layers with import commands (the PRD's M2).** Deferred
  by the owner as more than a demo needs.
- **Several named profiles selected by `--profile`, project or workspace.**
  Rejected for now as additional concepts without a demonstrated need.
- **A snapshot for Docker too, refreshed by `resume --update-profile`.** The
  owner chose the live folder for Docker. With every Kubernetes visit starting
  from the home, the flag had no remaining case, so it was not built.
- **Carry the profile inside the checkpoint artifact.** Moves would get it
  without new storage, but `run` has no checkpoint and the checkpoint is
  defined as work, not configuration.
- **An image or sidecar per profile.** Removes the size limit but needs a
  registry and builds on the home. Kept as the next step if profiles outgrow a
  ConfigMap.
- **Profile ConfigMaps in `sproozi-agents`.** The executor has no rights there,
  and granting them would widen its access to the agent namespace.

## Tradeoffs and consequences

- Profile changes reach new conversations. A continued conversation keeps
  what its first launch showed the model: Codex in its history, Claude Code in
  its saved system prompt and skill listing. Each launch still installs the
  current files; session mobility checks the rendered profile per visit.
- A compressed profile over 640 KiB cannot go to Kubernetes.
- Stored profiles, like registrations, are never deleted. Each distinct profile
  sent to an executor adds one ConfigMap.
- The digest is part of a visit's launch, so editing the profile between
  retries of the same `run` operation is a conflict.
- On Linux hosts the profile must be readable by container UID 1000; `doctor`
  warns otherwise. Docker must be able to bind-mount the folder.
- Windows hosts do not mount the profile into Docker: Docker Desktop may report
  a translated bind source that exact adoption would refuse, and this was not
  verified. Their moves to Kubernetes still carry it.
- The agent can read its profile but not change the source. As in ADR 0025, it
  can edit its own copies inside the session until the next launch.
- Default configuration moved from the platform configuration directory to
  `~/.sproozi/config.json`. Existing users pass `--config` or move the file.

## Revisit conditions

- Profiles that need more than a ConfigMap holds: add an image or sidecar
  carrier.
- More than one profile, or project-scoped configuration: revisit selection and
  layering from the PRD.
- Stored profile growth that matters to an executor: add garbage collection
  keyed on registrations that still name each digest.
- A harness that changes skill or instruction discovery: the stock-client tests
  in `cmd/native-session` fail first.

## Links

- Reference: [Profile](../reference/profile.md)
- Implementation: `internal/agentprofile`, `cmd/native-session/managed.go`,
  `internal/runtime/docker/managed_engine.go`,
  `internal/runtime/kubernetes/registry/profile.go`,
  `internal/runtime/kubernetes/managed.go`, `internal/session/store.go`,
  `internal/work/owner.go`, `cmd/sproozi/doctor.go`
