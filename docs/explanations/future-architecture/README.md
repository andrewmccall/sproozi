# Future architecture

These notes describe proposed extensions. They do not add requirements to the
current setup guides.

Kubernetes runs, four stock harnesses, native and configured HTTPS MCP, and
persistent Hermes delegation are implemented. Controlled Codex and Claude Code
sessions can move between Docker and Kubernetes. Retained remote tasks, local
resume, configured Projects and exact-revision launches are also implemented.
See the [current architecture](../system-architecture.md), [session
guide](../../guides/session-mobility.md) and [remote work and
local resume guide](../../guides/portable-workflows.md) for setup and limits.

## Remaining directions

| Direction | Question | Main constraint |
| --- | --- | --- |
| [DNS egress containment](dns-egress.md) | Which names need resolution without unrestricted external DNS? | Enforce allowed internal queries and refused external queries over UDP and TCP |
| [Managed MCP](managed-mcp-servers.md) | How should Sproozi own server lifecycle and upstream restrictions? | Separate server credentials and lifecycle |
| [Managed harness configuration and trust](managed-harness-configuration-and-trust.md) | How should managed sessions receive protected integrations, approved skills and access diagnostics? | Preserve enforced authority across configuration layers and future access requests |
| [Simplified local client onboarding](simplified-local-client-onboarding.md) | How should an installed CLI prepare images, networking, shared agent state and TLS automatically? | Preserve native harness UX and trusted credential boundaries; keep Kubernetes installation operator-owned |
| [Further runtimes](execution-runtimes.md) | Which additional placements can enforce the approved intent? | Equivalent permission, network, stop and accounting guarantees |
| [Harness and client compatibility](harness-portability.md) | Which additional native sessions and clients support continuity? | Client-specific state and connection support |
| [Session extensions](session-mobility.md) | How should adoption, enterprise profiles and retention broaden the current session? | Stop the original writer and approve destination access independently |

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
