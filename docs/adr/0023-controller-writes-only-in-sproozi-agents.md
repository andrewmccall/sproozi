# 0023: The controller writes sandbox objects only in sproozi-agents

Status: accepted
Date: 2026-10-18

## Problem and constraints

The controller's ClusterRole granted create and delete on Pods,
ServiceAccounts, ConfigMaps and NetworkPolicies in every namespace, and also
create and delete on Roles and RoleBindings and `get` on `pods/log`. No code
created RBAC objects or read Pod logs. Every sandbox object the controller
writes lives in `sproozi-agents`.

The controller parses untrusted input: AgentRun specs and sandbox termination
messages. A flaw there would act with its RBAC. Creating a Pod in any
namespace is enough to mount any Secret or a host path on a node without Pod
Security enforcement, so the old binding amounted to cluster administration.

The controller still needs cluster-wide reads. It watches Pods, its cached
client starts informers for ServiceAccounts and NetworkPolicies, and a
managed run's registry lookup reads a ConfigMap in the run's own namespace.

## Decision

The cluster-wide `controller-manager-role` keeps only reads: `get`, `list` and
`watch` on Pods, ServiceAccounts and NetworkPolicies, and `get` on ConfigMaps.
The Role and RoleBinding rules and `pods/log` are removed, from both the
hand-written role and the `+kubebuilder:rbac` markers.

A namespaced Role, `controller-sandbox-writer` in `sproozi-agents`, grants the
writes: create and delete on Pods, ServiceAccounts and ConfigMaps (and `get` on
ConfigMaps), and create, update and delete on NetworkPolicies. It replaces the
earlier `controller-contract-writer` Role. It is installed from
`config/agent-rbac`, not generated from markers, because a namespace marker
would be rewritten into `sproozi-system` by the default kustomization.

## Alternatives

- **Keep the cluster-wide writes.** Simplest, but a controller flaw could
  create privileged Pods in any namespace.
- **Run the controller only in `sproozi-agents`.** It must read AgentRuns,
  templates, policies and registry ConfigMaps in run namespaces, so a
  single-namespace manager cannot serve it.
- **Generate the namespaced Role from markers.** Markers cannot express a
  Role in a namespace other than the manager's under the current kustomize
  layout.

## Tradeoffs and consequences

A Pod the controller creates can still mount any Secret in `sproozi-agents`,
so that namespace must hold no Secrets beyond what sandboxes are meant to see.
Adding a new kind of sandbox object, or writing in another namespace, now needs
a matching rule in `controller_sandbox_role.yaml`; `test/manifests` fails if
write verbs reappear in the ClusterRole.

## Revisit conditions

Revisit if sandboxes are placed in more than one namespace, if the controller
starts writing objects outside `sproozi-agents`, or if the registry lookup
moves into `sproozi-agents` and the cluster-wide ConfigMap read can go.

## Links

- [Permissions and capabilities](../reference/permissions-and-capabilities.md)
- [Security hardening](../reference/security-hardening.md)
- `config/rbac/controller_manager_role.yaml`
- `config/agent-rbac/controller_sandbox_role.yaml`
- `test/manifests/agent_rbac_test.go`
