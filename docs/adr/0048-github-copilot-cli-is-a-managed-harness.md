# 0048: GitHub Copilot CLI is a managed harness

Status: accepted
Date: 2026-10-24

Supersedes [ADR 0046](0046-github-copilot-cli-harness-in-byok-mode.md)'s
"an AgentRun harness, not managed" and "only the Kind client image installs
it". Its bring-your-own-key route, MCP loading and instructions file stay.

## Problem and constraints

People asked for Copilot as a first-class agent beside Codex and Claude Code:
`sproozi init` and `sproozi run` on this machine, sessions that move between
Docker and Kubernetes, and every credential mechanism of
[ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md). ADR 0046
had deferred this for want of a request; ADR 0047 then routed Copilot's own
GitHub sign-in through the gateway, which removed its other reason.

A managed harness needs a native state format, a completed-turn observer and a
checkpoint ([ADR 0011](0011-native-session-compatibility-and-completion.md)),
and release users must not have to build an image.

Pinned and tested: `@github/copilot` 1.0.102.

## Decision

- **A managed kind, `copilot`.** The registry entry sets `Native`,
  `NativeDirectory` `copilot` and `SignInFile` `config.json`. Its model
  protocol with a key is OpenAI; with a gateway-held token or its own sign-in
  it reaches GitHub's Copilot service (`SignInProvider`).
- **The supervisor names each session.** A new visit passes
  `--session-id <uuid>` that the supervisor chose, and a resumed one passes
  the session's ID, so the supervisor never adopts a session Copilot chose,
  such as one opened with `/new`.
- **Completion comes from the journal.** Copilot writes each session's events
  to `$COPILOT_HOME/session-state/<id>/events.jsonl`, live. A turn has
  completed when the main agent's last `assistant.turn_end` after the last
  prompt follows an answer that requested no tools, every tool execution has
  finished, and no `session.error` or `abort` follows. Each event must name an
  earlier event as its parent, and the journal must start the session. Other
  event types stay opaque history. Subagent events carry `parentToolCallId`
  and don't count. Terminal input must settle first, so a prompt sent just
  before a stop reaches the journal.
- **The checkpoint carries only the journal.** Copilot rebuilds the rest of
  the session directory and its `session-store.db` index on resume.
- **The supervisor stops Copilot**, as it stops Claude Code. The journal
  proves completion, as Codex's rollout does, so a lost stop reply is
  resolved by the stopped container (`StateProvesCompletion`).
- **Launch files.** The workspace is trusted in `config.json`'s
  `trustedFolders`, merged into a restored sign-in, since Copilot keeps both
  in that file; `--trust-folder` works only in prompt mode. Instructions and
  the profile go to `copilot-instructions.md`, skills to
  `$COPILOT_HOME/skills`, and the control and named MCP connections to
  `--additional-mcp-config`. `settings.json` sets `storeTokenPlaintext`, so
  `/login` stores its token in `config.json` without asking. Flags Sproozi
  owns, such as `--allow-all`, `--resume` and `--additional-mcp-config`, are
  refused as harness arguments.
- **One provider for each mechanism.** A gateway-held key runs Copilot
  offline in bring-your-own-key mode on the OpenAI route. A gateway-held
  GitHub token (`--token` locally, `nativeSession.signInToken` on Kubernetes)
  gives Copilot a placeholder `COPILOT_GITHUB_TOKEN` that the gateway
  replaces on its plan's hosts. Its own sign-in gets no GitHub placeholder,
  because Copilot prefers `GH_TOKEN`, and the token `gh` stores, to its
  `/login`; the agent's shell gets `GH_TOKEN` through `BASH_ENV`. The
  gateway's sign-in relay for GitHub's device flow is a path route
  (`/login/`) on `github.com`, beside the GitHub capability's Git route.
- **The released image installs Copilot** and unpacks its app once
  (`hack/clients/install.sh`). A sandbox home is a 16 MiB `noexec` tmpfs
  where Copilot can't unpack 170 MB or run it, so the supervisor links
  `~/.cache/copilot/pkg` to the image's copy.

## Evidence

With 1.0.102, against scripted providers:

- `--session-id` sets a new session's ID and resumes an existing one.
- A session copied as `events.jsonl` alone into a fresh `COPILOT_HOME`
  resumed, with the earlier turns in the next model request, and Copilot
  recreated `workspace.yaml`, its subdirectories and `session-store.db`.
- In interactive mode, `assistant.turn_end` reaches the journal while Copilot
  runs. A turn that requests a tool writes `assistant.turn_end` before the
  tool runs, then a new turn; a refused model call writes `turn_end` then
  `session.error`. The published schema
  (`schemas/session-events.schema.json`) marks `session.idle` ephemeral, so
  it is never in the journal.
- Without trust in `config.json`, an interactive session waits at a folder
  trust dialog.
- With `config.json` mounted as a single file, Copilot exits 1 with no
  output: it replaces the file, which a file mount refuses. This shaped the
  login store in [ADR 0049](0049-named-logins-share-a-live-store.md).
- Without a keychain, Copilot stores its `/login` token in `config.json`
  (`copilotTokens`, `loggedInUsers`) after a consent prompt that
  `storeTokenPlaintext` answers. With no token in its environment it runs
  `gh auth token` and uses that.
- Copilot loads skills from `$COPILOT_HOME/skills/<name>/SKILL.md` and adds
  `copilot-instructions.md` to its system prompt.
- Journal fixtures in `internal/harness/testdata/copilot` come from stock
  1.0.102 in a Linux container at `/workspace/session/workspace`.

## Alternatives

- **Keep Copilot an AgentRun harness.** No local sessions or movement, which
  is what was asked for.
- **Observe completion with Copilot's hooks.** Copilot has hooks, but the
  journal already records every transition the observer needs, live, and a
  journal check also validates the checkpoint, as Codex's does.
- **Checkpoint the whole session directory.** More bytes and files to
  validate; Copilot rebuilds them from the journal.
- **Trust the workspace with `--trust-folder`.** Prompt mode only.
- **Copilot's own sign-in with `GH_TOKEN` set.** Copilot would use the
  placeholder and never its `/login`.

## Consequences and limits

- Copilot sessions use the OpenAI route by default, which needs an OpenAI key,
  not a Copilot plan. A plan needs `--token` or `--sign-in`.
- In a session that signs in itself, `gh` works only through the agent's
  shell, which reads `BASH_ENV`.
- `/login` through the gateway's device-flow relay is untested with a real
  GitHub account. Copilot's plan hosts are routed as ADR 0047 recorded.
- Copilot releases often. 1.0.103 removed `--no-remote`; v0.2.1 pins 1.0.103
  and drops the flag from the supervisor and the AgentRun example
  ([ADR 0050](0050-prebuilt-cli-and-explicit-image-builds.md)).
- Copilot models on `/chat/completions` stream, which the gateway refuses
  (ADR 0047).

Revisit when Copilot's journal format or `--session-id` changes, when Copilot
can trust a folder in interactive mode without `config.json`, or when it
gains a setting to ignore `GH_TOKEN` and `gh`.

## Implementation

[registry](../../internal/harness/registry.go),
[journal and checkpoint](../../internal/harness/copilot_state.go),
[supervisor adapter](../../cmd/native-session/copilot.go),
[local gateway routes](../../internal/gateway/docker/gateway.go),
[provider table](../../internal/endpoints/model/providers.go),
[local setup](../../internal/localsetup/install.go),
[client install](../../hack/clients/install.sh) and the
[harness reference](../reference/harnesses.md#managed-interactive-sessions).
