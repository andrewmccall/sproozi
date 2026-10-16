# Session commands

The public executable is `sproozi`. Build it from the repository root:

```sh
go build -o bin/sproozi ./cmd/sproozi
```

These commands require administrator configuration, approved images and prepared
execution environments. The [home service reference](../../cmd/sproozi/README.md)
describes that configuration. Building the CLI does not install a cluster or
start an agent.

## Remote work and local continuation

| Command | Purpose |
| --- | --- |
| `run` | Submit a Codex task to a configured remote executor. |
| `sessions` | List accessible sessions, optionally filtered by executor or Project. |
| `status` | Inspect one session by its stable session UUID. |
| `pull` | Claim a stopped remote session and import its conversation and Git workspace. This does not start an agent. |
| `resume` | Start a new local execution of retained work with fresh permissions. It attaches the terminal unless `--detach` is selected. |
| `stop` | Stop local work and retain its supported state for later continuation. |
| `cancel` | Cancel an execution, including unfinished work that cannot stop gracefully. |
| `recover` | Continue a journaled operation after an uncertain response or interruption. |

These commands accept `--config FILE`, defaulting to `sproozi/config.json`
under the platform's user configuration directory. `--executor NAME` selects a
configured remote owner; `pull` selects its source with `--from NAME`. Preserve
the session and operation UUIDs when retrying the same operation. A native
conversation UUID identifies the agent's conversation and is distinct from the
session and its individual executions.

Follow [retained remote work](../guides/portable-workflows.md) for complete
commands and prerequisites, and [Projects and workspace revisions](projects-and-revisions.md)
for Project filtering and starting a fresh conversation at a recorded Git commit.

## Interactive sessions and movement

`serve --config FILE` starts the home daemon. The interactive `start`, `attach`
and `move` commands require an explicit `--socket PATH`; they do not use the
configuration-file selection above. `start` seeds a managed Docker workspace
from a stopped Git repository, `attach` connects its native terminal, and `move`
transfers the supported state to a configured destination.

Detaching the terminal does not stop the daemon or agent. Running a container on
your laptop still requires the laptop and Docker engine to remain available.
Follow [session mobility](../guides/session-mobility.md) for the complete setup,
commands, supported client versions and movement limits.
