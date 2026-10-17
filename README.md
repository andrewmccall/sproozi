# Sproozi

**YOLO your agents. Not your infrastructure.**

I want to give an AI agent a job and let it get on with it. No approving every shell command. No sitting around waiting for it to finish.

But I don't want to give it my personal credentials, let it delete my home directory, or discover that an innocent-looking task turned into something much less innocent. I'd rather it didn't hack Hugging Face or turn the world into paperclips, either.

Sproozi runs **existing AI agents in isolated environments**, controls what they can access outside those environments, and lets you bring their conversations and unfinished work back to your own machine.

It's not another agent or coding assistant. It's a way to run the agents you already use with more freedom **inside boundaries you choose**.

> **Project status:** Early open-source preview. The core paths described below have been tested in specific scenarios, but this isn't a production-ready security boundary. See [what works today](#what-works-today) and [security limitations](#security-limitations).

## What can I do with it?

**Let an agent investigate your infrastructure.** Ask an agent to diagnose a failing Kubernetes workload and propose a fix, without giving it your personal cluster-admin credentials. Define which Kubernetes resources it may read and which repository it may propose changes to.

**Run coding agents away from your laptop.** Run Codex in Kubernetes while your laptop is offline, then pull the stopped session back and continue the same conversation in local Docker, including unfinished Git changes.

**Give assistants disposable workers.** Keep an assistant such as Hermes running for conversations and scheduled tasks, while individual jobs execute in restricted environments and end when their work is done.

## How it works

Sproozi combines three things:

1. **Isolation.** The agent runs in a dedicated Docker container or Kubernetes environment rather than directly on your everyday machine. It can use ordinary tools such as a shell, Git and compilers inside that environment.
2. **Controlled access.** The agent doesn't need your broad personal credentials. When it needs an external service, Sproozi can route the request through a gateway that checks what the agent is allowed to do and supplies the necessary credentials. For example, you can grant access to a particular Kubernetes namespace instead of handing over unrestricted cluster access.
3. **Portable work.** Sproozi can preserve a supported agent's conversation and Git workspace, so you can stop work in one environment and resume it in another. Permissions are checked again when work resumes; they don't travel with the session.

This is about giving an agent autonomy *within a defined boundary*. It does not make untrusted code harmless or eliminate every security risk.

## Use the agents you already like

Sproozi does not replace an agent's own reasoning or tools. It launches an existing agent, provides an environment for it to work in, and enforces the access that you configure.

- **Codex and Claude Code:** supported for Kubernetes jobs. Both support controlled session movement between Docker and Kubernetes.
- **OpenCode and Hermes:** supported for Kubernetes jobs; portable sessions aren't yet supported for these agents.

[Agent support and details](docs/reference/harnesses.md)

## Get started

Try the infrastructure-investigation demo in a local Kubernetes cluster.

**[Run the Kind walkthrough →](docs/guides/getting-started.md)**

Or choose a path closer to what you want to do:

| I want to… | Start here |
| --- | --- |
| Install on an existing Kubernetes cluster | [Kubernetes installation](docs/guides/install-kubernetes.md) |
| Move an interactive session between Docker and Kubernetes | [Session mobility](docs/guides/session-mobility.md) |
| Run Codex remotely, then pull its work back to my machine | [Remote work and local resume](docs/guides/portable-workflows.md) |
| Have Hermes delegate bounded tasks | [Hermes orchestration](docs/guides/hermes-orchestration.md) |
| Check the investigation-to-PR demo manually | [Manual acceptance checks](docs/guides/manual-pr-demo.md) |
| Connect persistent Hermes to retained Codex work | [Retained-work deployment](docs/guides/retained-work-kubernetes.md) |

The CLI can be built with:

```bash
go build -o bin/sproozi ./cmd/sproozi
```

The linked guides cover configuration and prerequisites. This is not yet a one-command install.

## What works today

There's working code and recorded tests, but support varies by workflow:

