# Install on Kubernetes

This guide installs a Sproozi release on a Kubernetes cluster and runs a first
check that needs no model or GitHub account. You need kubectl and a cluster.
You don't need a checkout, a build or an image registry: the release's
manifests pull published images.

To build your own images instead, follow
[Install from a checkout](install-from-checkout.md).

Sproozi is a preview. Use an evaluation cluster first and read
[security hardening](../reference/security-hardening.md) before a longer-lived
deployment. [Core concepts](../explanations/core-concepts.md) explains runs,
runtimes, templates and policies.

## Before you start

You need kubectl 1.31 or later and a Linux cluster (Kind on macOS counts: its
nodes are Linux containers) whose network plugin
enforces NetworkPolicy from the moment a Pod starts, such as Calico or
Cilium. Kind's default network isn't enough: it lets a new Pod's first
traffic through before applying its policy.

## Download the release

Work in a new, empty directory. Set `VERSION` to the
[latest release](https://github.com/andrewmccall/sproozi/releases/latest),
then download the Kubernetes assets and check them:

```sh
VERSION=0.1.0
curl -fsSLO "https://github.com/andrewmccall/sproozi/releases/download/v$VERSION/sproozi_${VERSION}_kubernetes.tar.gz"
curl -fsSLO "https://github.com/andrewmccall/sproozi/releases/download/v$VERSION/checksums.txt"
grep "sproozi_${VERSION}_kubernetes.tar.gz" checksums.txt | shasum -a 256 -c -
tar -xzf "sproozi_${VERSION}_kubernetes.tar.gz"
```

The check prints `OK`. On Linux, `sha256sum -c -` works too. The archive
extracts to `sproozi-release/`.

## Create a Kind cluster

To try Sproozi on your computer, create a Kind cluster with Calico. Skip this
on a cluster you already have. Any cluster name works if `sproozi` is taken;
use the same name when you delete it.

```sh
kind create cluster --name sproozi --config sproozi-release/kind/cluster.yaml
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.30.4/manifests/calico.yaml
kubectl rollout status -n kube-system daemonset/calico-node --timeout=300s
```

Kind selects the new cluster for kubectl. On any cluster, check that
kubectl points at the one you mean before installing:

```sh
kubectl config current-context
kubectl get nodes
```

## Install

Before applying, read `sproozi-release/install/install.yaml`. It creates the
namespaces `sproozi-system` and `sproozi-agents`; the
controller and gateway Deployments in `sproozi-system`; and these cluster-wide permissions:

- The controller reads Pods, ServiceAccounts, NetworkPolicies, ConfigMaps,
  AgentPolicies, AgentRuntimes and AgentTemplates in every namespace, and
  updates and deletes AgentRuns in every namespace. It creates and deletes
  sandbox objects only in `sproozi-agents`. Nothing Sproozi installs creates
  AgentRuns; users and tools create them.
- The gateway reads Pods and their logs, events, ServiceAccounts, Services,
  Deployments, ReplicaSets and StatefulSets, and Sproozi's AgentRuns,
  AgentPolicies and AgentTemplates, in every namespace, and creates
  TokenReviews. An agent sees only what its policy allows through the gateway.
- Admin, editor and viewer ClusterRoles for Sproozi's resources, bound to
  no one until you bind them.

**This grants cluster-wide read of Pods, their logs and ServiceAccounts: use an
evaluation cluster.**

```sh
kubectl apply --server-side --field-manager=sproozi-installer -f sproozi-release/crds.yaml
kubectl wait --for=condition=Established --timeout=60s -f sproozi-release/crds.yaml
kubectl apply --server-side --field-manager=sproozi-installer -k sproozi-release/quickstart-self-managed-ca
kubectl rollout status -n sproozi-system deployment/sproozi-certificates --timeout=180s
kubectl rollout status -n sproozi-system deployment/sproozi-gateway --timeout=180s
kubectl rollout status -n sproozi-system deployment/sproozi-controller-manager --timeout=180s
```

The `quickstart-self-managed-ca` overlay applies `install/install.yaml` plus a
small certificate controller that creates the gateway's private CA. The gateway
may restart a few times while it waits for its first certificate. Apply the
CRDs first, as above: if you apply the overlay before them, the controller and
gateway restart until the CRDs exist. The gateway allows only Kubernetes reads (`kubernetes.read`) until
you enable more.

To use certificates from your own issuer, such as cert-manager, apply
`sproozi-release/quickstart-external-ca` instead; see
[Gateway certificates](../reference/gateway-certificates.md#your-own-certificates)
for what to create first.

## Run the first check

The first check starts a sandbox Pod that lists Sproozi's Pods through the
gateway, tries two things its policy forbids, then waits 60 seconds so you can
inspect it.

```sh
kubectl apply -f sproozi-release/first-check/resources.yaml
kubectl create -f sproozi-release/first-check/agentrun.yaml
kubectl wait -n sproozi-agents pod -l sproozi.com/agentrun=first-check --for=create --timeout=180s
kubectl wait -n sproozi-agents pod -l sproozi.com/agentrun=first-check --for=condition=Ready --timeout=180s
kubectl logs -n sproozi-agents -l sproozi.com/agentrun=first-check -c agent -f
```

The log lists Sproozi's Pods, then prints:

```text
PERMITTED: Pod read succeeded
DENIED: Secret read is outside the policy
DENIED: direct API server path is blocked
First check passed. Keeping this Pod for inspection for 60 seconds
```

The run then becomes `Succeeded`, and shortly after Sproozi removes its Pod,
identity and NetworkPolicy:

```sh
kubectl wait agentrun/first-check -n sproozi-system --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s
kubectl get agentrun first-check -n sproozi-system
kubectl get sa,pods,networkpolicy -n sproozi-agents
```

The last command may list the run's ServiceAccount for up to half a minute;
run it again to see it gone.

To run it again, delete the finished run first:
`kubectl delete agentrun first-check -n sproozi-system`.

## Next steps

- **Run a model.** The [investigation-to-PR demo](pr-demo.md) runs Codex
  against a broken workload and opens a pull request. It needs a model account
  and a GitHub App, and today it runs from a checkout.
- **Limit concurrent runs.** There is no cluster cap by default. Cap the runs
  one policy admits with `maxActiveRuns`; see
  [AgentPolicy](../reference/agentpolicy.md#concurrent-runs).
- **Keep certificates current.** The certificate controller renews the
  gateway's certificate itself. CA rotation is a manual, four-step process; see
  [Gateway certificates](../reference/gateway-certificates.md).
- **Reach a remote executor from your machine.** The release doesn't install
  a remote executor; [Run a task remotely](portable-workflows.md) sets one up
  from a checkout. `sproozi-release/cli-access/` then holds the CLI's
  ServiceAccount and Role, for the executor's namespace `sproozi-sessions`.
  Apply it and create the `sproozi` context; see
  [remote executor access](../reference/remote-executors.md).

## Troubleshooting

| Symptom | Next step |
| --- | --- |
| Nodes are `NotReady` and Pods stay `Pending` | The Kind config disables the default network; install Calico as in [Create a Kind cluster](#create-a-kind-cluster). |
| A Deployment has `ImagePullBackOff` | The nodes can't pull the release images. Check that they can reach `ghcr.io`, and that the node architecture is amd64 or arm64. |
| The gateway has `CrashLoopBackOff`, or its rollout times out | Read `kubectl logs -n sproozi-system deployment/sproozi-gateway --previous`; the message names the Secret, key and fix. |
| The gateway log starts with `WARNING: the gateway can <verb> ConfigMaps in sproozi-system, which hold its own configuration; remove the Role or binding that grants it (ADR 0041)` | A Role or ClusterRole bound to `sproozi-shared-gateway` lets it change its own configuration. Find the binding with `kubectl get rolebindings,clusterrolebindings -A -o wide \| grep sproozi-shared-gateway` and remove the rule or binding that grants that verb on ConfigMaps; the install grants none. The gateway checks once, at startup, so restart it afterwards to confirm the warning is gone. See [ADR 0041](../adr/0041-gateway-state-namespace-and-retained-state-owners.md). |
| `kubectl wait ... --for=create` says the flag is unknown | Use kubectl 1.31 or later. |
| The run stays `Queued` | Read its `Ready` condition. `WaitingForConcurrencyCap` means a concurrency cap is full. |
| The run is `Failed` before a Pod appears | Run `kubectl describe agentrun first-check -n sproozi-system` and read the controller log: `kubectl logs -n sproozi-system deployment/sproozi-controller-manager`. |
| `AlreadyExists` when you create the run | Delete the earlier run first. |
| `UNEXPECTED: direct API server path was reachable` | The network plugin doesn't enforce NetworkPolicy from Pod start. Use Calico or Cilium; see [security hardening](../reference/security-hardening.md). |

## Upgrade

Read the release notes' "Breaking changes" first. Before crossing a minor
version, delete finished AgentRuns and stop running ones while the old
controller still runs, and save your runtimes, policies and templates (see
[the alpha API](../contributing/releases.md#the-alpha-api)). Then apply the
new release's CRDs and overlay as in [Install](#install), and reapply
`cli-access/` if you use it.

`kubectl apply` does not delete objects a new release no longer ships. The
release notes' "Breaking changes" name any you must delete and how.

## Uninstall

Delete the runs in every namespace first, while the controller is still
running: it removes each run's sandbox and then its `sproozi.com/agentrun-cleanup`
finalizer. A run left behind after the controller is gone keeps that
finalizer, and its namespace cannot be deleted until you remove it by hand.

```bash
kubectl delete agentruns --all --all-namespaces --wait=true --timeout=5m
```

Deleting the overlay deletes Sproozi's namespaces and everything in them: the
certificate Secrets and CA key, the gateway's
[BudgetLedger](../reference/budgetledger.md) resources, and any AgentTemplates,
AgentPolicies and AgentRuntimes you created in `sproozi-system`, as the quickstart and examples
do. Export anything you want to keep before you delete it:

```bash
kubectl get agenttemplates,agentpolicies,agentruntimes -n sproozi-system -o yaml > sproozi-resources.yaml
```

```bash
kubectl delete -k sproozi-release/quickstart-self-managed-ca
```

If you installed the remote executor example, delete its namespace too, after
its runs are gone:

```bash
kubectl delete namespace sproozi-sessions --ignore-not-found
```

The CRDs remain, with resources in namespaces you did not delete. To delete
those too:

```sh
kubectl delete -f sproozi-release/crds.yaml
```

If you created a Kind cluster for this guide, delete it instead:
`kind delete cluster --name sproozi`, with the name you created it with.
