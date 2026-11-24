# 0048: GitHub Copilot CLI is a managed harness

Status: accepted
Date: 2026-10-24

Copilot first ran only as an AgentRun harness in bring-your-own-key mode, in
a Kind client image (ADR 0046, removed). This record makes it a managed harness
in the released image. What the earlier record decided and still holds is under
"AgentRun harness and bring-your-own-key mode" below. This record is also the
one place for Copilot's account namespaces and their verification
("Account namespaces and policy verification", moved from ADR 0053, which was
removed) and for dropping `--no-remote` ("Removing `--no-remote`", moved from
ADR 0050).

## Problem and constraints

People asked for Copilot as a first-class agent beside Codex and Claude Code:
`sproozi init` and `sproozi run` on this machine, sessions that move between
Docker and Kubernetes, and every credential mechanism of
[ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md). The
AgentRun harness had deferred this for want of a request; ADR 0047 then routed Copilot's own
GitHub sign-in through the gateway, which removed its other reason.

A managed harness needs a native state format, a completed-turn observer and a
checkpoint ([ADR 0011](0011-native-session-compatibility-and-completion.md)),
and release users must not have to build an image.

Pinned and tested: `@github/copilot` 1.0.102.

Corrected 2026-11-16: 1.0.102 was the pin when this record was written. The
pinned version is in `hack/clients/copilot/package.json`.

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

## AgentRun harness and bring-your-own-key mode

Corrected 2026-11-15: moved here from ADR 0046, which was removed. The
decisions below came first, for the AgentRun harness, and still hold. The
example is `examples/harnesses/agentruntime-copilot.yaml`; the managed
adapter is `cmd/native-session/copilot.go`.

- **The exit status is the outcome; an AgentRun has no launch helper.** Copilot
  exits 1 after a failed or refused model call and 0 after a completed turn, so
  the example `exec`s it, and no completion adapter or
  launch helper builds the instructions: four lines of shell write the file.
- **Instructions through `$COPILOT_HOME/copilot-instructions.md`.** Copilot has
  no system prompt flag but adds that file to its system prompt. The example
  writes the approved instructions and both integration files there and
  passes `request.json` as the prompt. `--no-custom-instructions` would also
  drop that file, so Sproozi doesn't pass it, and Copilot still loads workspace
  instruction files such as `AGENTS.md`, as Claude Code loads `CLAUDE.md`. A
  repository's own files reach the system prompt after the Sproozi file: they
  can contradict it but can't remove it.
- **Bring-your-own-key mode on the OpenAI Responses route.** The environment is
  `COPILOT_PROVIDER_BASE_URL=https://api.openai.com/v1`,
  `COPILOT_PROVIDER_TYPE=openai`, `COPILOT_PROVIDER_WIRE_API=responses` and a
  placeholder `COPILOT_PROVIDER_API_KEY`; the gateway supplies the real key, as
  for OpenCode, so no GitHub token enters the sandbox. The supervisor sets the
  same variables for a gateway-held key. Anthropic Messages through
  `COPILOT_PROVIDER_TYPE=anthropic` was not chosen as the default: the gateway
  has that route, but OpenAI matches OpenCode's, and an administrator can switch
  the variables.
- **Offline mode.** `COPILOT_OFFLINE=true` stops GitHub sign-in, telemetry, the
  built-in GitHub MCP server and auto-update, and `--disable-builtin-mcps`
  disables the built-in MCP servers. Remote MCP servers still load in offline
  mode.
- **Copilot's TLS stack refuses a CA certificate served as a leaf**
  (`CaUsedAsEndEntity`). The gateway serves leaves signed by its inspection CA,
  and the `harness-process` fixture does the same.

Revisit the instructions rule if Copilot adds a system prompt flag or a way to
disable workspace instructions alone.

Corrected 2026-11-24: the example writes the instructions file with one group of
six `cat` and `printf` commands (`examples/harnesses/agentruntime-copilot.yaml`),
not four lines.

