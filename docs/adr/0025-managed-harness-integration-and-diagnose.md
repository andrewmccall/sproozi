# 0025: Every harness receives the Sproozi integration, and the gateway explains its own denials

Status: accepted
Date: 2026-10-20

## Problem and constraints

An agent inside a sandbox cannot tell a gateway refusal from an ordinary
network or service failure. It retries, looks for workarounds or misreports the
cause. The [managed harness configuration PRD](../explanations/future-architecture/managed-harness-configuration-and-trust.md)
asks for three things in every execution: Sproozi environment instructions, an
access-recovery skill, and a Sproozi MCP connection whose `diagnose` tool
explains recent denials (milestones M1 and M3).

Constraints found in the code:

- Two preparation paths exist. Managed native sessions (Codex and Claude Code)
  are launched by `native-session`, which runs as the agent's UID. Ordinary
  AgentRuns are launched by the administrator's AgentRuntime command from a
  read-only contract ConfigMap; Hermes alone uses a Sproozi launcher.
- Approved instructions are bound by SHA-256 through the session registry,
  placements and work owner. Codex continuity hashes the first developer
  message of the rollout against that digest.
- The proxy rejected unknown CONNECT destinations before authentication, so the
  most common refusal had no execution identity and no audit record.
- Gateway audit records were stdout lines only. Every manifest runs one
  gateway replica.
- With `--setting-sources ""`, Claude Code loads no user or repository
  `CLAUDE.md` and no skills from `CLAUDE_CONFIG_DIR`, but it does load a
  `--plugin-dir` plugin and `--mcp-config` servers. Codex 0.163.0 lists
  `$CODEX_HOME/skills` and sends `$CODEX_HOME/AGENTS.md` before repository
  `AGENTS.md`, in a message separate from `developer_instructions`. Both were
  checked against the installed pinned clients with a capturing model.

## Decision

**Delivery.** One harness-neutral source, embedded in the binaries
(`internal/harness/managed/`), supplies the instructions and the
`sproozi-access` skill.

- Managed native Codex: the supervisor writes `$CODEX_HOME/AGENTS.md` and
  `$CODEX_HOME/skills/sproozi-access/SKILL.md` at every launch, replacing what
  is there, and appends a required `mcp_servers.sproozi` entry after
  administrator options.
- Managed native Claude Code: the Sproozi text follows the approved
  instructions in `--append-system-prompt`; a `sproozi` plugin loaded with
  `--plugin-dir` carries the skill (`sproozi:sproozi-access`); the control
  server is the only entry in the strict MCP config. Every setting source stays
  disabled.
- Ordinary runs: the contract always contains the control connection in the
  harness's MCP config, `sproozi-instructions.md` and `sproozi-access.md`. The
  Hermes launcher appends both to its system prompt. The administrator command
  places them for the other harnesses; the examples show how.

Approved instructions and their digest never change. The Sproozi text travels
beside them, so sessions started before this change still resume.

**Reserved names.** `sproozi` is not a grantable MCP server: `MCPServerName`
rejects it, so policy, registry and native launches all do, and the CRD
rejects `mcp.sproozi`. Administrator Codex options cannot assign
`mcp_servers` or `mcp_servers.sproozi`.

**Control connection.** The gateway serves `/mcp/sproozi` on its own
authority, beside named MCP connections; on the Docker home this is
`gateway.mcpAuthority`, now required. It needs no capability. Its one tool,
`diagnose`, is read-only and selects records only by the authenticated
execution. A native launch without the control connection is rejected.

**Denial journal.** Each gateway process wraps its audit writer in a bounded
in-memory journal that forwards every record unchanged and keeps recent
`allowed: false` records per execution: 1024 executions, 64 records each, one
hour. `diagnose` returns `denied` with those records, or `not_observed`. It
never infers a cause it did not record.

**Attributed destination denials.** The proxy authenticates and revalidates a
CONNECT before checking the destination, then audits an unknown destination as
a `network.egress` denial for that execution. Requests without a valid identity
still receive no record and no routing information.

## Alternatives

- **Add the Sproozi text to the approved instructions.** One channel for every
  harness, but it changes the bound digest and breaks Codex continuity for every
  existing session.
- **Enable Claude's user setting source.** Loads skills from
  `CLAUDE_CONFIG_DIR`, but would also honour any `settings.json` the agent
  writes there on the next launch.
- **Make protected files unwritable by the agent.** Needs a separate UID or
  read-only mounts in both runtimes. Rejected for now: the files carry
  guidance, not authority, and the gateway enforces policy whatever they say.
- **Durable shared journal.** Survives restarts and supports several replicas,
  but every installation runs one replica and a lost journal only degrades
  diagnosis to `not_observed`.
- **Serve the Docker control connection on the gateway IP.** Avoids requiring
  `mcpAuthority`, but inspection certificates are bound to DNS names.
- **Defer `diagnose` until the journal design is complete.** The connection
  would exist with nothing to prove harnesses use it.

## Tradeoffs and consequences

- The agent shares a UID with `native-session` and can edit or delete its copy
  of the instructions and skill during a session. The next launch restores
  them. User and repository content cannot replace them: repository
  `CLAUDE.md` is never loaded, and repository `AGENTS.md` follows the Sproozi
  text. Compliance with instructions is never a security control.
- Codex sessions created before this change resume with the skill and control
  connection but without the Sproozi text in their history. Claude Code resumes
  with the system prompt and skill listing its conversation started with, so
  earlier Claude conversations also keep their earlier prompt.
- Loading any plugin skill makes Claude Code record a `skill_listing` context
  attachment, which the checkpoint transcript validator accepts.
- The journal is lost on gateway restart. It records every refusal, including
  upstream failures the gateway reports as errors; the `reason` says which. It
  does not yet say whether access could be requested.
- Traffic blocked before the gateway, by NetworkPolicy or a client that ignores
  the proxy, leaves no record; `diagnose` reports `not_observed`.
- Docker home configurations must set `gateway.mcpAuthority`, and the
  inspection certificate must cover it.
- Unauthenticated CONNECTs to unknown destinations now receive 401 instead of
  403, and authenticated ones cost an authority lookup before refusal, bounded
  by the existing authority-call limit.
- Hermes ordinary runs without named MCP servers now have one read-only tool
  instead of none.

## Revisit conditions

- Gateway replicas above one, or a need for diagnosis across restarts: move the
  journal behind a shared store.
- Evidence that agents edit or ignore the integration: deliver it from
  read-only mounts or a separate UID.
- M2 user and project skills: reuse the reserved names and the plugin and
  skills directories above, and reject collisions with `sproozi-access`.
- M4 access requests: add a requestable classification to journal records
  rather than parsing `reason`.
- A harness version that changes skill, instruction or MCP discovery: the
  stock-client tests in `cmd/native-session` and `make verify-harness-clients`
  fail first.

## Links

- Reference: [Sproozi integration and diagnose](../reference/sproozi-integration.md)
- Implementation: `internal/diagnose`, `internal/proxytransport/gateway.go`,
  `internal/harness/managed.go`, `cmd/native-session/managed.go`,
  `internal/runtime/kubernetes/contract.go`
