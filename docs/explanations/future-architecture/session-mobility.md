# Session mobility extensions

Status: future extensions to controlled Codex and Claude Code session movement.

[Move a live session between Docker and Kubernetes](../../guides/session-mobility.md) describes managed
Codex and Claude Code sessions. This page covers extensions to that workflow.

[ADR 0004](../../adr/0004-portable-sessions-and-bounded-executions.md) records the
accepted outcome. ADRs [0008](../../adr/0008-selected-native-and-git-checkpoints.md),
[0009](../../adr/0009-runtime-backends-and-sealed-accounting.md) and
[0010](../../adr/0010-managed-session-authority-and-admission.md) record the concrete
checkpoint, accounting and authority mechanisms. An AgentRun identifies one
bounded remote visit; a completed or cancelled Run cannot reopen.

Remote-first Codex work and later local resume are now implemented in the
[Run a task remotely, then resume it locally](../../guides/portable-workflows.md), under [ADR
0012](../../adr/0012-retained-work-and-fresh-session-continuations.md). The
remote catalogue accepts tasks independently of the laptop, retains genuine
stopped state, and transfers ownership once. A later local resume starts a new
execution with fresh approval; it does not renew the old live-move ledger.

## Adopt directly started sessions

The first supported entry is Sproozi's controlled Docker launch, selected in
[ADR 0005](../../adr/0005-controlled-docker-launch-before-session-adoption.md).
Adopting an arbitrary local Codex or Claude conversation needs explicit
selection, compatible native state, inspection of workspace dependencies and
control of the original writer before a resumed agent starts. Imported
credentials, hooks, plugins and cached approvals cannot become trusted
destination configuration.

A copied transcript alone cannot prove ownership or safe completion. Define how
the original process and all tool descendants stop, how uncertainty is reported,
and what happens when the original environment cannot be stopped and denied
access. Keep refusal explicit when those guarantees cannot be established.

## Additional harnesses and clients

[ADR 0007](../../adr/0007-codex-round-trip-before-harness-expansion.md) selects
Claude Code after Codex. The current workflow supports those two selected clients.
[ADR 0011](../../adr/0011-native-session-compatibility-and-completion.md) records
the Claude transcript and completion contract. [ADR 0015](../../adr/0015-default-clients-and-native-tool-schemas.md)
removes exact-version checks.
OpenCode and Hermes mobility remain later candidates. Each adapter must handle the client's state format, completion,
tool records and resume command.
Preserve native semantics rather than translating history into a new common chat
protocol. Background tasks, subagents and compaction are outside the current supported
state. The current selected checkpoint does not transfer running memory or tools.

Native GUI/IDE access remains a separate extension under
[ADR 0006](../../adr/0006-native-cli-and-terminal-before-gui-mobility.md).
A supported CLI resume command does not prove an official desktop client accepts
an arbitrary Sproozi endpoint. Test each selected client, its authentication,
session discovery, detach/reconnect and revocation separately. See
[harness and client compatibility](harness-portability.md).

## Enterprise profiles and retained state

The current remote executor keeps its own Kubernetes credentials and accepts
the developer through a narrow `sproozi-cli` ServiceAccount, authorized by
Kubernetes RBAC ([ADR 0036](../../adr/0036-remote-executors-through-kubeconfig-contexts.md)). Multiple owners, identity
providers, profile discovery and organizational quotas may justify a broader
access model. Resolve principal isolation, transfer policy and audit attribution
before adding enrollment or federation mechanisms.

Checkpoints can contain sensitive conversation output and ordinary workspace
files. The current implementation uses protected storage. Multi-owner retention,
delete/export controls and encryption require explicit operational requirements.
Do not grant authority from imported history or relax unknown-stop/accounting
rules to simplify storage recovery.

## Testing requirements

See [session movement requirements](../../../test/acceptance/README.md#session-movement-extensions)
for checks required before extending support.
