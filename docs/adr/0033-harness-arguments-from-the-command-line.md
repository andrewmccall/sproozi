# 0033: Harness arguments from the command line

Status: accepted
Date: 2026-10-20

## Problem and constraints

Managed sessions launch the stock harness with flags Sproozi generates
([ADR 0011](0011-native-session-compatibility-and-completion.md),
[ADR 0025](0025-managed-harness-integration-and-diagnose.md)). Users had no way
to choose anything else per session, such as a model or reasoning effort, and
the configured `--model` became one more administrator setting to keep in
step with each harness's own catalogue. The owner chose to pass harness
parameters through from the command line rather than wrap each one.

Some flags must stay Sproozi's because the session contract depends on them:
the managed workspace directory, the unattended sandbox and approval mode,
provider routing through the gateway, the approved developer instructions
(their digest is checked on every checkpoint), the selected conversation, and
the interactive terminal and hooks the completion checks read. The gateway
enforces network access and credentials whatever the harness is told, so the
remaining flags are a matter of user choice, not of trust.

Separately, every tester's first Codex attach stopped at Codex 0.163.0's
"Try new model" prompt, with the upgrade preselected. Accepting it replaced
the configured model, and an unattended session waited behind it.

## Decision

`sproozi start ... -- ARGS` and `sproozi resume ID -- ARGS` carry `ARGS`
verbatim to the harness for that execution. They are stored on the execution
and in each of its launches, so a move to another local visit keeps them; they
are not session identity, and a later `resume` chooses its own. The supervisor
appends them after its own flags and before `resume <thread>` (Codex) or
`--resume <thread>` (Claude Code).

`harness.ValidateNativeArgs` refuses, with a message naming the flag and the
reason, any argument that would replace a flag Sproozi controls. Refusal is
explicit and happens before an execution is created; Sproozi never rewrites
the user's arguments. The lists are per harness:

- Codex: sandbox and approval flags (`--sandbox`, `--ask-for-approval`,
  `--yolo`, `--approve-for-me`, `--full-auto`), the directory (`--cd`,
  `--worktree`), provider selection (`--oss`, `--local-provider`, `--profile`),
  `--remote`, the flags Sproozi already sets (`--no-daemon`,
  `--no-alt-screen`), and `-c`/`--config` overrides of the keys Sproozi sets or
  that change those choices: instructions, `model_provider(s)`,
  `mcp_servers.sproozi`, `projects`, sandbox and approval keys,
  `check_for_update_on_startup`, profiles and `notice`.
- Claude Code: permission flags, `--tools`/`--allowedTools`, the MCP, plugin
  and settings flags, system prompt flags, conversation selection (`--resume`,
  `--continue`, `--session-id`, `--fork-session`, `--from-pr`, `--teleport`,
  `--no-session-persistence`), non-interactive and background modes
  (`--print`, `--output-format`, `--bg`, `--remote-control`, `--cloud`,
  `--desktop`, `--tmux`), `--bare`, `--safe-mode`, `--restricted` and
  `--worktree`.

The model is the user's: when the arguments choose one (`--model`, Codex `-m`
or `-c model=...`), the configured model is left out of the launch.

Arguments apply to local Docker visits only. Home refuses to move an execution
that has them to a remote placement, and the remote profile refuses launches
that carry them, because the Kubernetes launch path has no field for them yet.

To keep any model in force and an unattended session unblocked, the Codex
supervisor runs the binary's own `codex debug models --bundled` at activation
and passes `notice.model_migrations` with every upgrade in that catalogue, as
choosing "Use existing model" would record. Codex 0.163.0 has no single setting
that disables the prompt: it shows it unless `notice.model_migrations` maps the
current model to the offered target, and the older `hide_gpt5_1_*` keys cover
only two specific migrations. If the catalogue cannot be read the launch
continues and the prompt stays possible.

## Alternatives

- **Wrap each harness option as a Sproozi flag.** Every new harness option
  would need a Sproozi release, validation and documentation, and the
  configured model already showed the cost.
- **Let Sproozi's value win silently.** Appending Sproozi's flags last would
  hide the user's mistake and depends on each harness's repeat-flag rules;
  Codex rejects repeated single-value options.
- **Store arguments on the session.** Every later execution would inherit
  options the user may have meant for one run, such as a test model.
- **Hard-code the Codex migration map.** A Sproozi-maintained map would go
  stale with each Codex release; the binary's catalogue is the source Codex
  uses itself.
- **Replace the catalogue with `model_catalog_json`.** That also removes the
  prompt, but replaces model metadata Codex relies on.

## Consequences and limits

- A bare `--` is refused, because it would turn the conversation Sproozi
  resumes after the arguments into positional input.
- Other positional arguments pass through. A Codex positional becomes the initial
  prompt or a subcommand, so arguments that break the interactive terminal fail
  that launch rather than being caught in advance.
- The refused lists follow Codex 0.163.0 and Claude Code 2.1.302 options and
  need review with each client update.
- Arguments cannot move with an execution to Kubernetes.
- `codex debug models --bundled` reads only the bundled catalogue. A catalogue
  refreshed from the provider at run time could offer an upgrade it lacks.

Revisit when the Kubernetes launch path carries per-execution configuration,
when a harness adds a supported switch for upgrade prompts, or when a harness
option becomes part of the session contract.

Implementation: [argument rules](../../internal/harness/native_args.go),
[launch arguments](../../cmd/native-session/launch.go),
[Claude arguments](../../cmd/native-session/claude.go),
[home service](../../internal/session/home/service.go) and
[CLI parsing](../../cmd/sproozi/product.go). Usage is in the
[session commands reference](../reference/session-commands.md#pass-arguments-to-the-harness).
