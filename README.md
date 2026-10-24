# Sproozi

[![Tests](https://github.com/andrewmccall/sproozi/actions/workflows/test.yml/badge.svg)](https://github.com/andrewmccall/sproozi/actions/workflows/test.yml)
[![E2E](https://github.com/andrewmccall/sproozi/actions/workflows/test-e2e.yml/badge.svg)](https://github.com/andrewmccall/sproozi/actions/workflows/test-e2e.yml)
[![Lint](https://github.com/andrewmccall/sproozi/actions/workflows/lint.yml/badge.svg)](https://github.com/andrewmccall/sproozi/actions/workflows/lint.yml)
[![Release](https://img.shields.io/github/v/release/andrewmccall/sproozi?include_prereleases)](https://github.com/andrewmccall/sproozi/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

**YOLO your agents. Not your infrastructure.**

I want to give an AI agent a job and let it get on with it. No approving every shell command. No sitting around waiting for it to finish.

But I don't want to give it my personal credentials, let it delete my home directory, or discover that an innocent-looking task turned into something much less innocent. I'd rather it didn't hack Hugging Face or turn the world into paperclips, either.

Sproozi runs **existing AI agents in isolated environments**, controls what they can access outside those environments, and lets you continue their conversations and unfinished work in a session on your own machine. There's no command yet to copy that work into your own checkout.

It's not another agent or coding assistant. It's a way to run the agents you already use with more freedom **inside boundaries you choose**.

> **Project status:** Early open-source preview. The core paths described below have been tested in specific scenarios, but this isn't a production-ready security boundary. See [what works today](#what-works-today) and [security limitations](#security-limitations).

## What can I do with it?

**Let an agent investigate your infrastructure.** Ask an agent to diagnose a failing Kubernetes workload and propose a fix, without giving it your personal cluster-admin credentials. Define which Kubernetes resources it may read and which repository it may propose changes to.

**Run coding agents away from your laptop.** Run Codex in Kubernetes while your laptop is offline, then pull the stopped session back and continue the same conversation in local Docker, including unfinished Git changes.

**Give assistants disposable sandboxes.** Keep an assistant such as Hermes running for conversations and scheduled tasks, while each task it delegates runs in its own restricted environment that ends when the work is done. Delegated runs can be active at the same time; an administrator can cap how many.

## How it works

Sproozi combines three things:

1. **Isolation.** The agent program (the *harness*) runs in a *sandbox*: a dedicated Docker container or Kubernetes Pod rather than your everyday machine. It can use ordinary tools such as a shell, Git and compilers inside that sandbox.
2. **Controlled access.** The agent doesn't need your broad personal credentials. When it needs an external service, the request goes through Sproozi's *gateway*, which checks what the agent is allowed to do and supplies the necessary credentials. For example, you can grant access to a particular Kubernetes namespace instead of handing over unrestricted cluster access.
3. **Portable work.** Sproozi can keep a supported agent's *session*, its conversation and Git workspace, so you can stop work in one place and continue it in another. Permissions are checked again for each new *execution*; they don't travel with the session.

The [glossary](GLOSSARY.md) defines these and the other terms the documentation uses.

This is about giving an agent autonomy *within a defined boundary*. It does not make untrusted code harmless or eliminate every security risk.

## Use the agents you already like

Sproozi does not replace an agent's own reasoning or tools. It launches an existing agent, provides an environment for it to work in, and enforces the access that you configure.

- **Codex, Claude Code and GitHub Copilot CLI:** run locally in Docker or in Kubernetes Pods, with controlled session movement between the two (see [what works today](#what-works-today) for what is recorded). Copilot can use the gateway's OpenAI key, a Copilot plan token held by the gateway, or its own GitHub sign-in.
- **OpenCode and Hermes:** run in Kubernetes Pods only, and their sessions can't move. Hermes uses its official image; OpenCode needs an image you build with the stock CLI.

[Agent support and details](docs/reference/harnesses.md)

## Get started

### 1. Run an agent on your computer

You need Docker running and an OpenAI API key (for Codex or Copilot) or an
Anthropic API key (for Claude Code). Install the CLI, set up once, then start a session in
a Git repository with at least one commit:

```bash
brew install andrewmccall/tap/sproozi
sproozi init
cd /path/to/your/repository
sproozi run
```

`init` asks for your API key, then checks that the agent's container can't
reach the internet directly. Instead of typing the key:

- From a script, pass `--credential-stdin`, or `--credential-file FILE` with a
  file only you can read (`chmod 600 FILE`).
- To use your ChatGPT, Claude or Copilot subscription instead of a key, run
  `sproozi init --sign-in`. [How credentials reach an agent](docs/explanations/credentials.md)
  explains each way, and why a plan sign-in held by the gateway may breach
  your provider's terms.

`run` copies the repository into a container and opens Codex there; your own
directory isn't changed. To try it without a repository of your own, run
`git init demo && cd demo && git commit --allow-empty -m init` first.

- Ctrl-] detaches. Ctrl-C goes to the agent.
- `sproozi ls` lists sessions. The first few characters of an ID are enough
  for `sproozi attach ID` and `sproozi stop ID`.
- A stopped session shows as `retained`. `sproozi resume ID` carries on and
  reconnects your terminal (`-d` skips that).

The agents show expected warnings because the gateway refuses their own
services: Codex shows `⚠ 1 warning` (refused telemetry), Claude Code a 403 on
its remote settings. [Local sessions](docs/guides/local-sessions.md#run-an-agent)
explains them.

For Claude Code, run `sproozi init --harness claude-code`, then `sproozi run
--harness claude-code`; for Copilot, use `--harness copilot`. If a session is running, `init` saves the change and
says so. To apply it without stopping your sessions, run
`sproozi service stop --leave-running`, then `sproozi service start`;
`sproozi doctor` then checks the new agent's containment.

[Local sessions](docs/guides/local-sessions.md) explains each step.
[Install the CLI](docs/guides/install.md) covers downloads for Linux and
Windows.

### 2. Try it on Kubernetes

Install a release on a local Kind cluster and run a first check that shows one
request allowed through the gateway and three refused: one at the gateway and
two at the network (the API server and the internet, reached directly). It needs Kind, kubectl and internet access (the guide installs
Calico for network policy), but no accounts and no builds.

**[Install on Kubernetes →](docs/guides/install-kubernetes.md)**

### 3. Go further

| I want to… | Guide |
| --- | --- |
| Have Codex investigate a broken workload and open a PR (needs a model account and a GitHub App) | [Investigation-to-PR demo on Kind](docs/guides/pr-demo.md) |
| Move a live session between Docker and Kubernetes | [Session mobility](docs/guides/session-mobility.md) |
| Run Codex remotely, then continue its work on my machine | [Run a task remotely, then resume it locally](docs/guides/portable-workflows.md) |
| Have a persistent Hermes assistant delegate bounded tasks | [Persistent Hermes assistant on Kubernetes](docs/guides/hermes-orchestration.md) |
| Connect persistent Hermes to remote Codex work | [Hermes with remote work](docs/guides/retained-work-kubernetes.md) |
| Build and install my own images | [Install from a checkout](docs/guides/install-from-checkout.md) |

These guides need a clone of this repository, at the tag that matches your
CLI (`sproozi --version` prints it). The
[command reference](docs/reference/session-commands.md) lists every command
and flag.

## What works today

There's working code and recorded tests, but support varies by workflow:

| Area | Current state |
| --- | --- |
| Agent runs in Kubernetes Pods | Tested with Codex, Claude Code, OpenCode, Copilot and Hermes using [deterministic providers](GLOSSARY.md#deterministic-provider), by [`make test-e2e`](test/acceptance/running.md#harnesses-and-hermes-in-kind). The [E2E workflow](https://github.com/andrewmccall/sproozi/actions/workflows/test-e2e.yml) runs it on pull requests and pushes to `main`; check there for the latest result, which isn't always green |
| Controlled access to Kubernetes and external tools | [Recorded tool-access acceptance](test/acceptance/reports/verified-mcp-demo.md) with a real model, including native Kubernetes reads, configured HTTPS test servers and denied operations. Recorded with Codex 0.154.0; not re-recorded with the current pins. The full redacted reports aren't published yet |
| Local Docker sessions from `sproozi init` | Provisioning, per-session Codex, Claude Code or Copilot selection, gateway containment probes and certificate renewal checked on macOS Docker Desktop with deterministic providers by [`make verify-local-onboarding`](test/acceptance/running.md#local-onboarding); no recorded report is published yet. See the [local sessions guide](docs/guides/local-sessions.md) |
| Using your own subscription | Each agent can use a key or sign-in held by the gateway, or its own sign-in, which the gateway forwards while still inspecting and counting each call ([ADR 0047](docs/adr/0047-harnesses-sign-in-to-their-own-model-providers.md)). Local Docker sessions with each harness's own sign-in are checked with deterministic providers by `make verify-local-onboarding SIGN_IN=1`; Copilot's own GitHub sign-in runs through the gateway with a fixture account in `make verify-harness-process`. No real subscription run through Sproozi is recorded. Options differ by agent: ChatGPT plan sign-in is for Codex, and Copilot's plan uses a GitHub token ([model providers](docs/reference/harnesses.md#model-providers)) |
| Explaining refused access to the agent | Every harness receives Sproozi instructions, a `sproozi-access` skill and a `diagnose` tool for its own recent gateway refusals; tested with installed Codex, Claude Code, OpenCode and Copilot clients, which call `diagnose` through the gateway after a refused request; not yet in a recorded Kind run. See [Sproozi integration](docs/reference/sproozi-integration.md) |
| Your own instructions and skills | One profile, `~/.sproozi/profile` in a local installation, reaches every managed Codex, Claude Code and Copilot session with shared and per-harness instructions and skills. It is sent with each Docker launch and stored by digest for Kubernetes. Tested with installed clients and a Docker ↔ Kind session-mobility round trip. See [Profile](docs/reference/profile.md) |
| Docker ↔ Kubernetes session movement | [Recorded round trips](test/acceptance/reports/verified-session-mobility.md) with Codex 0.161.0, Claude Code 2.1.27 and deterministic providers, including unfinished Git changes. The pinned clients are now Codex 0.163.0 and Claude Code 2.1.302; movement hasn't been re-recorded with them. Copilot 1.0.102 moved Docker ↔ Kind ↔ Docker ↔ Kind with a bash tool call through `copilot_mobility.py` on 24 October 2026; no published report yet |
| Concurrent AgentRuns | New. There's no cluster cap by default. An optional cluster cap (`--max-active-runs`) or per-policy cap (`maxActiveRuns`) keeps extra runs `Queued`. Checked with unit tests and envtest. The Kind verifier's `uncapped-policy-concurrent-runs` and `policy-cap-monotonic-cancel-cleanup` checks ([`verify.py`](hack/verify/orchestration-kind/verify.py)) run concurrent fixture runs in the [E2E workflow](https://github.com/andrewmccall/sproozi/actions/workflows/test-e2e.yml). Concurrent runs with a real model are not verified. Read the [concurrency warnings](docs/reference/security-hardening.md#concurrency-and-gateway-capacity) before sharing a cluster |
| Remote Codex work followed by local resume | [Recorded remote-work acceptance](test/acceptance/reports/verified-portable-workflows.md) with deterministic models, actual package checks and isolated Kubernetes reads/denials |
| Infrastructure investigation and proposed GitHub fix | [Recorded SRE runs](test/acceptance/reports/verified-sre-demo.md) with a real model and GitHub; no merge or deployment. Recorded on 4 October 2026 from working trees that predate this repository's public history, so the base commit they cite isn't in it. The demo repository holding the PRs is private and the full reports aren't published yet |

These are **specific verified scenarios**, not a claim that every configuration works or that the project is ready for production. The Kubernetes API is alpha (`sproozi.com/v1alpha1`): a minor release may change it incompatibly, so pin a release and read its notes before upgrading ([versioning and compatibility](docs/contributing/releases.md#the-alpha-api)). The [recorded test reports](test/acceptance/reports/README.md) give the evidence for each run, and the [agent reference](docs/reference/harnesses.md) documents the supported paths.

## Security limitations

Isolation only works if the environment is configured and enforced correctly. In particular:

- Kubernetes network restrictions require an enforcing network plugin and verification that agents cannot bypass the gateway. Kubernetes NetworkPolicy permits traffic involving a Pod's own node, so policy alone is not a complete isolation guarantee. Sandboxes on both backends resolve no names, so DNS is not a path around the gateway; on Kubernetes the controller pins the gateway's address in each Pod ([ADR 0040](docs/adr/0040-sandboxes-reach-the-gateway-without-dns.md)).
- Rules can restrict external operations, but their precision depends on the integration. A service-aware permission can constrain particular actions. Configured Model Context Protocol (MCP) access checks tools and arguments, while generic HTTPS egress only constrains the destination.
- Agent containers must run locked down: non-root, read-only, no extra Linux capabilities and no mounted secrets.
- The gateway limits connections: by default 2048 connections shared by all runs (one per MiB of the gateway's 2Gi memory limit, leaving headroom for a few hundred concurrent model calls rather than one per connection), each one inspected, and at most 16 inspected connections per execution, or the connection limit if lower. Cleanup keeps a run's network controls in place until Sproozi sees its Pod gone, but interrupted cleanup and lost provisioning status aren't fully covered: after an interrupted run, check the Pod yourself.
- **Interrupted model calls are charged an estimate.** The gateway streams model responses, so output reaches the agent before its usage is settled. A model call can stream for up to 20 minutes by default (configurable up to an hour). A call interrupted after the provider received the request, for example by pressing Esc, is charged an estimate from its input and elapsed time, which can be more or less than the provider bills. That includes a cancel before the response headers, which a call that doesn't stream receives only when the model has finished. A call is free only when the gateway refuses it before inference or while it waits for its budget, when the request never fully reached the provider, or when the provider answers with an error. A call whose usage can't be estimated, or that a budget cut ends, is charged its whole reservation. Codex and ChatGPT-mode calls carry no output bound, so each one reserves everything left in the budget: a run's `model.inference` budget on Kubernetes, or the execution's allowance from `allowances` on the local service. See [long model calls](docs/guides/model-provider-auth.md#long-model-calls), [interrupted calls](docs/guides/model-provider-auth.md#interrupted-calls) and [calls that aren't charged](docs/guides/model-provider-auth.md#calls-that-arent-charged).
- Resource and usage limits exist, but model-token and cost estimates are **not** hard guarantees against provider spend.
- There's no cluster cap by default. Without a cluster cap, policy caps or a ResourceQuota, many runs can load the single gateway and the API server, share one ChatGPT plan's rate limit and each spend a full budget at once. See [concurrency and gateway capacity](docs/reference/security-hardening.md#concurrency-and-gateway-capacity).
- A sandbox does not guarantee that an agent's answer, code or proposed fix is correct. Review changes before deploying or merging them.

Read the [security hardening guide](docs/reference/security-hardening.md) before using Sproozi with sensitive infrastructure.

## Where this is going

The aim is simple: **your agents, your infrastructure, your rules, and your sessions wherever you want to continue them.**

That opens the door to agents triggered by incidents, pull requests and persistent assistants; people taking over work started by an agent; and, eventually, agents collaborating without having to share one giant set of credentials.

Some of that is still future architecture, not shipped functionality. The [future architecture notes](docs/explanations/future-architecture/README.md) separate planned ideas from the working paths.

## Dig deeper

- [Core concepts](docs/explanations/core-concepts.md) and the [glossary](GLOSSARY.md)
- [Architecture](docs/explanations/system-architecture.md)
- [Capabilities and gateways](docs/explanations/capabilities-and-gateways.md)
- [Architecture decisions](docs/adr/README.md)
- [Full documentation index](docs/README.md)

Sproozi is open source under the [Apache License 2.0](LICENSE) and evolving.
See [contributing](CONTRIBUTING.md) and the [security policy](SECURITY.md). If you're trying to let agents do useful work without handing them the keys to everything, I'd love to hear what you're building.
