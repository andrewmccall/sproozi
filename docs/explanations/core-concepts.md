# Core concepts

Sproozi runs an agent with access that you approve. It keeps provider credentials
outside the agent's container and checks service requests through a gateway.

Start with a task, such as investigating a failed Deployment. Choose how the
agent runs, set its permissions, then submit the task. The concepts below
describe those parts and how they fit together.

## The agent and its environment

The **harness** is the program that runs the agent. Codex, Claude Code, OpenCode
and Hermes are supported harnesses. The harness manages the conversation and
tool calls.

The **AgentRuntime** resource holds Kubernetes configuration for the container
image, launch command, resources and security settings. An administrator controls
it. Docker and Kubernetes provide the environments where containers run.

The **sandbox** is the isolated environment where the harness runs. For an
ordinary Kubernetes run, it is a disposable Pod with its own identity and network
policy. Sproozi creates these Pods directly; see
[cluster dependencies](../reference/dependencies.md).

The **gateway** checks requests from the sandbox to approved services. It holds
provider credentials and checks the run's identity, permissions and current
status. The network must enforce this route. See
[security hardening](../reference/security-hardening.md) for the required controls.

## Tasks and permissions

A **capability** names an action that a run can request. For example,
`kubernetes.read` permits approved reads, while `github.pull_request` permits
approved pull-request work. Policy sets the allowed resources and other limits.
Installing a tool does not give it permission to use a service.

Four Kubernetes resources describe a task:

| Resource | What it defines | Who controls it |
| --- | --- | --- |
| `AgentRuntime` | How the agent runs. | Administrator |
| `AgentPolicy` | Which capabilities, resources, usage and duration are allowed. | Administrator |
| `AgentTemplate` | A runtime, a policy and trusted instructions for a type of task. | Administrator |
| `AgentRun` | One task, a template reference and requested capabilities. | Caller |

For example, an investigation template can select Codex and a policy that permits
Pod reads in one namespace. A run supplies the incident details and requests
`kubernetes.read` and `model.inference`. It cannot select a different image or
give itself permission to write to the cluster.

Sproozi checks the request before starting the run. This check is called
**admission**. The gateway checks permissions again when the agent calls a
service. Policy changes can therefore affect later calls from an active run.

An ordinary run succeeds when its `agent` container exits with code zero after
the Pod becomes terminal. That status does not prove that the agent's diagnosis
or proposed change is correct. Review the result separately.

See [the run lifecycle](run-lifecycle.md) for admission, completion and cleanup,
or [capabilities and gateways](capabilities-and-gateways.md) for permission checks.

## Continuing work

A single task can finish in one disposable run. If you want to continue the
conversation later, use a managed **Sproozi session**. It tracks the agent
conversation and Git workspace across executions.

| Concept | Meaning |
| --- | --- |
| Sproozi session | The record tracking the conversation and workspace, with a stable Sproozi session ID. |
| Sproozi execution | One period of work with its own approved permissions, usage allowance and deadline. |
| Placement | Where an execution runs, such as local Docker or remote Kubernetes. |
| Sproozi checkpoint | The saved conversation and Git workspace from a stopped agent. |

The harness also has a native conversation ID. It is separate from the Sproozi
session ID and execution ID.

These operations serve different purposes:

- `pull` imports stopped remote work. It does not start the agent.
- `resume` starts a new execution with current approval. It restores the same
  supported native conversation and unfinished workspace.
- `move` transfers an active execution between placements. Its original deadline
  and total allowance still apply.
- `stop` stops local work and saves supported state for a later resume.

A checkpoint contains work, not permission to run it. Sproozi creates fresh
access at the destination. Before a move can start another agent, Sproozi must
confirm that the source has stopped and finish its usage accounting.

Managed remote tasks currently use Codex. Controlled interactive moves support
Codex and Claude Code. Support for a harness in ordinary
Kubernetes runs does not imply support for session movement. See
[harness support](../reference/harnesses.md) for the exact limits.

## Grouping work and reusing a commit

A **Project** groups retained sessions under administrator configuration. It
selects an approved template, limits capabilities and names the installations
that can access the work. Local resume requires separate local approval.

A **workspace revision** is the Git commit recorded in a checkpoint. You can
start a fresh conversation at that exact commit. This operation uses a clean
checkout. It does not restore the old conversation or unfinished file changes.

See [Projects and workspace revisions](../reference/projects-and-revisions.md)
when you need these options.

## Delegating through an assistant

A persistent Hermes assistant can submit tasks through MCP, the Model Context
Protocol. The task service selects an approved workflow and Sproozi runs the
worker. Hermes keeps its own conversation, memory and schedules.

There are two task services. The disposable-job service retains status and an
optional final answer. The retained-work service also saves supported native
conversation and workspace state for later resume. Choose the
[Hermes job guide](../guides/hermes-orchestration.md) or the
[Hermes with retained work](../guides/retained-work-kubernetes.md) guide for the
workflow you need.

## Choose a next step

- [Getting started](../guides/getting-started.md) runs the Kind demo.
- [Install on Kubernetes](../guides/install-kubernetes.md) runs a first Pod on an existing cluster.
- [Remote work and local resume](../guides/portable-workflows.md) starts a remote task that you can continue locally.
- [System architecture](system-architecture.md) explains the components and trust boundaries.
