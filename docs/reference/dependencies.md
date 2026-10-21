# Cluster dependencies

This page lists what a Kubernetes cluster needs before Sproozi can run an agent.
`make demo` installs all the required pieces in its dedicated Kind cluster.
On an existing cluster, follow [Install on Kubernetes](../guides/install-kubernetes.md).

| Dependency | Required? | How it is supplied |
| --- | --- | --- |
| Sproozi CRDs: `AgentRuntime`, `AgentPolicy`, `AgentTemplate`, `AgentRun`, `BudgetLedger` | Yes | A release ships the five definitions as `sproozi-release/crds.yaml`; from a checkout, `make install` installs them. |
| A CNI that enforces NetworkPolicy | Yes | Any enforcing CNI works. The Kind demo downloads pinned Calico 3.30.4, which installs its controller, `crd.projectcalico.org` CRDs and bundled `AdminNetworkPolicy` and `BaselineAdminNetworkPolicy` definitions under `policy.networking.k8s.io`. |
| Model account and GitHub App | Only for agents that call a model or open PRs | External services, configured in the [investigation-to-PR demo](../guides/pr-demo.md) or as [gateway Secrets](required-secrets-and-config.md). |
| cert-manager | No | The demo creates the gateway certificate with OpenSSL; the install guide's self-managed CA controller creates it in the cluster. |
| Prometheus Operator and monitoring CRDs | No | Only the optional monitoring overlays use them. |
| gVisor, Kata Containers or another `RuntimeClass` | No | Optional. Select one with [`spec.runtimeClassName`](agentruntime.md#runtime-class). The demo uses Kind's default runtime. |
| Kubernetes Agent Sandbox controller and CRDs | No | See below. |

## Kubernetes Agent Sandbox is not required

On Kubernetes, a Sproozi [sandbox](../../GLOSSARY.md#sandbox) is a disposable
Pod in `sproozi-agents`, with its own run identity, NetworkPolicy and input
contract. Sproozi creates these Pods directly.

It does not use the `Sandbox` resource from the separate
[Kubernetes Agent Sandbox project](https://github.com/kubernetes-sigs/agent-sandbox).
You don't need that project's `Sandbox`, `SandboxTemplate`, `SandboxClaim` or
`SandboxWarmPool` CRDs or controllers. Installing them does not switch Sproozi
to use them. Integration with Agent Sandbox is being evaluated and hasn't been
decided.

## Tested versions

The repository's Kind checks use Kubernetes 1.33.1. This is a tested baseline,
not a compatibility claim for every Kubernetes version. The demo tools image
uses kubectl 1.34.1; check the
[kubectl version skew policy](https://kubernetes.io/releases/version-skew-policy/#kubectl)
against your cluster.
