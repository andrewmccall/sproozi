# Sproozi docs

Start with the getting started guide to explore the Kind demo, or the Kubernetes
installation guide to deploy on an existing cluster. References describe
the current API and configuration; explanations describe how the code fits together.
Future architecture is kept separate from implemented behaviour and verified guides.

## Guides

- [Getting started](guides/getting-started.md), from a fresh checkout to a Kind demo and a real pull request
- [Kind verification skill](../.agents/skills/verify-sproozi/SKILL.md), repeatable dependency checks, demo acceptance and retained proof
- [Install on Kubernetes and run your first Pod](guides/install-kubernetes.md), including registry images, certificates and a provider-free first run
- [Manual AgentRun to pull request](guides/manual-pr-demo.md), including Kind setup, example resources and optional monitoring
- [Recorded native and configured MCP acceptance](demos/verified-mcp-demo.md), including stock Codex tool use, denials and cancellation
- [Persistent Hermes orchestration](guides/hermes-orchestration.md), native chat, schedules, PVC state and full Kind testing
- [Portable native sessions](guides/session-mobility.md), controlled Docker launch, native terminal and fixed Kubernetes profile moves
- [Retained remote work](guides/portable-workflows.md), remote-first Codex tasks, MCP dispatch, session pull and fresh local continuation; scoped home-ops setup
- [Portable workflow verification](../hack/verify/portable-workflows/README.md), deterministic native task and real external MCP checks in isolated infrastructure
- [Recorded retained-work acceptance](demos/verified-portable-workflows.md), stock Hermes dispatch, native continuity, fresh grants and real MCP transport; paid contributions and personal home-ops remain unassessed
- [Model provider authentication](guides/model-provider-auth.md)
- [Recorded SRE acceptance](demos/verified-sre-demo.md), with scope and limitations

## Planned home-ops integration

These guides are retained for upcoming work. Their alert delivery, GitOps PR and
recovery flow has not been verified end to end.

- [Home-ops playbook](guides/home-ops-playbook.md)
- [Home-ops demo runbook](guides/home-ops-demo-runbook.md)

## Reference

- [AgentRuntime](reference/agentruntime.md)
- [AgentPolicy](reference/agentpolicy.md)
- [AgentTemplate](reference/agenttemplate.md)
- [AgentRun](reference/agentrun.md)
- [Permissions and capabilities](reference/permissions-and-capabilities.md)
- [Security hardening](reference/security-hardening.md)
- [Webhook contract](reference/webhook-contract.md)
- [Required secrets and config](reference/required-secrets-and-config.md)
- [Agent contract and completion](reference/agent-contract.md)
- [Standard client compatibility](reference/client-compatibility.md)
- [Kubernetes MCP delivery](reference/kubernetes-mcp.md)
- [Configured remote MCP capabilities](reference/configured-mcp.md)
- [Native CLI harnesses](reference/harnesses.md)
- [Persistent task MCP interface](reference/orchestration.md)

- [Projects and workspace revisions](reference/projects-and-revisions.md), configuration, membership, local approval and exact-revision fresh work

## Explanations

- [System architecture](explanations/system-architecture.md)
- [Run lifecycle](explanations/run-lifecycle.md)
- [Capabilities and gateways](explanations/capabilities-and-gateways.md)

## Architecture decisions

Architecturally significant changes require an ADR and matching current docs.
[Decision records and contributor guidance](adr/README.md) describe the process.

- [0001: MCP through the shared capability gateway](adr/0001-mcp-capabilities-through-shared-gateway.md)
- [0002: Native CLI harnesses and model protocols](adr/0002-native-cli-harnesses-and-model-protocols.md)
- [0003: Persistent assistants and retained worker results](adr/0003-persistent-assistants-and-retained-results.md)
- [0004: Portable sessions across bounded executions, controlled round trips verified](adr/0004-portable-sessions-and-bounded-executions.md)
- [0005: Controlled Docker launch before existing-session adoption](adr/0005-controlled-docker-launch-before-session-adoption.md)
- [0006: Native CLI and terminal access before GUI mobility](adr/0006-native-cli-and-terminal-before-gui-mobility.md)
- [0007: Codex then Claude round trips, sequencing fulfilled](adr/0007-codex-round-trip-before-harness-expansion.md)
- [0008: Selected native and Git checkpoints](adr/0008-selected-native-and-git-checkpoints.md)
- [0009: Runtime backends and sealed accounting](adr/0009-runtime-backends-and-sealed-accounting.md)
- [0010: Managed session authority and fresh admission](adr/0010-managed-session-authority-and-admission.md)
- [0011: Native session compatibility and completion](adr/0011-native-session-compatibility-and-completion.md)
- [0012: Retained work and fresh session continuations](adr/0012-retained-work-and-fresh-session-continuations.md)
- [0013: Configured Projects and workspace revisions](adr/0013-configured-projects-and-workspace-revisions.md)

## Future architecture

Proposals for unimplemented extensions. Completed native/remote HTTPS MCP,
stock CLI launch and persistent Hermes delegation are documented in the current
references above. Controlled Docker and Kubernetes mobility and retained Codex
work are implemented. Existing-session adoption, managed servers, more backends
and deeper harness control remain future directions.

- [Overview and evolution](explanations/future-architecture/README.md)
- [Managed MCP servers and further semantic bindings](explanations/future-architecture/managed-mcp-servers.md)
- [Multiple execution runtimes](explanations/future-architecture/execution-runtimes.md)
- [Harness session adapters and remote control](explanations/future-architecture/harness-portability.md)
- [Portable agent sessions](explanations/future-architecture/session-mobility.md)

- [Verified native session mobility](demos/verified-session-mobility.md) records Codex and Claude placement, tool, denial and accounting evidence.
