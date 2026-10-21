# Sproozi documentation

Start with the main [README](../README.md#get-started): run an agent locally
in Docker, then try Sproozi on Kubernetes. For background,
[core concepts](explanations/core-concepts.md) explains how tasks, permissions
and sessions fit together, and the [glossary](../GLOSSARY.md) defines exact
terms.

## Start here

| I want to… | Guide | Needs |
| --- | --- | --- |
| Run Codex or Claude Code locally in Docker | [Local sessions](guides/local-sessions.md) | The CLI and Docker |
| Install a release on Kubernetes, including Kind | [Install on Kubernetes](guides/install-kubernetes.md) | kubectl and a cluster |
| Install the command-line tool | [Install the CLI](guides/install.md) | Homebrew or a download |
| Have Codex investigate a broken workload and open a PR | [Investigation-to-PR demo on Kind](guides/pr-demo.md) | A checkout, Make, a model account and a GitHub App |
| Run the investigation-to-PR demo step by step | [Manual AgentRun to pull request](guides/manual-pr-demo.md) | The demo above |
| Build and install my own images | [Install from a checkout](guides/install-from-checkout.md) | A checkout, Make and an image registry |
| Have a persistent Hermes assistant delegate bounded tasks | [Run a persistent Hermes assistant on Kubernetes](guides/hermes-orchestration.md) | A checkout and Make |
| Move an interactive session between Docker and Kubernetes | [Move a live session between Docker and Kubernetes](guides/session-mobility.md) | The CLI and a matching checkout |
| Run Codex remotely, then continue its work on my machine | [Run a task remotely, then resume it locally](guides/portable-workflows.md) | The CLI and a matching checkout |
| Connect persistent Hermes to remote Codex work | [Hermes with remote work](guides/retained-work-kubernetes.md) | The CLI and a matching checkout |

A matching checkout is a clone of this repository at the tag of your CLI's
version. Before a deployment, check [cluster dependencies](reference/dependencies.md)
and [security hardening](reference/security-hardening.md).

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
| Kubernetes task resources | [AgentRuntime](reference/agentruntime.md), [AgentPolicy](reference/agentpolicy.md), [AgentTemplate](reference/agenttemplate.md), [AgentRun](reference/agentrun.md), [BudgetLedger](reference/budgetledger.md) |
| Access and credentials | [Permissions and capabilities](reference/permissions-and-capabilities.md), [security hardening](reference/security-hardening.md) |
| Installation | [Local installation](reference/local-installation.md), [gateway certificates](reference/gateway-certificates.md), [cluster dependencies](reference/dependencies.md), [required secrets and config](reference/required-secrets-and-config.md) |
| Harnesses | [Harnesses](reference/harnesses.md), [stock runtime examples](../examples/harnesses/README.md), [client compatibility](reference/client-compatibility.md), [input contract and completion](reference/agent-contract.md), [native launch and control](reference/native-control.md) |
| MCP tools | [Native Kubernetes MCP](reference/kubernetes-mcp.md), [configured remote MCP](reference/configured-mcp.md), [Sproozi integration and diagnose](reference/sproozi-integration.md) |
| Sessions and remote work | [Session commands](reference/session-commands.md), [remote executor access](reference/remote-executors.md), [profile](reference/profile.md), [Projects and workspace revisions](reference/projects-and-revisions.md) |
| Task submission services | [Task service MCP interface](reference/orchestration.md) |
| Component references | [CLI service configuration](../cmd/sproozi/README.md), [remote executor](../cmd/session-profile/README.md), [native session supervisor](../cmd/native-session/README.md), [checkpoint helper](../cmd/checkpoint/README.md), [Docker execution backend](../internal/runtime/docker/README.md) |

## Decisions and future work

[Architecture decisions](adr/README.md) explain the choices behind the current
system, their alternatives and their tradeoffs.

[Future architecture](explanations/future-architecture/README.md) describes
proposed extensions and distinguishes them from implemented behaviour.

The [Home-ops playbook](guides/home-ops-playbook.md) describes a planned
installation. Sproozi has no event receiver, so its alerts cannot start runs by
themselves; see [event webhooks](explanations/future-architecture/event-webhooks.md).

For contributor test requirements and commands, see
[Testing](../test/acceptance/README.md). The
[recorded test reports](../test/acceptance/reports/README.md) hold the evidence
from specific acceptance runs.

Maintainers can follow [the release steps](contributing/releases.md). Contributors adding an agent program follow
[Add a harness](contributing/add-a-harness.md).