Corrected 2026-11-16: the example `exec`s Copilot directly, but Claude Code's
does not, as this section first said. Claude Code AgentRuns launch through a
helper (ADR 0054; `examples/harnesses/agentruntime-claude-code.yaml`).

## Account namespaces and policy verification

Moved 2026-11-24 from ADR 0053, which was removed (it had absorbed ADR 0052,
also removed). The text is unchanged but for its headings.

### Problem and constraints

GitHub-backed Copilot reads account metadata under both `/copilot_internal/`
and `/copilot/` on `api.github.com`. The gateway mounted only the former.
In particular, `GET /copilot/mcp_registry` could not reach GitHub.
ADR 0052 (removed) had allowed a query only on the separate managed-settings
endpoint, without proving this registry-policy route.

Hosted Copilot E2E used OpenAI bring-your-own-key mode. The manual GitHub
process fixture represented an individual account, which could complete a
model/tool turn without requesting the registry policy. Its positive
managed-settings counter did not prove registry routing.

### Decision

For GitHub-backed Copilot, relay GET requests under both account namespaces,
including queries. Keep the fixed `api.github.com` origin, existing credential
mechanism, request/response bounds, no redirects and no-store responses.
Other methods, unrelated GitHub API paths and noncanonical paths are refused.
Other providers retain exact method/path rules. OpenAI-key Copilot does not
gain GitHub account routes.

GitHub continues to decide account permissions and MCP policy. Forward its
successes and refusals unchanged. The gateway does not invent a permissive
policy or log queries, credentials or policy bodies.

Docker and Kubernetes mount the same two prefixes from the provider table.
Tests use literal request paths independently of that table and verify that
the selected relay forwards the request and preserves the provider response.

PR/main CI and the latest-client canary run the GitHub-authenticated process
verifier with the relevant lockfile client. A separate explicit request to
`/copilot/mcp_registry` traverses the same authenticated TLS proxy and must
return the fixture policy. This probe is recorded separately from the stock
client's tool turn. It cannot be replaced by a model-completion result or a
counter for another endpoint. The fixture also rejects unmodeled account
requests so a newly exercised client path needs review.

Corrected 2026-11-15: moved here from ADR 0052, which was removed. Relay
refusals report the request's shape and never its content. The denial
reason reads "the gateway relays only the provider's sign-in and account
requests (method GET; query parameters present: true)", naming a recognized
HTTP method or "unrecognized" for any other, because an unrecognized method
can contain arbitrary text, and whether a query is present. It never records
query names, query values, headers or bodies, which can carry credentials
(`internal/endpoints/model/signin.go`). Presence diagnoses a refused route
without exposing content, but cannot identify a new parameter: capture new
request shapes with an isolated fixture client before extending a route.
Forwarding
queries only on fixed, named routes, rather than on every account route or by
validating today's three client metadata keys, keeps the relay from breaking
whenever the provider adds a parameter and from extending access without an
observed need.

### Alternatives and tradeoffs

Adding only one exact endpoint would fix this report but retain a fragile
inventory of Copilot's changing read-only account APIs. The administrator
approved both account namespaces. Allowing the whole GitHub API or arbitrary
write methods would cross that scope and is rejected.

The individual fixture does not reproduce an organization's live policy
selection. The explicit probe proves gateway routing and response fidelity;
the stock process proves the exercised signed-in model/MCP flow. Neither
claims a real subscription or every enterprise setting. Handler tests also
preserve upstream 403 and 404 responses.

### Verification and revisit conditions

Revisit if Copilot needs another host, a write method or a namespace outside
these two roots. Such a change needs a new boundary review. A real-account
failure after successful routing must be investigated separately from a
gateway refusal.

## Removing `--no-remote`

Moved 2026-11-24 from ADR 0050, whose other decisions stay there. The text is
unchanged but for its heading and two cross-references.

