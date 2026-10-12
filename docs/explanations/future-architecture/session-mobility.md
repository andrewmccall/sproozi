# Session mobility extensions

**Status:** future extensions to controlled Codex and Claude Code session mobility.
The [current session guide](../../guides/session-mobility.md) describes native
terminal attachment, selected conversation/Git checkpoints, protected home
journals, fixed remote profiles and fresh per-visit admission. A deterministic
three-turn Docker-to-Kubernetes-to-Docker core proof passed with the same native
UUID, workspace, original deadline and sealed usage. The extended Codex proof
also passed actual native tool effects, a held remote move, both placements'
gateway/direct-access denials, forced held-turn cancellation and immutable
accounting replay. The final-source matched-image Codex rerun passed.
The production Claude Code 2.1.27 round trip separately passed three genuine
terminal turns, same-UUID conversation, actual foreground Bash and exact
unfinished Git state. It retained selected ignored files, the original deadline
and 320 sealed units across eight deterministic Messages requests. Its optional
administrator probes passed both-placement gateway/direct-access denials.
Claude's full held placement move/cancel matrix remains unassessed; the native
held-turn prerequisite is a separate observation. These fixtures do not establish
long histories, compaction, background tasks or every failure. See the
[verified demo](../../demos/verified-session-mobility.md) for the exact scope.

[ADR 0004](../../adr/0004-portable-sessions-and-bounded-executions.md) records the
accepted outcome. ADRs [0008](../../adr/0008-selected-native-and-git-checkpoints.md),
[0009](../../adr/0009-runtime-backends-and-sealed-accounting.md) and
[0010](../../adr/0010-managed-session-authority-and-admission.md) record the concrete
checkpoint, accounting and authority mechanisms. An AgentRun identifies one
bounded remote visit; a completed or cancelled Run cannot reopen.

## Adopt directly started sessions

The first supported entry is Sproozi's controlled Docker launch, selected in
[ADR 0005](../../adr/0005-controlled-docker-launch-before-session-adoption.md).
Adopting an arbitrary local Codex or Claude conversation needs explicit selection,
compatible native state, inspection of workspace dependencies and control of the
original writer before any continuation starts. Imported credentials, hooks,
plugins and cached approvals cannot become trusted destination configuration.

A copied transcript alone cannot prove ownership or safe completion. Define how
the original process and all tool descendants stop, how uncertainty is reported,
and what happens when the original environment cannot be fenced. Keep refusal
explicit when those guarantees cannot be established.

## Additional native sessions and clients

[ADR 0007](../../adr/0007-codex-round-trip-before-harness-expansion.md) selects
Claude Code after Codex. Both exact pins now have production placement proofs.
The selected Claude 2.1.27 transcript adapter and completion path are recorded under
[ADR 0011](../../adr/0011-native-session-compatibility-and-completion.md).
OpenCode and Hermes mobility remain later candidates. Each further adapter needs
evidence for its pinned native version, state format, completion boundary, tool
records and resume command.
Preserve native semantics rather than translating history into a new common chat
protocol. Background tasks, subagents and compaction need their own compatibility
proofs. The current selected checkpoint does not transfer running memory or tools.

Native GUI/IDE access remains a separate extension under
[ADR 0006](../../adr/0006-native-cli-and-terminal-before-gui-mobility.md).
A supported CLI resume command does not prove an official desktop client accepts
an arbitrary Sproozi endpoint. Test each pinned client, its authentication,
session discovery, detach/reconnect and revocation separately. See
[harness and client compatibility](harness-portability.md).

## Enterprise profiles and retained state

The current fixed TLS profile keeps kubeconfig on the administrator's service and
supplies the developer a narrow principal token. Multiple owners, identity
providers, profile discovery and organizational quotas may justify a broader
access model. Resolve principal isolation, transfer policy and audit attribution
before adding enrollment or federation mechanisms.

Checkpoints can contain sensitive conversation output and ordinary workspace
files. The current implementation uses protected storage. Multi-owner retention,
delete/export controls and encryption require explicit operational requirements.
Do not grant authority from imported history or relax unknown-stop/accounting
rules to simplify storage recovery.

## Proofs before implementation claims

| Milestone | Required evidence |
| --- | --- |
| Claude held placement behavior | Wait for the held turn before moving; force cancel with an actual writer fence, no destination and unknown or conservatively sealed usage |
| Broader failures | Extend retained failure evidence for capture/restore/admission, restart, stale owners and unknown stop/accounting |
| Broader native and failure coverage | Extend the two verified foreground contracts with specific tools/states and failure cases; verify each independently |
| Additional native state | Separate evidence for compaction, background tasks, subagents or another pinned version without importing configuration/authority |
| Session adoption | Explicit original-writer control, compatible selected state and rejection of imported authority |
| Native client connection | Separate pinned CLI, terminal and GUI/IDE proofs; unavailable paths stated explicitly |
| Enterprise access | Owner isolation, approved profiles, quotas and audit without developer cloud/Kubernetes credentials |

Successful output alone does not prove the whole matrix. Record each observed
result against the actual artifact, native client version and backend; preserve
failed proof evidence and distinguish unknown usage from known zero.
