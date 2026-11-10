# 0054: Claude Code AgentRuns launch through a helper

Status: accepted
Date: 2026-11-10

## Problem and constraints

The stock Claude Code runtime replaced its launch shell with one
`claude --print`. Two things were missing for unattended AgentRuns, found while
running Claude Code SRE triage and fix runs for a persistent Hermes assistant
on a home cluster:

- **No answer.** The controller keeps a run's answer only from a
  `sproozi.run-result/v1` envelope in the termination file. Hermes and OpenCode
  have launch helpers that write it; Claude Code had none, so its runs succeeded
  with no result and the task service returned nothing to the assistant.
  Operators had to add their own wrapper to each runtime.
- **No usable login store.** `clientConfig.signIn.claims` keeps a harness's
  login file in a PersistentVolumeClaim, and the managed-session supervisor
  keeps the harness's own copy in step with it while it runs
  ([ADR 0049](0049-named-logins-share-a-live-store.md)). An AgentRun has no
  supervisor. Claude Code's subscription sign-in refreshes and rotates its
  tokens, so a run that reads the store once and never writes back leaves the
  next run a stale refresh token. A fixed `CLAUDE_CODE_OAUTH_TOKEN` Secret works
  but doesn't keep Claude Code's own login, as a person's `~/.claude` does.

The agent loop stays the stock client's, as for the Hermes and OpenCode helpers ([CLI harnesses](../reference/harnesses.md)):
a helper may prepare, launch and publish, never decide task success from text.

## Decision

The `claude-code` harness ships `claude-launch.py` in its immutable contract
(`LaunchFiles`), and the stock example launches it.
The helper:

1. Prepares a fresh native home (`$HOME/.claude`) and fixed environment, and
   runs one stock `claude --print --output-format json` with the same flags the
   example used, the approved instructions appended to the system prompt and
   `request.json` as the prompt.
2. Writes Claude Code's standard output to a file, then accepts exactly one
   `result` record with `subtype: success` and `is_error: false`, publishing its
   `result` text in the envelope with the Hermes helper's truncation. Any other
   outcome, a nonzero exit or a signal fails the container with no result.
3. When `/etc/sproozi/sign-in` is mounted, gives Claude Code no key placeholder
   and runs the supervisor's two-way login sync between the store's
   `.credentials.json` and the native file: under the store's lock, once before
   launch, every second, and once after exit. A read-only store, such as a
   Secret, only flows in. Without a sign-in it sets the key placeholder for a
   gateway-held key.
4. With `--login`, runs Claude Code interactively under the same sync, so an
   operator can sign a new login in once from `kubectl exec` in a waiting
   AgentRun. A shell from exec lacks the runtime command's proxy, so the helper
   builds it from the run's gateway token when it is missing.

## Alternatives

- **Each operator wraps the stock command.** Rejected: every installation
  re-derives the envelope and login sync, and gets the edge cases (truncation,
  failed records, lock order) wrong in different ways.
- **Run the managed-session supervisor for AgentRuns.** Rejected: it is an
  interactive terminal server with checkpoints and portable state; a one-shot
  run needs none of that.
- **Point `CLAUDE_CONFIG_DIR` at the login claim.** Rejected: every run's
  settings, projects and caches would accumulate in the shared store, and one
  run could leave files that change the next run's behaviour.
- **Only a fixed `CLAUDE_CODE_OAUTH_TOKEN` Secret.** Still supported through
  `signIn.secretName` and `env`, but it doesn't refresh and isn't Claude Code's
  own login.

## Consequences

- Claude Code AgentRun results now reach the task service and any AgentRun
  reader. Result text is untrusted and may contain scoped tool data, as for
  other helpers.
- The image needs Python 3. The release's native image has it.
- Concurrent runs of one login share its store as processes on one machine
  share `~/.claude`: the latest native change wins, and two runs refreshing at
  the same moment can still race, as in managed sessions.
- The agent can read its own sign-in, as ADR 0047 records for every harness
  sign-in.

Revisit if Claude Code gains a native result file or a credentials helper that
reads a store directly, or if AgentRuns gain a supervisor.

## Links

- [CLI harnesses](../reference/harnesses.md)
- [AgentRuntime harness sign-in](../reference/agentruntime.md#harness-sign-in)
- `internal/harness/launchfiles/claude-launch.py`, `internal/harness/claude_launch_test.go`
- [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md), [ADR 0049](0049-named-logins-share-a-live-store.md)
