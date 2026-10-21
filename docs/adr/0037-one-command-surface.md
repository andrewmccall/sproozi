# 0037: One command surface for local and remote sessions

Status: accepted
Date: 2026-10-20

## Problem and constraints

Five launch testers reviewed the whole CLI and raised the same problems. Help
and errors sent newcomers to a bare `init` that wrote a remote-only starter.
`start` needed `--workspace "$PWD" --harness codex`, and `start .` failed with a
UUID error. `run` was remote-only, so Docker users read it as the local
command. Sessions needed full UUIDs. `status` printed kilobytes of one-line
JSON. One machine had three names (`docker`, `home`, `local.name`), and help
used `home`, `placement`, `execution`, `visit` and `journaled operation`.
`sessions` started the service, undoing `service stop`. Every error looked the
same whether repeating it could help or not.

Sproozi is unreleased, so the owner asked for one coherent surface with no
aliases or compatibility shims. The owner's decisions: one name for this
machine, `local`; `run` for both local and remote work; `init` sets up local
Docker; the default root is `~/.sproozi`, chosen by `--root` or `SPROOZI_ROOT`;
remote commands name a cluster with a kubeconfig context, designed separately
in ADR 0036; no command
returns work to the user's repository yet; and there is no CLI installer for
Kubernetes.

## Decision

### Commands

Help groups the commands as Sessions (`run`, `sessions`/`ls`, `status`,
`attach`, `stop`, `resume`, `cancel`, `rm`), Remote (`move`, `pull`) and Setup
(`init`, `doctor`, `service`, `credentials`, `images`, `certificates`,
`version`). `recover` and `service run` work but are not listed: errors name
`recover` when it applies, and `service start` runs the service for you.

- `run [PATH] [-- AGENT ARGS]` starts a local session on `PATH`, default `.`,
  with the configured agent; `--remote` submits a task to a cluster instead,
  and `--context` or `--namespace` imply it.
- `move ID --local|--remote [--context] [--namespace]` only relocates. The
  service accepts `local` as the name of its own placement, whatever
  `local.name` is.
- `pull ID [--context]` imports a session; there is no `--from`.
- `rm ID` deletes a stopped or cancelled session.
- `cancel` succeeds on a cancelled session.
- `credentials set AGENT|PROVIDER` and `credentials list` accept `codex` and
  `claude-code` as well as `openai` and `anthropic`.

### Conventions

- **IDs.** Any unique prefix of a session ID works; lists show 8 characters.
  The CLI resolves a prefix from the owner's own list.
- **Output.** Text by default; `--json` everywhere except `attach` and `help`.
  Round 1 printed JSON whenever standard output was not a terminal; that hid
  behaviour behind a property users cannot see, so scripts now pass `--json`.
- **Exit status.** 2 for a command line that cannot run as written, 1 for any
  other failure. Errors start `error:`; a misspelt command, verb or flag gets
  `did you mean`. Flags are validated before anything is read or sent.
- **Retries.** A retry hint appears only when the outcome is unknown. A
  rejection, an unknown session or a stopped service has none.
- **Plain names.** This machine is `local` in flags and output (`executor`,
  `ownership` and `placement` values); the CLI maps the service's internal
  names. The service is "the Sproozi service" in messages.
- **Location.** `--root`, else `SPROOZI_ROOT`, else the root `init --root`
  recorded, else `~/.sproozi`. `--config`, `--socket`, `--id`, `--operation`
  and the revision flags stay for scripts and hand-written administrator
  setups but are hidden from help.
- **Side effects.** Session commands start a stopped service, except
  `sessions`, which never does.

### `rm`

`rm` refuses a session with a running agent, an unfinished operation or
unsettled usage, and one still being pulled. Otherwise, holding the
coordinator exclusively, it deletes the catalogue entry, the executions'
journals, and every content-addressed workspace, checkpoint and profile that
no remaining journal or catalogue entry names. It waits at most five seconds
for operations in progress, so it never stalls the service behind a long stop.
It keeps accounting and grant records and the Docker placement's record of
each finished visit, which are small and are evidence for budgets and fencing.

### Remote seam

Remote commands reach a cluster through one file, `cmd/sproozi/remote.go`:
`resolveRemote(ctx, config, context, namespace)` and `remoteTargets(ctx,
config)`. Until ADR 0036's discovery is wired in, a context name selects a
configured `remote` or `workSources` entry and kubeconfig files and
`--namespace` are refused.

## Alternatives

- **Keep `start` beside a remote-only `run`.** It matched the code's two
  owners but not how users think: every tester typed `run` for local work.
- **`--to NAME` for every move.** It needed users to know placement names and
  reserved names; `--local` and `--remote` say where the session goes.
- **JSON when not a terminal.** It saved scripts a flag but made output depend
  on invisible state; it also surprised testers who piped `ls` to `grep`.
- **A garbage collector for retained files.** `rm` gives the user an explicit,
  immediate way to reclaim space without guessing what may be deleted.
- **Delete placement records and inputs in `rm`.** They are fencing evidence
  owned by the placement; deleting them needs a placement contract change.
- **Keep the platform starter configuration for hand-written setups.**
  `init` no longer writes one; administrators write `config.json` from the
  documented example, and `SPROOZI_ROOT` points every command at it.

## Tradeoffs and consequences

- Scripts must pass `--json`; the verification scripts do.
- Help hides flags that the hand-written administrator setup still needs; the
  command reference lists them.
- After `rm`, each finished visit's Docker placement record and its copy of
  the visit's input remain in `state/docker` until the installation is
  removed.
- Prefix resolution costs one extra listing request.
- On Windows, `init` creates only the private root and explains that the
  configuration is written by hand.

## Revisit conditions

- ADR 0036's context discovery lands: replace the seam's configured-entry
  lookup and accept kubeconfig files and `--namespace`.
- Users need placement inputs reclaimed: give placements a release contract
  and have `rm` call it.
- A command returning work to the user's repository is designed; see
  [returning a session's work](../explanations/future-architecture/returning-work-to-the-user.md).

## Links

- [Command reference](../reference/session-commands.md) and
  [local sessions guide](../guides/local-sessions.md)
- `cmd/sproozi`, `internal/session/home/remove.go`, `internal/session/remove.go`
- [ADR 0029](0029-local-provisioning-and-lifecycle.md) for local setup and its
  amendments
