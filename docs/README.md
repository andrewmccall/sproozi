# Sproozi docs

Start with the manual guide to run the proven semantic demo. References describe
the current API and configuration; explanations describe how the code fits together.
Future architecture is kept separate from implemented behaviour and verified guides.

## Guides

- [Manual AgentRun to pull request](guides/manual-pr-demo.md), including Kind setup, example resources and optional monitoring
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

## Explanations

- [System architecture](explanations/system-architecture.md)
- [Run lifecycle](explanations/run-lifecycle.md)
- [Capabilities and gateways](explanations/capabilities-and-gateways.md)

## Future architecture

Directional designs for extensions to the current Kubernetes/SRE execution model.
These are not implemented APIs or verified integrations.

- [Overview and evolution](explanations/future-architecture/README.md)
- [Capabilities and MCP interoperability](explanations/future-architecture/capabilities-and-mcp.md)
- [Multiple execution runtimes](explanations/future-architecture/execution-runtimes.md)
- [Harness portability and control](explanations/future-architecture/harness-portability.md)