| Area | Current state |
| --- | --- |
| Kubernetes-based agent jobs | Tested with Codex, Claude Code, OpenCode and Hermes using deterministic providers |
| Controlled access to Kubernetes and external tools | [Recorded tool-access acceptance](test/acceptance/reports/verified-mcp-demo.md) with a real model, including native Kubernetes reads, configured HTTPS test servers and denied operations |
| Docker ↔ Kubernetes session movement | [Recorded round trips](test/acceptance/reports/verified-session-mobility.md) with Codex, Claude Code and deterministic providers, including unfinished Git changes |
| Remote Codex work followed by local resume | [Recorded retained-work acceptance](test/acceptance/reports/verified-portable-workflows.md) with deterministic models, actual package checks and isolated Kubernetes reads/denials |
| Infrastructure investigation and proposed GitHub fix | [Recorded SRE runs](test/acceptance/reports/verified-sre-demo.md) with a real model and GitHub; no merge or deployment |

These are **specific verified scenarios**, not a claim that every configuration works or that the project is ready for production. The [verification reports](test/acceptance/reports/README.md) give the evidence, and the [agent reference](docs/reference/harnesses.md) documents the supported paths.

## Security limitations

Isolation only works if the environment is configured and enforced correctly. In particular:

- Kubernetes network restrictions require an enforcing network plugin and verification that agents cannot bypass the gateway. Kubernetes NetworkPolicy permits traffic involving a Pod's own node, so policy alone is not a complete isolation guarantee.
- Rules can restrict external operations, but their precision depends on the integration. A service-aware permission can constrain particular actions. Configured Model Context Protocol (MCP) access checks tools and arguments, while a generic HTTPS allowance only constrains the destination.
- Resource and usage limits exist, but model-token and cost estimates are **not** hard guarantees against provider spend.
- A sandbox does not guarantee that an agent's answer, code or proposed fix is correct. Review changes before deploying or merging them.

Read the [security hardening guide](docs/reference/security-hardening.md) before using Sproozi with sensitive infrastructure.

## Where this is going

The aim is simple: **your agents, your infrastructure, your rules, and your sessions wherever you want to continue them.**

That opens the door to agents triggered by incidents, pull requests and persistent assistants; people taking over work started by an agent; and, eventually, agents collaborating without having to share one giant set of credentials.

Some of that is still future architecture, not shipped functionality. The [future architecture notes](docs/explanations/future-architecture/README.md) separate planned ideas from the working paths.

## Dig deeper

- [Architecture](docs/explanations/system-architecture.md)
- [Capabilities and gateways](docs/explanations/capabilities-and-gateways.md)
- [Projects and workspaces](docs/reference/projects-and-revisions.md)
- [Full documentation index](docs/README.md)
- [Stock runtime examples](examples/harnesses/README.md)
- [Session command reference](docs/reference/session-commands.md)
- [Configured MCP tools](docs/reference/configured-mcp.md)
- Kubernetes resource references: [runtime](docs/reference/agentruntime.md), [policy](docs/reference/agentpolicy.md), [template](docs/reference/agenttemplate.md) and [run](docs/reference/agentrun.md)
- [Architecture decisions](docs/adr/README.md), including [what a checkpoint contains](docs/adr/0008-selected-native-and-git-checkpoints.md) and [retained work with fresh permissions](docs/adr/0012-retained-work-and-fresh-session-continuations.md)
- [Project and Git revision acceptance evidence](test/acceptance/reports/execution-roadmap-evidence.json)
- Planned home-ops workflow: [playbook](docs/guides/home-ops-playbook.md) and [runbook](test/acceptance/home-ops.md). This alert-to-PR and recovery workflow has not passed end-to-end acceptance.
- [Future collaboration directions](docs/explanations/future-architecture/README.md#execution-roadmap-extensions)

Sproozi is open source and evolving. If you're trying to let agents do useful work without handing them the keys to everything, I'd love to hear what you're building.
