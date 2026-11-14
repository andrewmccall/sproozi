# Claude transcript fixtures

[`capture.py`](capture.py) regenerates the Claude Code transcript and hook
fixtures that the managed Claude lifecycle and transcript check are tested
against:

| Scenario | The scripted turn |
| --- | --- |
| `mcp-tool` | A foreground Bash call, then an MCP tool call, then a reply. The task starts with `--` and spans two lines. |
| `failed-tools` | The same calls, but the Bash command exits non-zero and the MCP tool returns an error. |
| `profile-skill` | A `Skill` call that loads `profile:fixture-skill` from the profile plugin. |
| `sproozi-skill` | A `Skill` call that loads `sproozi:sproozi-access` from the sproozi plugin. |

Each scenario writes the transcript `claude-<version>-<scenario>.jsonl` to
`internal/harness/testdata` and its hook payloads
`claude-<version>-<scenario>-hooks.jsonl` to `cmd/native-session/testdata`.

## Regenerate

The fixtures come from the pinned client, Claude Code 2.1.318
(`hack/clients/claude-code/package-lock.json`). Install that version, then run
from the repository root:

```bash
python3 hack/verify/claude-transcripts/capture.py --claude "$(command -v claude)" \
  mcp-tool failed-tools profile-skill sproozi-skill
```

With `--check` the same runs prove only that each task is submitted and its
turn completes unattended in a fresh home, and write no fixture. Use it to
check another client version.

The script needs Python 3, Go and a POSIX terminal. It makes no network
request and uses no credentials: a local provider scripts the model and a
local stdio MCP server stands in for the gateway's connections. Each run
writes a new session, so afterwards update the session IDs, digests and leaf
UUIDs in `internal/harness/claude_tools_test.go`; the script prints each
session ID.

## What the capture matches

The client runs in its interactive terminal with the arguments, plugins,
settings and preferences that `cmd/native-session` gives a managed session: the
`Bash,Skill` tools, `--strict-mcp-config`, the sproozi plugin with the
`sproozi-access` skill that `internal/harness` renders, a profile plugin with
one fixture skill, every setting source disabled, the `--settings` file that
`internal/harness` renders (the lifecycle hooks and the accepted
`bypassPermissions` confirmation), `bypassPermissions` and a task as the
positional prompt. The script types nothing into the terminal: the task must
be submitted and the turn completed unattended, or the run fails and prints
the terminal. After `Stop` it kills the client's process group, as the
supervisor's controlled stop does. The `managed` helper prints the skill and
settings from the same Go code the supervisor uses.

One thing differs from a managed session: the MCP server is a local stdio
process rather than the gateway's HTTP connection, which records the same tool
names and transcript rows.

## What is scrubbed

- Capture paths become the managed ones: the workspace becomes
  `/workspace/session/workspace`, the client home `/workspace/session/claude`,
  the plugins `/tmp/sproozi-native/sproozi-plugin` and
  `/tmp/sproozi-native/profile-plugin`, and the hook command
  `/usr/local/bin/native-session observe-claude`.
- Context attachments keep only their type. They describe the host and carry
  the system prompt.
- API request caches keep only their identifying fields, and cached message
  text becomes `elided`.
- `gitBranch` and `slug` are removed.

Conversation, tool, skill and metadata rows are otherwise as the client wrote
them. The script refuses to write a fixture that still contains the capture
directory, `$HOME` or `$USER`.