Remove Copilot's obsolete `--no-remote` launch argument and pin 1.0.103. The
original offline grouping (ADR 0046, removed; now in the AgentRun section above) did not establish a separate requirement
for suppressing remote sessions. No replacement setting is introduced. Keep
explicit MCP configuration, disabled built-in MCPs, disabled updates and the
existing sandbox/gateway enforcement. This resolves the removal noted in this
record's consequences without treating `--no-remote` as a network isolation control.

Corrected 2026-11-24: the pin is no longer 1.0.103. The pinned version is in
`hack/clients/copilot/package.json`.

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

Moved 2026-11-24 from ADR 0047's Evidence, unchanged:

- Copilot 1.0.102 signed in to GitHub (free plan), through a recording proxy
  on the user's machine and in a Linux container with the GitHub CLI's token
  (method, path and status; the account response's policy flags and endpoint
  hosts): model calls go to `api.individual.githubcopilot.com` `/responses`,
  `/v1/messages` or `/chat/completions` by model, with `/models` and `/auto`
  beside them; Copilot completes with `api.github.com` limited to
  `copilot_internal/user`, `managed_settings` and `team_customizations`, and
  with GitHub's built-in MCP server, agents, experiments and telemetry
  refused. Without a keychain, Copilot must fetch `copilot_internal/user`,
  whose `is_mcp_enabled` flag decides whether it loads any MCP server. `make
  verify-harness-process` runs Copilot with a fixture GitHub sign-in through
  the gateway's Copilot routes: a tool call, a refusal and `diagnose` pass.

Corrected 2026-11-24: that recording saw only `copilot_internal/` paths on
`api.github.com`. Copilot also reads `/copilot/` there, as "Account
namespaces and policy verification" above records.

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
- Copilot releases often. 1.0.103 removed `--no-remote`; the pinned version
  (`hack/clients/copilot/package.json`) is past it, so the supervisor and the
  AgentRun example drop the flag ("Removing `--no-remote`" above).

  Corrected 2026-11-16: the bullet above named the release that pinned 1.0.103 and
  that pin. It now points at the pinned version rather than a number.
- Copilot models on `/chat/completions` (Gemini, Kimi and similar) stream,
  which the gateway's chat-completions route refuses; Copilot's OpenAI and
  Claude models work. Business and Enterprise hosts are routed but unverified.
  (Moved 2026-11-24 from ADR 0047's consequences, replacing this record's
  pointer to it.)

Revisit when Copilot's journal format or `--session-id` changes, when Copilot
can trust a folder in interactive mode without `config.json`, or when it
gains a setting to ignore `GH_TOKEN` and `gh`.

Open question, undecided: the checkpoint code treats Copilot's journal as
carrying no evidence of the approved instructions
(`CopilotState.ValidateContinuity` compares none, and `conversationContracts`
in `internal/checkpoint/conformance_test.go` says why), yet this record says
Copilot adds `copilot-instructions.md` to its system prompt, and the journal
records the system message at the first turn. Does that message carry the
approved instructions, so that the "no instruction evidence" rule is wrong? The
checkpoint fixture comes from a launch with no instructions file, so it cannot
show it.

## Implementation

[registry](../../internal/harness/registry.go),
[journal and checkpoint](../../internal/harness/copilot_state.go),
[supervisor adapter](../../cmd/native-session/copilot.go),
[local gateway routes](../../internal/gateway/docker/gateway.go),
[provider table](../../internal/endpoints/model/providers.go),
[local setup](../../internal/localsetup/install.go),
[account relay](../../internal/endpoints/model/signin.go),
[stock process verifier](../../hack/verify/harness-process/main.go),
[CI checks](../../.github/workflows/test.yml),
[client install](../../hack/clients/install.sh) and the
[harness reference](../reference/harnesses.md#managed-interactive-sessions).
