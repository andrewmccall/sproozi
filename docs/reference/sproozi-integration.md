# Sproozi integration and diagnose

Every execution with a selected harness receives three Sproozi components:

- **Environment instructions** that tell the agent it runs in a sandbox, that
  the gateway may refuse access, and never to work around a refusal.
- **The `sproozi-access` skill**, which tells the agent how to recover from a
  failed external request.
- **The `sproozi` control connection**, an MCP server on the execution's
  gateway with one read-only tool, `diagnose`.

They improve how agents respond to a refusal. They grant no access: the gateway
enforces policy whatever an agent reads or does. [ADR
0025](../adr/0025-managed-harness-integration-and-diagnose.md) records the
design and its tradeoffs.

## What each harness receives

| Execution | Instructions | Skill | Control connection |
| --- | --- | --- | --- |
| Managed Codex session | `$CODEX_HOME/AGENTS.md` | `$CODEX_HOME/skills/sproozi-access` | `mcp_servers.sproozi`, required |
| Managed Claude Code session | After the approved `--append-system-prompt` text | Plugin `sproozi`, listed as `sproozi:sproozi-access` | Only entry in `--strict-mcp-config` |
| Ordinary run, any harness | Contract file `sproozi-instructions.md` | Contract file `sproozi-access.md` | Generated MCP config file |

The supervisor installs the managed-session files at every launch and replaces
anything already at those paths. The approved template instructions and their
digest do not change. Codex sessions started before this release resume with
the skill and control connection but without the instruction text in their
history. Claude Code resumes a conversation with the system prompt and skill
listing it started with, so a conversation started before this release keeps
its earlier prompt; the control connection is available on every launch.

In ordinary runs the administrator's AgentRuntime command places the contract
files. The [demo runtime](../../examples/sre-demo/agentruntime.yaml) shows Codex
and the [stock runtime examples](../../examples/harnesses/README.md) show the
others:

- **Codex** copies the instructions to `$HOME/.codex/AGENTS.md` and the skill to
  `$HOME/.codex/skills/sproozi-access/SKILL.md`.
- **Claude Code** appends both files to `--append-system-prompt`, because the
  example disables skills with `--disable-slash-commands`.
- **OpenCode** adds both files to `instructions`, because the example disables
  external skills.
- **Hermes** needs no command change: `hermes-launch.py` appends both files to
  the approved instructions and fails if either is missing.

## Reserved names

| Name | Where | Rejected |
| --- | --- | --- |
| `sproozi` | MCP server | `mcp.sproozi` in AgentPolicy and AgentRun, gateway MCP registry entries, native MCP grants |
| `mcp_servers`, `mcp_servers.sproozi.*` | Codex native options | Administrator `nativeSession.options` |
| `sproozi-access` | Skill | Reserved for the user and project skills planned in M2 |

Repository files cannot replace the integration. Managed Claude Code sessions
load no `CLAUDE.md` at all. Codex sends repository `AGENTS.md` after the Sproozi
instructions, so a hostile repository can contradict them but cannot remove
them.

## diagnose

Clients list the tool as `mcp__sproozi__diagnose` (Claude Code) or `diagnose` on
server `sproozi` (Codex).

| Argument | Type | Default | Meaning |
| --- | --- | --- | --- |
| `destination` | string | none | Case-insensitive substring of the denied operation or resource, such as `docs.example.com` |
| `lookback_seconds` | integer | `300` | How far back to look; values above `3600` use `3600` |
| `decision_id` | string | none | Return one earlier result's decision; ignores the other arguments |

The result is JSON text:

```json
{
  "status": "denied",
  "decisions": [{
    "decision_id": "d42",
    "timestamp": "2026-10-20T12:00:00Z",
    "source": "gateway",
    "capability": "network.egress",
    "operation": "CONNECT docs.example.com:443",
    "reason": "destination docs.example.com:443 is not enabled at this gateway",
    "policy": "sre-readonly"
  }]
}
```

| Status | Meaning |
| --- | --- |
| `denied` | The gateway refused at least one matching request from this execution. `decisions` lists up to 20, newest first. |
| `not_observed` | The gateway kept no matching refusal. `note` explains that this is evidence of neither a refusal nor an allowed request. |

Decision fields come from the gateway audit record: `capability`,
`enforcement` (`semantic`, `protocol` or `destination`), `operation`,
`resource`, `reason` and `policy`. Optional fields are omitted when empty. A
record never contains request bodies, headers, credentials or query strings.

`decisions` includes every request the gateway refused, which also covers
upstream failures it reports as errors, such as `upstream request failed`.
Read `reason` before treating a decision as a policy refusal.

### What diagnose can see

- **Only the calling execution's records.** The gateway selects records by the
  authenticated execution; no argument selects another execution's records.
  Decision IDs from another execution return `not_observed`.
- **Only the gateway's refusals.** NetworkPolicy, Docker network rules, and
  clients that bypass the proxy block traffic before it reaches the gateway, so
  those failures return `not_observed`.
- **Requests to unknown destinations.** The gateway authenticates a CONNECT
  before checking its destination, so a refused destination is recorded as a
  `network.egress` decision for that execution. Requests without a valid
  identity are refused without a record.

### Journal limits

Each gateway process keeps the records in memory:

| Limit | Value |
| --- | --- |
| Executions retained | 1024, evicting the least recently written |
| Records per execution | 64, oldest dropped first |
| Record age | 1 hour |

A gateway restart clears the journal. Every installed manifest runs one gateway
replica; with more than one, an execution could reach a replica that has none of
its records.

## Configuration

**Kubernetes.** The shared gateway serves `/mcp/sproozi` on its own host, on
ports 443 and 8443, beside configured MCP servers. Native sessions reach it at
`AgentRuntime.spec.gatewayEndpoint`. No configuration is needed.

**Docker home.** `gateway.mcpAuthority` is required, as `host:443`. It serves
the control connection whether or not `gateway.mcpRegistryFile` is set.
`sproozi init` writes `mcp.sproozi.internal:443`, also into an existing
configuration that lacks it, and the home's certificate authority covers it.
With `gateway.certificateFile` instead, that certificate must include the host
as a DNS name. Startup fails with `home: gateway mcpAuthority is required` or
`home: inspection certificate must cover MCP authority` otherwise.

## Limitations

- The agent runs with the same UID as the managed-session supervisor and can
  edit or delete its copy of the instructions and skill during a session. The
  next launch restores them.
- `diagnose` cannot yet say whether access can be requested; access requests
  are planned for M4.
- Installed Claude Code and OpenCode call `diagnose` through the authenticated
  gateway proxy after a refused request (`make verify-harness-process`). No
  Docker or Kind acceptance run covers it yet, and Codex is covered only by its
  configuration and tool listing.
