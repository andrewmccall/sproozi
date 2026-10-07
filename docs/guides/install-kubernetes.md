# Install on Kubernetes and run your first Pod

This guide installs Sproozi from this checkout on an existing cluster, then
creates an `AgentRun`. Its Pod uses kubectl to list Pods through the gateway,
waits 90 seconds so you can inspect it, and exits. This checks the controller,
run identity, gateway and completion path without model or GitHub credentials.
It does not run an LLM. For that, continue with the
[Kind AgentRun-to-PR demo](getting-started.md).

Use an evaluation cluster for your first installation. The current release is a
preview; [security hardening](../reference/security-hardening.md) describes its
limits and the controls needed for a longer-lived deployment.

## 1. Check the prerequisites and select the cluster

You need:

- An existing Linux Kubernetes cluster, with a CNI that enforces NetworkPolicy.
  Use your cluster's CNI; do not install the Kind demo's Calico manifest over it.
- Administrator access to create CRDs, ClusterRoles, namespaces and workloads.
- An image registry that your build machine can push to and your cluster nodes
  can pull from. If images are private, use the optional registry steps below.
- Docker with Buildx, Git, kubectl, Go 1.26.6, Make, OpenSSL and Python 3 on your computer.

The repository's Kind checks use Kubernetes 1.33.1. This is a tested baseline,
not a compatibility claim for every Kubernetes version. The tools image uses
kubectl 1.34.1; check the
[kubectl version skew policy](https://kubernetes.io/releases/version-skew-policy/#kubectl)
against your cluster version before using it.

Start from the repository root. If you have not cloned it yet:

```sh
git clone https://github.com/andrewmccall/sproozi.git
cd sproozi
```

Select your existing kubeconfig and confirm the target before installing:

```sh
export KUBECONFIG=/absolute/path/to/your/cluster.kubeconfig
kubectl config current-context
kubectl cluster-info
kubectl get nodes -o wide
```

The commands below install into `sproozi-system`, `sproozi-agents` and
`sproozi-webhook-state`. Use a cluster without an existing Sproozi installation
for this walkthrough. Keep the chosen `KUBECONFIG` exported throughout it.

## 2. Build and publish the images

The controller, gateway and webhook share one image built from the repository's
root Dockerfile. The first Pod uses the demo tools image, but runs kubectl rather
than Codex.

Replace the example registry path with your own. Authenticate to that registry
using `docker login` before building:

```sh
export SPROOZI_IMAGE_PREFIX=ghcr.io/YOUR-ACCOUNT
export SPROOZI_CONTROLLER_TAG="$SPROOZI_IMAGE_PREFIX/sproozi:getting-started"
export SPROOZI_TOOLS_TAG="$SPROOZI_IMAGE_PREFIX/sproozi-tools:getting-started"
export SPROOZI_PLATFORMS=linux/amd64,linux/arm64

docker buildx build --platform "$SPROOZI_PLATFORMS" \
  --tag "$SPROOZI_CONTROLLER_TAG" --push .
docker buildx build --platform "$SPROOZI_PLATFORMS" \
  --file hack/demo/codex.Dockerfile --tag "$SPROOZI_TOOLS_TAG" \
  --push hack/demo

docker buildx imagetools inspect "$SPROOZI_CONTROLLER_TAG"
docker buildx imagetools inspect "$SPROOZI_TOOLS_TAG"
```

You can set `SPROOZI_PLATFORMS` to just `linux/amd64` or `linux/arm64` if all your
nodes use that architecture. The tools Dockerfile supports those two architectures.
The first build downloads dependencies and compiles the pinned tools.

Copy the top-level `Digest` from each inspection into these values, replacing
`<controller-digest>` and `<tools-digest>` with the 64 hexadecimal characters:

```sh
export IMG="$SPROOZI_IMAGE_PREFIX/sproozi@sha256:<controller-digest>"
export SPROOZI_TOOLS_IMAGE="$SPROOZI_IMAGE_PREFIX/sproozi-tools@sha256:<tools-digest>"
```

The runtime requires a digest-pinned image. The Kind demo's `localhost:5001`
image is local to your computer and must not be used on remote cluster nodes.

## 3. Create the namespaces and gateway certificate

```sh
kubectl create namespace sproozi-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k config/agent-rbac
mkdir -p .local/install/tls
```

The gateway terminates both the proxy connection and the inspected Kubernetes
connection. Its certificate therefore needs the gateway hostname and
`kubernetes.default.svc`. For this evaluation, create a private CA and a
30-day certificate:

```sh
umask 077
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -subj '/CN=Sproozi evaluation CA' \
  -keyout .local/install/tls/ca.key -out .local/install/tls/ca.crt
openssl req -newkey rsa:2048 -nodes \
  -subj '/CN=sproozi-gateway.sproozi-system.svc' \
  -keyout .local/install/tls/tls.key -out .local/install/tls/tls.csr
cat > .local/install/tls/extensions.cnf <<'EOF'
subjectAltName=DNS:sproozi-gateway.sproozi-system.svc,DNS:sproozi-gateway.sproozi-system.svc.cluster.local,DNS:kubernetes.default.svc
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EOF
openssl x509 -req -days 30 -sha256 \
  -in .local/install/tls/tls.csr \
  -CA .local/install/tls/ca.crt -CAkey .local/install/tls/ca.key -CAcreateserial \
  -extfile .local/install/tls/extensions.cnf -out .local/install/tls/tls.crt

kubectl create secret tls sproozi-gateway-tls -n sproozi-system \
  --cert=.local/install/tls/tls.crt --key=.local/install/tls/tls.key \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create configmap sproozi-sandbox-trust -n sproozi-agents \
  --from-file=ca-bundle.pem=.local/install/tls/ca.crt --dry-run=client -o json | \
  python3 -c 'import json,sys; obj=json.load(sys.stdin); obj["immutable"]=True; json.dump(obj,sys.stdout)' | \
  kubectl apply -f -
```

Only the public CA goes into the sandbox trust ConfigMap. The gateway Secret
contains its certificate and private key. Keep the CA private key on your computer.
Reuse these files if you repeat the install. The trust ConfigMap is immutable, so replacing its CA needs
a new ConfigMap name and matching runtime configuration. For a persistent
installation, arrange certificate renewal before expiry.

### If your images are private

Skip this step if your nodes can already pull both images. Otherwise, sign in
with a registry credential that can pull them. Replace `ghcr.io` if you use
another registry. A separate Docker configuration keeps the credential in a
local file that kubectl can turn into a pull Secret. The initial `auths` entry
keeps this configuration independent of your normal credential helper:

```sh
mkdir -p .local/install/registry
printf '%s\n' '{"auths":{"ghcr.io":{}}}' > .local/install/registry/config.json
docker --config "$PWD/.local/install/registry" login ghcr.io
for namespace in sproozi-system sproozi-agents; do
  kubectl create secret generic sproozi-registry -n "$namespace" \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=.dockerconfigjson=.local/install/registry/config.json \
    --dry-run=client -o yaml | kubectl apply -f -
done
```

For GHCR, use a credential with package-read permission, as described in
[GitHub's registry authentication guide](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-to-the-container-registry).
The two Secrets let Kubernetes pull images in each namespace. The Pod does not
mount the registry credential. Add the deployment and runtime references in the
following steps.

## 4. Install Sproozi with Kubernetes reads enabled

The base gateway manifest enables model and GitHub modules as well. Those
modules need provider credentials. Create a local Kustomize overlay that enables
only `kubernetes.read` for this first run:

```sh
make kustomize
cat > .local/install/kustomization.yaml <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../../config/install
patches:
- target:
    group: apps
    version: v1
    kind: Deployment
    name: sproozi-gateway
  patch: |-
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: sproozi-gateway
    spec:
      template:
        spec:
          containers:
          - name: shared-gateway
            env:
            - name: SPROOZI_ENABLED_CAPABILITIES
              value: kubernetes.read
EOF
(cd .local/install && ../../bin/kustomize edit set image controller="$IMG")
bin/kustomize build .local/install > .local/install/install.yaml
```

If you created `sproozi-registry`, add it to the trusted Deployments before
installing. Public-image users can skip this block:

```sh
cat > .local/install/private-registry-patch.yaml <<'EOF'
- op: add
  path: /spec/template/spec/imagePullSecrets
  value:
  - name: sproozi-registry
EOF
(cd .local/install && ../../bin/kustomize edit add patch \
  --group apps --version v1 --kind Deployment --path private-registry-patch.yaml)
bin/kustomize build .local/install > .local/install/install.yaml
```

Review `.local/install/install.yaml`, especially the image references and
cluster-wide RBAC. Then install the CRDs and workloads:

```sh
bin/kustomize build config/crd | \
  kubectl apply --server-side --field-manager=sproozi-installer -f -
kubectl wait --for=condition=Established --timeout=60s \
  crd/agentruntimes.sproozi.com crd/agentpolicies.sproozi.com \
  crd/agenttemplates.sproozi.com crd/agentruns.sproozi.com
kubectl apply --server-side --field-manager=sproozi-installer -f .local/install/install.yaml
kubectl rollout status -n sproozi-system deployment/sproozi-controller-manager --timeout=180s
kubectl rollout status -n sproozi-system deployment/sproozi-gateway --timeout=180s
kubectl rollout status -n sproozi-system deployment/sproozi-webhook --timeout=180s
kubectl get pods -n sproozi-system
```

All three Deployments should become available. No model or GitHub credential
Secret is needed for this overlay. The install includes the event webhook, but
this guide submits runs manually and configures no event receiver. It requires
neither cert-manager nor Prometheus.

## 5. Create the runtime, policy and template

The [first-run resources](../../examples/getting-started/resources.yaml) grant
only Pod reads in `sproozi-system`, with a five-minute execution deadline. Render
your tools image into the example and apply it:

```sh
python3 - <<'PY' > .local/install/first-run-resources.yaml
import os
import re
from pathlib import Path

image = os.environ['SPROOZI_TOOLS_IMAGE']
if not re.fullmatch(r'[A-Za-z0-9./_:-]+@sha256:[a-f0-9]{64}', image):
    raise SystemExit('SPROOZI_TOOLS_IMAGE must be repository@sha256:<digest>')
print(Path('examples/getting-started/resources.yaml').read_text().replace('SPROOZI_TOOLS_IMAGE', image))
PY
kubectl apply -f .local/install/first-run-resources.yaml
kubectl get agentruntimes,agentpolicies,agenttemplates -n sproozi-system
```

If you created the private registry Secret, reference it in the runtime too:

```sh
kubectl patch agentruntime getting-started -n sproozi-system --type=merge \
  -p '{"spec":{"podTemplate":{"spec":{"imagePullSecrets":[{"name":"sproozi-registry"}]}}}}'
```

The trusted runtime command configures the authenticated proxy. Sproozi supplies
the per-run identity token, kubeconfig, public trust bundle and writable scratch
volumes. The Pod uses one container named `agent` so the current controller can
observe its exit code.

## 6. Submit the run and read its output

```sh
kubectl create -f examples/getting-started/agentrun.yaml
kubectl wait -n sproozi-system agentrun/getting-started \
  --for=jsonpath='{.status.phase}'=Running --timeout=180s
SPROOZI_FIRST_POD="$(kubectl get agentrun getting-started -n sproozi-system \
  -o jsonpath='{.status.identity.sandboxName}')"
kubectl wait -n sproozi-agents "pod/$SPROOZI_FIRST_POD" \
  --for=create --timeout=180s
kubectl wait -n sproozi-agents "pod/$SPROOZI_FIRST_POD" \
  --for=condition=Ready --timeout=180s
kubectl logs -n sproozi-agents "$SPROOZI_FIRST_POD" -c agent -f
kubectl wait -n sproozi-system agentrun/getting-started \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
kubectl get agentrun getting-started -n sproozi-system -o yaml
```

The log should print `Listing Pods through the Sproozi gateway`, a table of
Sproozi's Pods, then `Read succeeded`. The command keeps the Pod alive for 90
seconds. During that interval, use another terminal with the same kubeconfig:

```sh
kubectl get pods,serviceaccounts,networkpolicies -n sproozi-agents
kubectl logs -n sproozi-system deployment/sproozi-gateway --tail=30
```

After the shell exits with zero, the run becomes `Succeeded` and Sproozi removes
its Pod, identity, contract and NetworkPolicy. The run record remains for 24
hours. Save logs while the Pod is running if you want to keep them.

To repeat, delete the terminal run and submit it again:

```sh
kubectl delete agentrun getting-started -n sproozi-system
kubectl create -f examples/getting-started/agentrun.yaml
```

## Troubleshooting

| Symptom | Next step |
| --- | --- |
| Deployment has `ImagePullBackOff` | Describe its Pod. Check registry visibility, image digest, node architecture and image pull credentials. |
| Gateway has `CrashLoopBackOff` | Read its logs. Check the TLS Secret and confirm the overlay sets `SPROOZI_ENABLED_CAPABILITIES=kubernetes.read`. |
| Run stays `Queued` | Another run may occupy the controller's single active-run slot. Inspect `kubectl get agentruns -A -o yaml`. |
| Run becomes `Failed` before a Pod appears | Describe the run and inspect controller logs. Check the image digest, references, trust ConfigMap and policy resource bounds. |
| Read fails with a certificate error | Check that the certificate covers both required hostnames and the public trust bundle belongs to the same CA. |
| Read times out | Check the cluster's DNS and CNI policies, including agent-to-gateway traffic on TCP 8443. |
| Logs or Pod are already gone | Inspect the retained run's phase and conditions. Submit a fresh run and attach to its logs immediately. |

Useful diagnostic commands:

```sh
kubectl describe agentrun getting-started -n sproozi-system
kubectl logs -n sproozi-system deployment/sproozi-controller-manager --tail=100
kubectl logs -n sproozi-system deployment/sproozi-gateway --tail=100
kubectl get events -n sproozi-agents --sort-by=.metadata.creationTimestamp
```

## Next steps and cleanup

For an LLM-driven investigation, follow the [Kind demo](getting-started.md) first.
To add those capabilities to this cluster, configure the trusted
[provider Secrets and certificate hostnames](../reference/required-secrets-and-config.md),
enable the gateway modules, and use a runtime, policy and template that select
them. Restart the gateway after changing its configuration. Copying only the
demo's `AgentRun` does not install those dependencies.

Remove the tutorial resources when finished:

```sh
kubectl delete agentrun getting-started -n sproozi-system --ignore-not-found
kubectl delete -f .local/install/first-run-resources.yaml
```

If this installation was created solely for the tutorial and has no other runs,
you can also uninstall it. The following removes Sproozi's CRDs and all their
custom resources, as well as its namespaces and trusted Secrets:

```sh
kubectl delete -f .local/install/install.yaml
kubectl delete -k config/agent-rbac
```

Keep the certificate files and rendered manifests if you intend to reinstall
with the same configuration.
