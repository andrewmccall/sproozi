# Sproozi documentation

Start with [Getting started](guides/getting-started.md), or choose another
guide below for the work you want to do. For background,
[core concepts](explanations/core-concepts.md) explains how tasks, permissions
and sessions fit together, and the [glossary](../GLOSSARY.md) defines exact
terms. The main [README](../README.md) gives the project overview.

## Start here

| I want to… | Guide | Uses |
| --- | --- | --- |
| Try Sproozi on a local Kind cluster | [Getting started](guides/getting-started.md) | Checkout and Make |
| Install on an existing Kubernetes cluster and run a model | [Install on Kubernetes and run your first Pod](guides/install-kubernetes.md) | Checkout and Make |
| Run the investigation-to-PR demo step by step | [Manual AgentRun to pull request](guides/manual-pr-demo.md) | Checkout and Make |
| Have a persistent Hermes assistant delegate bounded tasks | [Run a persistent Hermes assistant on Kubernetes](guides/hermes-orchestration.md) | Checkout and Make |
| Install the command-line tool | [Install the CLI](guides/install.md) | Homebrew or a download |
| Move an interactive session between Docker and Kubernetes | [Move a live session between Docker and Kubernetes](guides/session-mobility.md) | CLI and a matching checkout |
| Run Codex remotely, then continue its work on my machine | [Run a task remotely, then resume it locally](guides/portable-workflows.md) | CLI and a matching checkout |
| Connect persistent Hermes to remote Codex work | [Hermes with remote work](guides/retained-work-kubernetes.md) | CLI and a matching checkout |

"Checkout and Make" guides run from a clone of this repository and never call
`sproozi`. The CLI guides use the installed `sproozi` command, and build images
and the profile service from a checkout of the same release as the CLI. Before
a deployment, check [cluster dependencies](reference/dependencies.md) and
[security hardening](reference/security-hardening.md).

## Task guides

These guides give prerequisites, steps and checks for specific tasks.

- [Model provider authentication](guides/model-provider-auth.md)
- [Use Sproozi on Windows](guides/windows.md)

## Understand the system

Read these explanations when you need more detail about the basic model.

- [System architecture](explanations/system-architecture.md) describes components and trust boundaries.
- [Capabilities and gateways](explanations/capabilities-and-gateways.md) explains how Sproozi checks service requests.
- [Run lifecycle](explanations/run-lifecycle.md) explains admission, completion and cleanup.

## Look up configuration

References describe exact fields, commands and supported behaviour.

| Topic | References |
| --- | --- |
| Terminology | [Glossary](../GLOSSARY.md) |
| Kubernetes task resources | [AgentRuntime](reference/agentruntime.md), [AgentPolicy](reference/agentpolicy.md), [AgentTemplate](reference/agenttemplate.md), [AgentRun](reference/agentrun.md) |
| Access and credentials | [Permissions and capabilities](reference/permissions-and-capabilities.md), [security hardening](reference/security-hardening.md) |
| Installation | [Cluster dependencies](reference/dependencies.md), [required secrets and config](reference/required-secrets-and-config.md) |
| Harnesses | [Harnesses](reference/harnesses.md), [stock runtime examples](../examples/harnesses/README.md), [client compatibility](reference/client-compatibility.md), [input contract and completion](reference/agent-contract.md), [native launch and control](reference/native-control.md) |
| MCP tools | [Native Kubernetes MCP](reference/kubernetes-mcp.md), [configured remote MCP](reference/configured-mcp.md) |
| Sessions and remote work | [Session commands](reference/session-commands.md), [Projects and workspace revisions](reference/projects-and-revisions.md) |
| Task submission services | [Task service MCP interface](reference/orchestration.md), [webhook contract](reference/webhook-contract.md) |

## Decisions and future work

[Architecture decisions](adr/README.md) explain the choices behind the current
system, their alternatives and their tradeoffs.

[Future architecture](explanations/future-architecture/README.md) describes
proposed extensions and distinguishes them from implemented behaviour.

The [Home-ops playbook](guides/home-ops-playbook.md) describes a planned
installation. It requires an Alertmanager converter and signer that this
repository does not supply.

For contributor test requirements and commands, see
[Testing](../test/acceptance/README.md). The
[recorded test reports](../test/acceptance/reports/README.md) hold the evidence
from specific acceptance runs.

Maintainers can follow [the release steps](contributing/releases.md).
