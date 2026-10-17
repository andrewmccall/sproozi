# Sproozi documentation

Start with [core concepts](explanations/core-concepts.md) to learn how tasks,
permissions and sessions fit together. Then choose a guide for the work you want
to do. The main [README](../README.md) gives the project overview.

## Start here

| I want to… | Read |
| --- | --- |
| Understand the basic model | [Core concepts](explanations/core-concepts.md) |
| Try the local Kind demo | [Getting started](guides/getting-started.md) |
| Install on an existing cluster | [Install on Kubernetes and run a first Pod](guides/install-kubernetes.md) |
| Check deployment requirements and limits | [Security hardening](reference/security-hardening.md) |
| Start a remote task and continue locally | [Retained remote work](guides/portable-workflows.md) |

## Task guides

These guides give prerequisites, steps and checks for specific tasks.

- [Manual AgentRun to pull request](guides/manual-pr-demo.md)
- [Model provider authentication](guides/model-provider-auth.md)
- [Run disposable jobs through Hermes](guides/hermes-orchestration.md)
- [Deploy Hermes with retained native work](guides/retained-work-kubernetes.md)
- [Move a native session between Docker and Kubernetes](guides/session-mobility.md)

## Understand the system

Read these explanations when you need more detail about the basic model.

- [System architecture](explanations/system-architecture.md) describes components and trust boundaries.
- [Capabilities and gateways](explanations/capabilities-and-gateways.md) explains how Sproozi checks service requests.
- [Run lifecycle](explanations/run-lifecycle.md) explains admission, completion and cleanup.

## Look up configuration

References describe exact fields, commands and supported behaviour.

| Topic | References |
| --- | --- |
| Terminology | [Sproozi terms](../GLOSSARY.md) |
| Kubernetes task resources | [AgentRuntime](reference/agentruntime.md), [AgentPolicy](reference/agentpolicy.md), [AgentTemplate](reference/agenttemplate.md), [AgentRun](reference/agentrun.md) |
| Access and credentials | [Permissions and capabilities](reference/permissions-and-capabilities.md), [required secrets and config](reference/required-secrets-and-config.md), [security hardening](reference/security-hardening.md) |
| Agent clients | [Harnesses](reference/harnesses.md), [client compatibility](reference/client-compatibility.md), [input contract and completion](reference/agent-contract.md), [native launch and control](reference/native-control.md) |
| MCP tools | [Native Kubernetes MCP](reference/kubernetes-mcp.md), [configured remote MCP](reference/configured-mcp.md) |
| Sessions and retained work | [Session commands](reference/session-commands.md), [Projects and workspace revisions](reference/projects-and-revisions.md) |
| Task submission services | [Disposable-job MCP interface](reference/orchestration.md), [webhook contract](reference/webhook-contract.md) |

## Decisions and future work

[Architecture decisions](adr/README.md) explain the choices behind the current
system, their alternatives and their tradeoffs.

[Future architecture](explanations/future-architecture/README.md) describes
proposed extensions and distinguishes them from implemented behaviour.

The [home-ops playbook](guides/home-ops-playbook.md) describes a planned
installation. It requires an Alertmanager converter and signer that this
repository does not supply.

For contributor test requirements and commands, see
[Testing](../test/acceptance/README.md).
