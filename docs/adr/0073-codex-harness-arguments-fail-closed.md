# 0073: Codex harness arguments fail closed

Status: accepted
Date: 2026-11-21

Extends [ADR 0033](0033-harness-arguments-from-the-command-line.md).

## Problem and constraints

ADR 0033 lets a user pass their own arguments to the stock harness of a local
session and refuses those that would replace a flag or configuration key
Sproozi controls. Its Codex list refused `-c`/`--config` overrides of the keys
Sproozi sets, but the validator compared keys as text and read only the
spellings it expected. These forms passed it:

- `-c=KEY=VALUE`, whose key the validator read as empty;
- a key spelled with TOML quoting, an escape or spaces inside its quotes
  (`" sandbox_mode"=x`);
- a table that holds a controlled key (`-c mcp_servers={}`), which replaces
  the control connection Sproozi sets before the user's arguments;
- an approved connection Sproozi renders (`mcp_servers.<name>.url`), since
  only `mcp_servers.sproozi` was controlled;
- `-c`, `--config`, `-m` or `--model` followed by another option, which then
  became the override's value instead of being refused (`-c --sandbox`), or
  by nothing, which swallowed the `resume <id>` Sproozi appends after the
  arguments;
- an override that is not a TOML key and `=` at all (`-c foo`, `a b=x`).

The validator guards local Docker visits only. Harness arguments never reach
a remote executor: the remote profile refuses a launch that carries them
(`validateLaunch` in `internal/session/profile/protocol.go`), and home refuses
to move an execution that has them to a remote placement
(`internal/session/home/service.go`). Within a local visit the gateway still
enforces network access and credentials, so the validator keeps the session's
own settings, not the machine's boundary.

The validator also ran on every load and save of a stored session record, so
a stricter one would refuse a record an earlier release wrote and strand its
session, which [ADR 0066](0066-a-contradicted-retained-record-refuses-work-and-never-blocks-an-end.md)
forbids for contradicted records.

## Decision

1. **Fail closed, whatever the client's parser does.** The validator reads
   each override's key as a TOML dotted key: bare, basic (escapes decoded)
   and literal parts, with whitespace around each part and inside its quotes
   trimmed before the comparison (`internal/harness/toml_key.go`). It refuses
   an override it cannot read, one with no `=`, an empty key, a controlled
   key, any key below one and any table that holds one. `-c=KEY=VALUE` is read
   as `-c KEY=VALUE`. Case is kept, since TOML keys are case-sensitive.
2. **The whole `mcp_servers` table is controlled.** A Codex launch renders
   every approved connection there beside the control connection, and which
   names are approved varies by launch. A user can no longer add a Codex MCP
   server with `-c`, as Claude Code's `--mcp-config` and
   `--strict-mcp-config` and Copilot's `--additional-mcp-config` are already
   refused; configured MCP goes through the session's allowances
   ([configured MCP](../reference/configured-mcp.md)).
   `TestEveryKeyACodexLaunchSetsIsControlled` reads a rendered launch with
   the validator's own reader and requires every key it sets to be
   controlled. This is the orchestrator's recommendation and awaits the
   owner's confirmation; narrowing it to the names a launch renders is one
   change to `codexConfigOverride`.
3. **An option's separate value is neither an option nor missing.** `-c`,
   `--config`, `--model` and Codex's `-m`, given as a separate argument,
   need a next argument that does not begin with `-`. A joined value
   (`--model=NAME`) is unchanged.
4. **Arguments are checked where a launch is created and where it starts,
   not when a record loads.** Loading and saving a record check only the
   arguments' size (`harness.NativeArgsBounded`). `ValidateNativeArgs` runs
   when home starts or resumes a session, in `Coordinator.Create`, before a
   move records its intent, before a stored launch is prepared
   (`prepareDestination`) and again in the Docker placement's launch
   contract. A record holding arguments an earlier release accepted still
   loads and can be stopped, cancelled and removed; starting or moving it is
   refused with the validator's message.

## Alternatives

- **Control only the MCP names Sproozi renders.** It keeps a user's own
  `-c mcp_servers.<name>` working, but the validator does not see which names
  a launch approves, and a user's server outside the allowances is what
  configured MCP exists to prevent.
- **Match the client's parser exactly.** It would refuse only what Codex
  reads as a controlled key, and accept everything else it accepts. It needs
  the pinned client's parsing rules, which change between releases, and a
  difference is a bypass. Refusing what the validator cannot read costs a
  user only an unusual spelling.
- **Refuse every `-c`.** Simplest and closed, but it removes documented uses
  such as `-c model_reasoning_effort=high`.

## Tradeoffs and consequences

- A user loses `-c mcp_servers.<name>…`, `-c` values with no `=` and unusual
  key spellings, and must join an option's value that begins with `-`.
- A session started by an earlier release with such arguments still loads and
  ends. `resume` takes its own arguments, so it resumes; only a move of the
  execution that holds them (`move --local`) is refused.
- A record's arguments are no longer checked against the policy on load; an
  edited journal is refused when it would start, not when it is read.

## Revisit conditions

- How the pinned Codex parses `-c=KEY=VALUE`, quoted and escaped keys, and
  whether a table override replaces its children, is not established: only
  an unpinned client was installed where this was written. A client that
  reads a spelling differently could make a refusal unnecessary, never a
  bypass.
- `openai_base_url` and `shell_environment_policy.*` are not controlled. The
  first retargets a signed-in Codex's built-in provider and the second
  decides which variables reach the agent's shell; the gateway still decides
  where requests go, and the credential variables hold placeholders.
- Any other value-taking option given last (`--add-dir`, `--image`) still
  swallows the `resume <id>` Sproozi appends and breaks the session; only
  `-c`, `--config`, `--model` and `-m` are checked.
- The owner narrows the `mcp_servers` rule.

## Links

- [Session commands: agent arguments](../reference/session-commands.md#agent-arguments)
- `internal/harness/native_args.go` (`ValidateNativeArgs`,
  `NativeArgsBounded`, `codexConfigOverride`), `internal/harness/toml_key.go`,
  `internal/session/rules.go`, `internal/session/coordinator.go`
- Records: `internal/harness/testdata/registry-outcomes.json`,
  `cmd/native-session/testdata/launch-arguments.json`
- Tests: `internal/harness/codex_override_test.go`,
  `cmd/native-session/codex_keys_test.go`,
  `internal/session/legacy_args_test.go`
