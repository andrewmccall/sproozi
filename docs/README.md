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

## Explanations

- [System architecture](explanations/system-architecture.md)
- [Run lifecycle](explanations/run-lifecycle.md)
- [Capabilities and gateways](explanations/capabilities-and-gateways.md)

## Architecture decisions

Architecturally significant changes require an ADR and matching current docs.
[Decision records and contributor guidance](adr/README.md) describe the process.

- [0001: MCP through the shared capability gateway](adr/0001-mcp-capabilities-through-shared-gateway.md)
- [0002: Native CLI harnesses and model protocols](adr/0002-native-cli-harnesses-and-model-protocols.md)

## Future architecture

Directional designs for extensions to the implemented capability model. Native
Kubernetes MCP and configured remote HTTPS MCP tools are supported today; the
references and acceptance report above describe their bounds. Managed servers,
additional runtimes and managed harness control remain future directions.
[Stock CLI harnesses](reference/harnesses.md) describes current Codex, Claude Code
and OpenCode preparation and its bounded verification evidence.

- [Overview and evolution](explanations/future-architecture/README.md)
- [Capabilities and MCP interoperability](explanations/future-architecture/capabilities-and-mcp.md)
- [Multiple execution runtimes](explanations/future-architecture/execution-runtimes.md)
- [Harness portability and control](explanations/future-architecture/harness-portability.md)
