# Sproozi terms

These are the words the documentation uses, one per concept. Where code, flags
or older records still use another word, it is listed as "also seen as". For
examples and a reading path, see [core concepts](docs/explanations/core-concepts.md).

## The agent and where it runs

### Harness

The existing agent program that Sproozi launches: Codex, Claude Code, OpenCode
or Hermes. The harness runs the conversation and its tool calls; Sproozi
supplies the environment and access. Selected by `clientConfig.harness`.
Also seen as: client, native client.

### Sandbox

The isolated environment a harness runs in. On Kubernetes it is a disposable
agent Pod in `sproozi-agents` with its own run identity, NetworkPolicy and input
contract; on Docker it is a controlled container. It is not the `Sandbox`
resource from the Kubernetes Agent Sandbox project; see
[cluster dependencies](docs/reference/dependencies.md).
Also seen as: agent Pod (the Kubernetes form), worker Pod.

### Gateway

The trusted proxy that every request from a sandbox to an outside service goes
through. It holds provider credentials, identifies the run and checks each
request against current policy. See
[capabilities and gateways](docs/explanations/capabilities-and-gateways.md).

### Control connection

The `sproozi` MCP server that every execution reaches on its own gateway. Its
`diagnose` tool explains the execution's recent gateway refusals. It needs no
capability and grants none. See
[Sproozi integration and diagnose](docs/reference/sproozi-integration.md).

### Denial journal

The gateway's bounded, in-memory record of each execution's recent refused
requests, kept from its audit records. `diagnose` reads it.

### Sproozi integration

What Sproozi adds to every harness it launches: environment instructions, the
`sproozi-access` skill and the control connection. It is guidance, not
authority. Also seen as: managed harness configuration.

### Capability

A named kind of access a run can request, such as `kubernetes.read`,
`github.pull_request`, `model.inference`, `network.egress` or `mcp.<server>`.
Policy decides whether it is allowed and sets its limits. See
[permissions and capabilities](docs/reference/permissions-and-capabilities.md).

### Budget

A usage limit for one capability in one run, set in `AgentPolicy.spec.budgets`:
tokens or requests (`maxUnits`) and cost (`maxCostMicros`). Budgets are not hard
spending ceilings; see
[security hardening](docs/reference/security-hardening.md#model-budgets-are-not-hard-spending-ceilings).

### Allowance

The total usage limit, per capability, granted to one execution and shared by
all of its visits. For local executions it comes from the home daemon's
`allowances` configuration. A move doesn't refill it; a new execution gets a
fresh one.

## Kubernetes resources

### AgentRuntime

Administrator-owned configuration for how the agent runs: image, command,
resources, security settings and gateway endpoint. See
[AgentRuntime](docs/reference/agentruntime.md).

### AgentPolicy

Administrator-owned rules for capabilities, resource access, budgets and
duration. See [AgentPolicy](docs/reference/agentpolicy.md).

### AgentTemplate

A named type of task that selects a runtime and a policy and supplies trusted
instructions. See [AgentTemplate](docs/reference/agenttemplate.md).

### AgentRun

One request to run a task through an AgentTemplate, with an explicit list of
requested capabilities. Also called a run. See [AgentRun](docs/reference/agentrun.md).

### Admission

The check Sproozi makes before a run, or a visit, starts: the requested
capabilities, resources and deadline must fit current policy.

## Sessions and continuing work

### Session

A Sproozi record that tracks one harness conversation and its Git workspace
across executions. Its `id` is stable. The harness's own conversation ID
(`threadId`) is separate. Also seen as: Sproozi session.

### Execution

One approved period of work on a session, with its own permissions, allowance
and deadline. `resume` starts a new execution; `move` keeps the current one.
Its ID is `executionId`. Also seen as: Sproozi execution.

### Placement

Where an execution runs: local Docker, managed by the home daemon, or a
Kubernetes cluster reached through a profile service.

### Visit

One stay of an execution at one placement. Each `move` ends one visit and
starts another, with fresh admission, gateway access and a new sandbox. The
execution's deadline and allowance carry across visits. Its ID is
`allocationId`. Also seen as: allocation.

### Writer

The running sandbox process that can change a session's conversation or
workspace. Before another visit starts, Sproozi must confirm the previous writer
has stopped. Status reports the exact writer as `incarnation`.

### Checkpoint

The saved conversation state and Git workspace of a stopped harness. It carries
work, never permissions or credentials. See
[ADR 0008](docs/adr/0008-selected-native-and-git-checkpoints.md).
Also seen as: Sproozi checkpoint.

### Finalized usage

The usage of a stopped writer, recorded so that it can no longer change.
Sproozi needs finalized usage before it captures a checkpoint or starts another
visit. Also seen as: sealed usage, sealed accounting (in ADR 0009 and other
earlier records).

### Workspace revision

The Git commit recorded in a checkpoint. A new session can start from that exact
commit with a clean checkout and a fresh conversation. See
[Projects and workspace revisions](docs/reference/projects-and-revisions.md).

### Project

An administrator-configured group of retained sessions with an approved
template, capability limits and explicit membership. See
[Projects and workspace revisions](docs/reference/projects-and-revisions.md).

## Owners and services

### Home daemon

The `sproozi serve` process on your own machine. It owns local Docker
placements, their gateway and accounting, and the sessions you have pulled.
Also seen as: home (in CLI output and flags), home service.

### Profile service

The `session-profile` program. It serves one administrator-approved Kubernetes
placement, holds that cluster's credentials and hosts a catalogue of remote
work. Also seen as: remote profile, fixed remote profile.

### Executor

The owner a CLI command addresses, selected with `--executor NAME`. `docker`
means the home daemon; any other name is a profile service from the home
configuration.

### Catalogue

The list of sessions that one owner holds. `sproozi sessions` combines the
catalogues of the home daemon and configured executors. See
[session commands](docs/reference/session-commands.md#read-status-and-discovery).

### Coordinator

A persistent assistant, currently Hermes, that delegates tasks to Sproozi
through a task service. It runs outside the sandboxes it starts.

### Task service

A private MCP service that turns a coordinator's request into an approved run.
The disposable-job service keeps status and a final answer; the retained-work
service also keeps a checkpoint for later resume. See
[orchestration](docs/reference/orchestration.md).
