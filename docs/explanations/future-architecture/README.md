# Future architecture

**Audience:** contributors, operators evaluating remaining directions

Kubernetes execution, native and configured HTTPS MCP, four stock CLI launch
adapters and persistent Hermes delegation are implemented. Controlled Docker
sessions and selected Codex/Claude checkpoints now have independently verified
local-to-Kubernetes-to-local proofs. Their implementation and limits belong
in the [current architecture](../system-architecture.md), references and
[session guide](../../guides/session-mobility.md).

Remote-first Codex task retention, authenticated MCP dispatch and fresh local
continuation are also implemented. The [workflow guide](../../guides/portable-workflows.md)
and [ADR 0012](../../adr/0012-retained-work-and-fresh-session-continuations.md)
describe the remote catalogue, one-way home claim and execution-bound retries.
Configured Projects and exact workspace revisions now have
[isolated native acceptance](../../demos/execution-roadmap-evidence.json), including
actual Sproozi package checks and governed Pod-read/Secrets-denial tasks in both
placements. Paid-model engineering and personal home-ops acceptance remain unassessed.

This roadmap retains extensions that need further design or native evidence.
The extended Codex proof adds actual native tools, held-turn handoff, gateway and
direct-access denials, forced cancellation and immutable accounting replay.
The final-source matched-image Codex rerun passed. Claude Code 2.1.27 separately
passed three genuine production terminal turns, foreground Bash, exact unfinished
Git state, the original deadline, immutable accounting and both-placement denial
probes. Its full held placement move/cancel matrix remains unassessed. Broader
client/state/failure proofs remain scoped extensions. The
[verified demo](../../demos/verified-session-mobility.md) records the exact scope.

## Remaining directions

| Direction | Question | Next proof |
| --- | --- | --- |
| [DNS egress containment](dns-egress.md) | Which names need resolution without unrestricted recursive egress? | Isolated allowed/internal and refused/external UDP/TCP behavior |
| [Managed MCP](managed-mcp-servers.md) | How should Sproozi own server lifecycle and upstream restrictions? | One isolated managed server with revocation and cleanup |
| [Further runtimes](execution-runtimes.md) | Which additional placements can enforce the approved intent? | One concrete backend with admission, network, stop and accounting evidence |
| [Harness and client compatibility](harness-portability.md) | Which additional native sessions and clients support continuity? | Another pinned harness or separately proved GUI/IDE attachment |
| [Session extensions](session-mobility.md) | How should adoption, enterprise profiles and retention broaden the current session? | Explicit original-writer control and fresh destination authority |

## Completed sequence and remaining compatibility

The original delivery destination was the complete local-to-Kubernetes-to-local
roundtrip with the same conversation and unfinished workspace. Codex's extended
controlled proof passed, followed by Claude Code 2.1.27's production round trip.
The supported foreground workflow has two explicit native pins and independent
completion paths. OpenCode and Hermes mobility, arbitrary session adoption,
GUI/IDE attachment and broader native state remain future work. Repeat the
applicable authority, state and failure checks for each extension.

| Decision | Initial scope | Record |
| --- | --- | --- |
| Local entry | Sproozi launches the session in Docker; adoption follows later. | [ADR 0005](../../adr/0005-controlled-docker-launch-before-session-adoption.md) |
| Client experience | Native CLI and remote terminal; GUI/IDE needs independent proof. | [ADR 0006](../../adr/0006-native-cli-and-terminal-before-gui-mobility.md) |
| Harness sequence | Codex first, then Claude Code; sequencing fulfilled. | [ADR 0007](../../adr/0007-codex-round-trip-before-harness-expansion.md) |

[ADR 0004](../../adr/0004-portable-sessions-and-bounded-executions.md) records the
session/execution split. [ADR 0008](../../adr/0008-selected-native-and-git-checkpoints.md),
[ADR 0009](../../adr/0009-runtime-backends-and-sealed-accounting.md) and
[ADR 0010](../../adr/0010-managed-session-authority-and-admission.md) record the
selected checkpoint, accounting, admission and authority mechanisms. Those choices
have concrete implementations and revisit conditions; they are no longer open
roadmap proposals.

## Execution roadmap extensions

These notes preserve the requested future questions. They do not add dependencies
to the current retained-work, Project or workspace-revision workflow.

- [Session forks and conversation checkpoints](session-forking-and-checkpoints.md)
- [Multiplayer sessions](multiplayer-sessions.md)
- [Agent communication](agent-to-agent-communication.md)
- [Shared workspace reconciliation](shared-workspace-reconciliation.md)
- [Workspace memory and indexing](workspace-memory-and-indexing.md)
- [Cross-harness context handoff](cross-harness-handoff.md)
- [Project permissions and membership](project-permissions-and-membership.md)
