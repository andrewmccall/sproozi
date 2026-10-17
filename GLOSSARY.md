# Sproozi terms

These names describe Sproozi resources and managed work. For examples and a
reading path, see [core concepts](docs/explanations/core-concepts.md).

## Kubernetes resources

**AgentRuntime**:
Administrator-owned configuration for the agent's container environment and launch.

**AgentPolicy**:
Administrator-owned rules for capabilities, resource access, usage and duration.

**AgentTemplate**:
A named type of task that selects a runtime, a policy and trusted instructions.

**AgentRun**:
One request to execute a task through an AgentTemplate, with an explicit capability list.

## Managed work

**Sproozi session**:
A record that tracks an agent conversation and its Git workspace across executions.
Its ID is separate from the harness's native session or conversation ID.

**Sproozi execution**:
One period of work with an approved set of permissions, usage allowance and deadline.

**Project**:
An administrator-configured group of retained sessions with approved capabilities,
a template and explicit installation membership.
