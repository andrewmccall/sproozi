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

You need a Linux cluster whose network plugin enforces NetworkPolicy, and
administrator access to it.

To try Sproozi on your computer, create a Kind cluster. Kind's default network
enforces NetworkPolicy (tested with Kind 0.33):

```sh
kind create cluster --name sproozi
```

Kind selects the new cluster for kubectl. For any cluster, check that kubectl
points at the one you mean before installing:

```sh
kubectl config current-context
kubectl get nodes
```

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

## Install

Before applying, read `sproozi-release/install/install.yaml`. It creates
namespaces whose names start with `sproozi-`, and ClusterRoles that let the
controller read Pods, ServiceAccounts, NetworkPolicies and ConfigMaps across
the cluster. The controller writes only in `sproozi-agents`.

```sh
kubectl apply --server-side --field-manager=sproozi-installer -f sproozi-release/crds.yaml
kubectl wait --for=condition=Established --timeout=60s crd --all
kubectl apply --server-side --field-manager=sproozi-installer -k sproozi-release/quickstart-self-managed-ca
kubectl rollout status -n sproozi-system deployment/sproozi-certificates
kubectl rollout status -n sproozi-system deployment/sproozi-gateway --timeout=180s
kubectl rollout status -n sproozi-system deployment/sproozi-controller-manager
```

This overlay includes a small certificate controller that creates the gateway's
private CA. The gateway may restart a few times while it waits for its
first certificate. The gateway allows only Kubernetes reads (`kubernetes.read`) until
you enable more.

To use certificates from your own issuer, such as cert-manager, apply
`sproozi-release/quickstart-external-ca` instead; see
[Gateway certificates](../reference/gateway-certificates.md#your-own-certificates)
for what to create first.

## Run the first check

The first check starts an agent Pod that lists Sproozi's Pods through the
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

The run then becomes `Succeeded` and Sproozi removes its Pod, identity and
NetworkPolicy:

```sh
kubectl get agentrun first-check -n sproozi-system
```

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

## Troubleshooting

| Symptom | Next step |
| --- | --- |
| A Deployment has `ImagePullBackOff` | The nodes can't pull the release images. Check that they can reach `ghcr.io`, and that the node architecture is amd64 or arm64. |
| The gateway has `CrashLoopBackOff`, or its rollout times out | Read `kubectl logs -n sproozi-system deployment/sproozi-gateway --previous`; the message names the Secret, key and fix. |
| `kubectl wait ... --for=create` says the flag is unknown | Use kubectl 1.31 or later. |
| The run stays `Queued` | Read its `Ready` condition. `WaitingForConcurrencyCap` means a concurrency cap is full. |
| The run is `Failed` before a Pod appears | Run `kubectl describe agentrun first-check -n sproozi-system` and read the controller log: `kubectl logs -n sproozi-system deployment/sproozi-controller-manager`. |
| `AlreadyExists` when you create the run | Delete the earlier run first. |

## Uninstall

Delete the runs first, so the controller removes their sandboxes. Then delete
the overlay you applied:

```sh
kubectl delete agentruns --all -n sproozi-system
kubectl delete -k sproozi-release/quickstart-self-managed-ca
```

This deletes Sproozi's namespaces, including the certificate Secrets and the
CA key. It keeps the CRDs and any templates, policies and runtimes you created.
To delete those too:

```sh
kubectl delete -f sproozi-release/crds.yaml
```

If you created a Kind cluster for this guide, delete it instead:
`kind delete cluster --name sproozi`.
