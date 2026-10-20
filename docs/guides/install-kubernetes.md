# Install on Kubernetes and run your first Pod

This guide installs Sproozi from this checkout on an existing cluster, then
creates an `AgentRun`. Its Pod uses kubectl to list Pods through the gateway,
waits 90 seconds so you can inspect it, and exits. This checks the controller,
run identity, gateway and completion path without model or GitHub credentials.
It does not run a model. [Run a model on this cluster](#7-run-a-model-on-this-cluster)
then adds model and GitHub credentials and runs the investigation-to-PR demo
from [Getting started](getting-started.md) on the same installation.

Everything here runs from a clone of this repository with Make. You don't need
the `sproozi` CLI.

Use an evaluation cluster for your first installation. The current release is a
preview; [security hardening](../reference/security-hardening.md) describes its
limits and the controls needed for a longer-lived deployment.

Read [core concepts](../explanations/core-concepts.md) if you are new to runs,
runtimes, templates and policies.

## 1. Check the prerequisites and select the cluster

You need:

- An existing Linux Kubernetes cluster, with a CNI that enforces NetworkPolicy.
  Use your cluster's CNI; do not install the Calico manifest from Getting started over it.
- Administrator access to create CRDs, ClusterRoles, namespaces and workloads.
- An image registry that your build machine can push to and your cluster nodes
  can pull from. If images are private, use the optional registry steps below.
- Docker with Buildx, Git, kubectl, Go 1.26 or later, Make, OpenSSL and Python 3 on your computer.
- Optional, for [step 7](#7-run-a-model-on-this-cluster) only: the
  [GitHub CLI](https://cli.github.com/), signed in with `gh auth login`.

[Cluster dependencies](../reference/dependencies.md) lists what the cluster
needs and the Kubernetes and kubectl versions that have been tested.

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

The commands below install into `sproozi-system`, `sproozi-agents`,
`sproozi-webhook-state` and `sproozi-webhooks`. Use a cluster without an existing Sproozi installation
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
  --push .

docker buildx imagetools inspect "$SPROOZI_CONTROLLER_TAG"
docker buildx imagetools inspect "$SPROOZI_TOOLS_TAG"
```

You can set `SPROOZI_PLATFORMS` to just `linux/amd64` or `linux/arm64` if all your
nodes use that architecture. The tools Dockerfile supports those two architectures.
The first build downloads dependencies and installs the client tools.

Copy the top-level `Digest` from each inspection into these values, replacing
`<controller-digest>` and `<tools-digest>` with the 64 hexadecimal characters:

```sh
export IMG="$SPROOZI_IMAGE_PREFIX/sproozi@sha256:<controller-digest>"
export SPROOZI_TOOLS_IMAGE="$SPROOZI_IMAGE_PREFIX/sproozi-tools@sha256:<tools-digest>"
```

The runtime requires a digest-pinned image. The `localhost:5001` image that
[Getting started](getting-started.md) builds is local to your computer and must
not be used on remote cluster nodes.

## 3. Create the namespaces and gateway certificate

```sh
kubectl create namespace sproozi-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k config/agent-rbac
mkdir -p .local/install/tls
```

`config/agent-rbac` creates `sproozi-agents` and the only Role that lets the
controller create sandbox Pods, ServiceAccounts, NetworkPolicies and contract
ConfigMaps. Runs cannot start without it.

The gateway terminates both the proxy connection and the inspected Kubernetes
connection. Its certificate therefore needs the gateway hostname and
`kubernetes.default.svc`. For this evaluation, create a private CA and a
30-day certificate. The block is safe to rerun, until you
[use a new CA](#use-a-new-ca): it creates the CA, and then the gateway key and
certificate, only when their files are missing. After a CA switch, don't rerun
it as written; that section says what to run instead. It runs in a
subshell, so its `umask 077` doesn't stay set in your shell.

```sh
(
umask 077
if [ ! -f .local/install/tls/ca.key ] || [ ! -f .local/install/tls/ca.crt ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 30 \
    -subj '/CN=Sproozi evaluation CA' \
    -addext 'basicConstraints=critical,CA:TRUE' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -addext 'subjectKeyIdentifier=hash' \
    -keyout .local/install/tls/ca.key -out .local/install/tls/ca.crt
  rm -f .local/install/tls/tls.crt
fi
if [ ! -f .local/install/tls/tls.key ] || [ ! -f .local/install/tls/tls.crt ]; then
  openssl req -newkey rsa:2048 -nodes \
    -subj '/CN=sproozi-gateway.sproozi-system.svc' \
    -keyout .local/install/tls/tls.key -out .local/install/tls/tls.csr
  cat > .local/install/tls/extensions.cnf <<'EOF'
subjectAltName=DNS:sproozi-gateway.sproozi-system.svc,DNS:sproozi-gateway.sproozi-system.svc.cluster.local,DNS:kubernetes.default.svc
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
EOF
  openssl x509 -req -days 30 -sha256 \
    -in .local/install/tls/tls.csr \
    -CA .local/install/tls/ca.crt -CAkey .local/install/tls/ca.key \
    -CAserial .local/install/tls/ca.srl -CAcreateserial \
    -extfile .local/install/tls/extensions.cnf -out .local/install/tls/tls.crt
fi
openssl verify -x509_strict -purpose sslserver \
  -CAfile .local/install/tls/ca.crt .local/install/tls/tls.crt &&
kubectl create configmap sproozi-sandbox-trust -n sproozi-agents \
  --from-file=ca-bundle.pem=.local/install/tls/ca.crt --dry-run=client -o json | \
  python3 -c 'import json,sys; obj=json.load(sys.stdin); obj["immutable"]=True; json.dump(obj,sys.stdout)' | \
  kubectl apply -f - &&
kubectl create secret tls sproozi-gateway-tls -n sproozi-system \
  --cert=.local/install/tls/tls.crt --key=.local/install/tls/tls.key \
  --dry-run=client -o yaml | kubectl apply -f -
)
```

The `verify` command prints `.local/install/tls/tls.crt: OK`. If it fails, for
example because the certificate has expired, the `&&` stops both applies.
Delete `tls.crt` and rerun the block to sign a new one; if the CA itself has
expired, move to a new CA as described below. The key
identifier extensions let strict verifiers, such as Python 3.13 and later,
accept the chain. Only the public CA goes into the sandbox trust ConfigMap. The
gateway Secret contains its certificate and private key. Keep the CA private
key on your computer, and keep the files in `.local/install/tls`: later steps
sign with the same CA.

Both `kubectl` commands create the object or apply it again, so a rerun with
the same files changes nothing. The trust ConfigMap is immutable, though. If
the CA files are lost and the block makes a new CA, the ConfigMap apply fails
with `field is immutable`, and the `&&` stops the gateway Secret from changing
too, so sandboxes keep trusting the certificate the gateway serves. To switch
to the new CA, follow [use a new CA](#use-a-new-ca).

The gateway reads its certificate only when it starts, so it keeps serving the
old one after the Secret changes, and new runs fail with `curl: (60) SSL
certificate problem: unable to get local issuer certificate`. Once the
gateway is installed (step 4), restart it after applying the Secret. Its
Deployment uses the `Recreate` strategy, so gateway calls fail while the new
Pod starts:

```sh
kubectl rollout restart -n sproozi-system deployment/sproozi-gateway &&
  kubectl rollout status -n sproozi-system deployment/sproozi-gateway --timeout=180s
```

### Use a new CA

Use this when step 3 made a new CA and its ConfigMap apply failed with
`field is immutable`. Runs started before the switch still trust only the old
CA and lose the gateway for good: they stay `Running` and every call fails
with `curl: (60)`. The block therefore refuses while any AgentRun is
`Admitted` or `Running`, and lists them. Wait for them to finish or cancel
them; to switch anyway and lose them, run it with
`SPROOZI_ALLOW_ACTIVE_RUNS=1` set.

The block creates the trust ConfigMap under a new name, points every
AgentRuntime that names the old ConfigMap at the new one, then applies the
gateway Secret and, if the gateway is installed, restarts it. When no
AgentRuntime names the old ConfigMap and the Secret already holds these files,
it changes nothing, prints `Nothing to switch: …` and doesn't restart the
gateway. Choose a name that isn't in use yet; for a later move, use
`sproozi-sandbox-trust-3` and set `OLD_TRUST` to the name in use:

```sh
(
umask 077
OLD_TRUST=sproozi-sandbox-trust
NEW_TRUST=sproozi-sandbox-trust-2
if kubectl get crd agentruns.sproozi.com >/dev/null 2>&1; then
  runs=$(kubectl get agentruns -A --no-headers -o \
    custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase) || exit 1
  active=$(printf '%s\n' "$runs" | awk '$3 == "Admitted" || $3 == "Running" { print "  " $1 "/" $2 " " $3 }')
  if [ -n "$active" ] && [ "${SPROOZI_ALLOW_ACTIVE_RUNS:-}" != 1 ]; then
    echo "Refusing to switch CAs: these AgentRuns would fail every gateway call:" >&2
    printf '%s\n' "$active" >&2
    echo "Wait for them to finish or cancel them, or rerun with SPROOZI_ALLOW_ACTIVE_RUNS=1 to switch anyway." >&2
    exit 1
  fi
fi
openssl verify -x509_strict -purpose sslserver \
  -CAfile .local/install/tls/ca.crt .local/install/tls/tls.crt || exit 1
kubectl create configmap "$NEW_TRUST" -n sproozi-agents \
  --from-file=ca-bundle.pem=.local/install/tls/ca.crt --dry-run=client -o json | \
  python3 -c 'import json,sys; obj=json.load(sys.stdin); obj["immutable"]=True; json.dump(obj,sys.stdout)' | \
  kubectl apply -f - || exit 1
: > .local/install/tls/runtimes.txt
if kubectl get crd agentruntimes.sproozi.com >/dev/null 2>&1; then
  runtimes=$(kubectl get agentruntimes -A --no-headers -o \
    custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,TRUST:.spec.clientConfig.trustBundleConfigMap.name) || exit 1
  printf '%s\n' "$runtimes" | awk -v old="$OLD_TRUST" '$3 == old { print $1, $2 }' > .local/install/tls/runtimes.txt
fi
if [ ! -s .local/install/tls/runtimes.txt ] &&
  kubectl get secret sproozi-gateway-tls -n sproozi-system -o json 2>/dev/null |
  python3 -c 'import base64,json,sys; d=json.load(sys.stdin)["data"]; sys.exit(any(base64.b64decode(d[k]) != open(".local/install/tls/" + k, "rb").read() for k in ("tls.crt", "tls.key")))'
then
  echo "Nothing to switch: no AgentRuntime names $OLD_TRUST and the gateway Secret already holds these files; the gateway was not restarted"
  exit 0
fi
while read -r namespace name; do
  echo "Pointing AgentRuntime $namespace/$name at $NEW_TRUST"
  kubectl patch agentruntime "$name" -n "$namespace" --type=merge \
    -p "{\"spec\":{\"clientConfig\":{\"trustBundleConfigMap\":{\"name\":\"$NEW_TRUST\"}}}}" || exit 1
done < .local/install/tls/runtimes.txt
kubectl create secret tls sproozi-gateway-tls -n sproozi-system \
  --cert=.local/install/tls/tls.crt --key=.local/install/tls/tls.key \
  --dry-run=client -o yaml | kubectl apply -f - || exit 1
if kubectl get deployment sproozi-gateway -n sproozi-system >/dev/null 2>&1; then
  kubectl rollout restart -n sproozi-system deployment/sproozi-gateway &&
    kubectl rollout status -n sproozi-system deployment/sproozi-gateway --timeout=180s
fi
)
```

The block prints one `Pointing AgentRuntime NAMESPACE/NAME at NAME` line for
each AgentRuntime it changes. If a restart failed after the Secret was
applied, a rerun prints `Nothing to switch`; restart the gateway with the
command at the end of step 3 instead.

After a switch, don't rerun the step 3 block as written: it applies the new
CA to `sproozi-sandbox-trust`, which fails with `field is immutable`. To sign
a new gateway certificate with the same CA, delete `tls.crt`, run the two
`if … fi` sections of step 3, then rerun this block with the same `OLD_TRUST` and
`NEW_TRUST`: it applies the new Secret and restarts the gateway.

Later steps apply example AgentRuntimes that name `sproozi-sandbox-trust`,
and step 7's certificate check reads that ConfigMap. After a switch, replace
`sproozi-sandbox-trust` with your new name in those commands and resources
before you apply them, or rerun this block afterwards to patch the new
AgentRuntimes.

For a persistent installation, arrange certificate renewal before expiry.

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
only `kubernetes.read` for this first run, and a build script that the later
steps reuse:

```sh
make kustomize
mkdir -p .local/install
if [ ! -e .local/install/kustomization.yaml ]; then
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
fi
cat > .local/install/build.sh <<'EOF'
#!/bin/sh
# Rebuild install.yaml from this overlay, keeping the last good file on failure.
set -eu
cd "$(dirname "$0")"
trap 'rm -f install.yaml.new' EXIT
kept='No install.yaml was written.'
[ -e install.yaml ] && kept='Kept the previous install.yaml.'
../../bin/kustomize build . > install.yaml.new
deployments=$(awk '/^---$/ { kind = "" } /^kind: / { kind = $2 }
  kind == "Deployment" && /^  name: / { print $2 }' install.yaml.new)
for name in sproozi-controller-manager sproozi-gateway sproozi-webhook; do
  if ! printf '%s\n' "$deployments" | grep -qx "$name"; then
    echo "The build has no $name Deployment; check that kustomization.yaml lists ../../config/install under resources. $kept" >&2
    exit 1
  fi
done
if grep -Eq 'image: example\.com/sproozi($|[:@])' install.yaml.new; then
  echo "The build still uses the placeholder image example.com/sproozi: set IMG and rerun step 4. $kept" >&2
  exit 1
fi
mv install.yaml.new install.yaml
EOF
if [ -z "${IMG:-}" ]; then
  echo 'IMG is not set, so nothing was built. Set IMG to your pushed image digest from step 2, then rerun this block.' >&2
elif case "$IMG" in *[[:space:]]*) true ;; *) false ;; esac; then
  echo "IMG \"$IMG\" contains a space, tab or newline, so nothing was built. Set IMG to the image digest without whitespace, then rerun this block." >&2
else
  (cd .local/install && ../../bin/kustomize edit set image example.com/sproozi="$IMG") &&
    sh .local/install/build.sh &&
    grep -nE 'image: [^ ]' .local/install/install.yaml
fi
```

The block is safe to rerun. It writes `kustomization.yaml` only if the file is
missing, so a rerun keeps patches that later sections add; to start the overlay
again, delete the file first. `build.sh` builds into `install.yaml.new` and
replaces `install.yaml` only when the build succeeds, contains the
`sproozi-controller-manager`, `sproozi-gateway` and `sproozi-webhook`
Deployments, and no longer uses the placeholder image. Otherwise it removes
`install.yaml.new`, prints the reason and keeps the last good `install.yaml`,
which [Cleanup](#cleanup) needs. The reason ends `Kept the previous
install.yaml.`, or `No install.yaml was written.` when no build has succeeded
yet. A `kustomization.yaml` whose
`../../config/install` line was removed fails with `The build has no
sproozi-controller-manager Deployment`. So does an empty one in this block,
because `kustomize edit set image` first fills it with only an `images` entry.
When a later section runs `build.sh` on an empty file directly, kustomize
prints `Error: kustomization.yaml is empty` instead. Either way any previous
`install.yaml` is kept; delete `kustomization.yaml` and rerun this block.
Without `IMG`, the block prints `IMG is not set, so nothing was built.` and
leaves any earlier files unchanged. An `IMG` containing whitespace, such as a
trailing space copied with the digest, is refused the same way with `IMG "..."
contains a space, tab or newline, so nothing was built.`

`config/manager` already renames the source image `controller` to the
placeholder `example.com/sproozi`, so the overlay replaces that placeholder.
The `grep` should show your `$IMG` digest three times, for the controller,
gateway and webhook Deployments, and no `example.com/sproozi`.

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
  --group apps --version v1 --kind Deployment --path private-registry-patch.yaml) &&
  sh .local/install/build.sh
```

On a rerun, `kustomize edit add patch` prints a line such as `patch
types.Patch{Path:"private-registry-patch.yaml", ...} already in kustomization
file`. It is harmless: the patch is already listed, the command exits 0 and
the build continues.

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
```

The gateway becomes ready only after it has validated its configuration and
opened its TLS listener, so `kubectl rollout status` waits for a working
gateway. If it times out, read the reason with
`kubectl logs -n sproozi-system deployment/sproozi-gateway --previous`. If that
prints `previous terminated container ... not found`, the gateway container
never started; its events say why:

```sh
kubectl describe pod -n sproozi-system \
  -l app.kubernetes.io/name=sproozi,app.kubernetes.io/component=shared-gateway
```

`secret "sproozi-gateway-tls" not found` in the events means step 3 hasn't
run. Run step 3 first; the Pod starts once the Secret exists.

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

## 7. Run a model on this cluster

This step runs the investigation-to-PR demo from
[Getting started](getting-started.md) on your installation. A stock Codex
agent investigates a deliberately broken workload in a new `sproozi-demo`
namespace and opens a pull request in a disposable GitHub repository. It uses
the [SRE demo resources](../../examples/sre-demo/kustomization.yaml), rendered
with your tools image, which already contains Codex.

You need:

- Access to the demo's `gpt-6.1-sol` model, either through an OpenAI API key
  or through a ChatGPT plan. The ChatGPT sign-in needs a browser on the
  computer where you run these commands.
- A disposable GitHub repository with `demo-workload.yaml` on `main`, and a
  GitHub App installed only on it. Follow steps 4 and 5 of
  [Getting started](getting-started.md#4-create-the-disposable-github-repository).
- The GitHub CLI, signed in, from the optional prerequisite in step 1.

Delete the first run, so only the demo run is listed while you watch:

```sh
kubectl delete agentrun getting-started -n sproozi-system --ignore-not-found
```

### Reissue the gateway certificate

The gateway refuses to start unless its certificate covers every service it
inspects. Model inference adds `api.openai.com`, and pull requests add
`github.com` and `api.github.com`. Sign a new certificate with the CA and
gateway key from step 3. Sandboxes trust only the CA in the immutable
`sproozi-sandbox-trust` ConfigMap, so the block first checks that this
ConfigMap holds the same CA as `.local/install/tls/ca.crt`. If it doesn't,
for example because the CA files were lost and step 3 made a new CA, the
block prints a message and changes nothing; follow
[use a new CA](#use-a-new-ca) in step 3 and use your new ConfigMap name in the
check. The certificate is applied only when the check
and `openssl verify` both pass. The running gateway reads its certificate only
at startup and keeps serving the old one until the restart in
[Enable the model and GitHub modules](#enable-the-model-and-github-modules).
If you rerun step 3 afterwards, it keeps this certificate, because the files
exist:

```sh
(
umask 077
if ! kubectl get configmap sproozi-sandbox-trust -n sproozi-agents \
    -o jsonpath='{.data.ca-bundle\.pem}' | cmp -s - .local/install/tls/ca.crt; then
  echo 'ConfigMap sproozi-sandbox-trust in sproozi-agents does not hold .local/install/tls/ca.crt, so sandboxes would not trust a certificate signed by it. Nothing was changed. If step 3 made a new CA, follow "Use a new CA" in step 3.' >&2
  exit 1
fi
cat > .local/install/tls/extensions.cnf <<'EOF'
subjectAltName=DNS:sproozi-gateway.sproozi-system.svc,DNS:sproozi-gateway.sproozi-system.svc.cluster.local,DNS:kubernetes.default.svc,DNS:api.openai.com,DNS:github.com,DNS:api.github.com
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
EOF
openssl x509 -req -days 30 -sha256 \
  -in .local/install/tls/tls.csr \
  -CA .local/install/tls/ca.crt -CAkey .local/install/tls/ca.key \
  -CAserial .local/install/tls/ca.srl -CAcreateserial \
  -extfile .local/install/tls/extensions.cnf -out .local/install/tls/tls.crt &&
openssl verify -x509_strict -purpose sslserver \
  -CAfile .local/install/tls/ca.crt .local/install/tls/tls.crt &&
kubectl create secret tls sproozi-gateway-tls -n sproozi-system \
  --cert=.local/install/tls/tls.crt --key=.local/install/tls/tls.key \
  --dry-run=client -o yaml | kubectl apply -f -
)
```

### Add the provider credentials

These objects stay in `sproozi-system` and only the gateway reads them; see
[required secrets and config](../reference/required-secrets-and-config.md).
Add the GitHub App credential, replacing the App ID, installation ID and
private-key path with your own:

```sh
kubectl create secret generic capability-proxy-github-app -n sproozi-system \
  --from-literal=app-id=YOUR_APP_ID \
  --from-literal=installation-id=YOUR_INSTALLATION_ID \
  --from-file=private-key.pem=/absolute/path/to/github-app.pem \
  --dry-run=client -o yaml | kubectl apply -f -
```

Then add the model credential for one of the two modes. The
`model-gateway-auth` ConfigMap selects the mode, and the gateway doesn't fall
back from one to the other.

**With an OpenAI API key.** Type the key at the hidden prompt rather than
pasting it into a command:

```sh
printf 'OpenAI API key: '; stty -echo; IFS= read -r SPROOZI_OPENAI_KEY; stty echo; echo
(umask 077 && printf '%s' "$SPROOZI_OPENAI_KEY" > .local/install/openai-api-key)
unset SPROOZI_OPENAI_KEY
kubectl create secret generic model-gateway-credentials -n sproozi-system \
  --from-file=openai-api-key=.local/install/openai-api-key \
  --dry-run=client -o yaml | kubectl apply -f -
rm .local/install/openai-api-key
kubectl create configmap model-gateway-auth -n sproozi-system \
  --from-literal=mode=api_key --dry-run=client -o yaml | kubectl apply -f -
export SPROOZI_MODEL_AUTH=api_key
```

**With a ChatGPT plan.** Sign in on this computer, then load the saved ChatGPT
credential into the `model-gateway-chatgpt` Secret. The gateway that holds a
ChatGPT credential refreshes it and rotates its tokens, so only one gateway may
use each credential.
If `.local/chatgpt-session.json` already exists, for example because you ran
[Getting started](getting-started.md), `make demo-chatgpt-login` reuses it with
a warning, and the Kind demo's gateway may already own it. In that case,
replace the first command with `make demo-chatgpt-login CHATGPT_REAUTHORIZE=1`
(or `bash hack/demo/chatgpt-login.sh --reauthorize`), which always opens a
fresh browser sign-in:

```sh
make demo-chatgpt-login
kubectl create secret generic model-gateway-chatgpt -n sproozi-system \
  --from-file=session.json=.local/chatgpt-session.json \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create configmap model-gateway-auth -n sproozi-system \
  --from-literal=mode=chatgpt --dry-run=client -o yaml | kubectl apply -f -
export SPROOZI_MODEL_AUTH=chatgpt
```

Without a saved ChatGPT credential, `make demo-chatgpt-login` opens the browser sign-in
and saves `.local/chatgpt-session.json`. Either way it checks that the account
can use `gpt-6.1-sol`; if that check fails, the command fails and you shouldn't
load the credential. It also records the mode in `.local/demo.env`, which only
`make demo` reads. Each time the gateway refreshes the ChatGPT credential, it stores the
rotated tokens in the Secret, so the local file goes stale. Don't load the same
ChatGPT credential into another cluster or scale the gateway; sign in again with
`--reauthorize` first.
[Model provider authentication](model-provider-auth.md) explains the sign-in
checks, the rotating ChatGPT credential and the ChatGPT request limits.

### Enable the model and GitHub modules

In `.local/install/kustomization.yaml`, change the gateway's
`SPROOZI_ENABLED_CAPABILITIES` value from `kubernetes.read` to
`kubernetes.read,model.inference,github.pull_request`. Then rebuild, apply and
restart the gateway so it reads the new certificate and credentials. The
gateway Deployment uses the `Recreate` strategy, so the old gateway stops
before the new Pod is ready. Gateway calls fail for that interval, and a run in
progress may fail; make these changes while no run is active:

```sh
sh .local/install/build.sh &&
  kubectl apply --server-side --field-manager=sproozi-installer -f .local/install/install.yaml &&
  kubectl rollout restart -n sproozi-system deployment/sproozi-gateway &&
  kubectl rollout status -n sproozi-system deployment/sproozi-gateway --timeout=180s
```

If the rollout times out, read the gateway's logs as in step 4 before you
continue.

The installed `sproozi-model-gateway-pricing` ConfigMap already prices `gpt-6.1-sol`
for API-key mode's cost cap.

### Apply the demo resources and submit the run

Render the demo with your tools image, repository and model mode, review it,
and apply it. It creates the `sproozi-demo` namespace and its broken workload,
the `codex` runtime, the `sre` policy and the `sre-remediation` template:

```sh
# Replace YOUR-ACCOUNT with the owner of your disposable repository;
# render.py refuses placeholder owners such as YOUR-ACCOUNT, OWNER and example.
export SPROOZI_DEMO_REPOSITORY=YOUR-ACCOUNT/sproozi-demo
python3 hack/demo/render.py --codex-image "$SPROOZI_TOOLS_IMAGE" \
  --repository "$SPROOZI_DEMO_REPOSITORY" \
  --model-auth "$SPROOZI_MODEL_AUTH" > .local/install/sre-demo.yaml
kubectl apply -f .local/install/sre-demo.yaml
```

`--model-auth` sets the `sre` policy's model budget. With `api_key` it keeps
100000 tokens and a 500000-microdollar cost cap. With `chatgpt` it removes the
cost cap, which ChatGPT mode rejects, and raises the allowance to 300000 tokens,
because stock Codex resends its context on each turn and exhausts 100000 before
opening a PR.

If you created the private registry Secret, patch the `codex` runtime as in
step 5, replacing `getting-started` with `codex`. Then submit the run and watch
it:

```sh
kubectl create -f examples/sre-demo/manual-agentrun.yaml
kubectl get agentruns -n sproozi-system -w
```

The `sproozi-demo` workload's failing Pod is expected. When the run reaches
`Succeeded`, check for the PR:

```sh
gh pr list --repo "$SPROOZI_DEMO_REPOSITORY" --state open
kubectl get agentrun manual-sre-remediation -n sproozi-system -o yaml
```

Run success means the `agent` container exited with code zero; it doesn't
prove that the PR exists or that the fix is right. Review the PR's diff, which
should change only `demo-workload.yaml`. This manual run doesn't produce the
acceptance report that `make demo` writes. Step 8 of
[Getting started](getting-started.md#8-inspect-the-result-and-explore)
shows how to inspect, repeat or cancel a run.

The demo's policy grants only reads in `sproozi-demo`, pull requests to your
repository's `main` branch and a model budget. To run your own tasks, write a
runtime, policy and template for them; see
[AgentRuntime](../reference/agentruntime.md), [AgentPolicy](../reference/agentpolicy.md)
and [AgentTemplate](../reference/agenttemplate.md).

## Limit concurrent runs

This step is optional. There is no cluster cap by default, and the controller
admits every run that its policy allows, so any number of runs can be active at
once. Before you share
the cluster or connect a webhook, read the
[concurrency warnings](../reference/security-hardening.md#concurrency-and-gateway-capacity)
and set the caps you need.

To cap active runs across the cluster, add the controller flag
`--max-active-runs` with a patch file, then rebuild and apply:

```sh
if [ ! -e .local/install/max-active-runs-patch.yaml ]; then
cat > .local/install/max-active-runs-patch.yaml <<'EOF'
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: --max-active-runs=4
EOF
fi
(cd .local/install && ../../bin/kustomize edit add patch --group apps --version v1 \
  --kind Deployment --name sproozi-controller-manager --path max-active-runs-patch.yaml) &&
  sh .local/install/build.sh &&
  kubectl apply --server-side --field-manager=sproozi-installer -f .local/install/install.yaml
```

The patch appends to the `manager` container's arguments. `kustomize edit add
patch` adds it to the `patches:` list in `.local/install/kustomization.yaml`,
so you don't have to edit that file by hand; on a rerun it prints the
harmless `patch types.Patch{...} already in kustomization file` line described
in [step 4](#4-install-sproozi-with-kubernetes-reads-enabled) and continues. The block writes the patch file only if it
is missing, so rerunning it keeps your value. To change the cap, edit `value`
in `.local/install/max-active-runs-patch.yaml`, then rerun the block. `0`, or
no flag, means no cluster cap.

To cap the runs admitted under one policy, set `maxActiveRuns` on your policy,
for example `sre` if you followed step 7. Templates that share the policy share
the cap:

```sh
kubectl patch agentpolicy sre -n sproozi-system --type=merge \
  -p '{"spec":{"maxActiveRuns":2}}'
```

A run held by either cap stays `Queued` with `Ready` reason
`WaitingForConcurrencyCap`, and its message names the cap, for example
`AgentRun is held by a concurrency cap: cluster cap 4 reached` or
`AgentRun is held by a concurrency cap: policy cap 2 (AgentPolicy sre) reached`. Raising or
removing a cap admits waiting runs straight away. Editing `maxActiveRuns`
doesn't close running runs' open connections at the gateway; every other
policy `spec` change still does. See
[AgentPolicy](../reference/agentpolicy.md#concurrent-runs).

The gateway carries the traffic of every run. Its install requests 25m CPU and
128Mi memory and limits it to 2 CPU and 2Gi. Its gateway connection limits
allow at most 2048 connections, shared by all runs, and 16 inspected
connections per execution.
The scheduler reserves only the requests. To reserve capacity, or to shrink the
gateway, patch its resources and connection limit together. Allow one
connection per MiB of `limits.memory`. Each connection is budgeted 256 KiB,
against about 150–160 KiB measured across runs, so the connection budgets
take a quarter of the limit. The other three quarters are headroom for model
bodies, the gateway's own heap and the garbage collector; the gateway sets its
Go memory limit to 90% of `limits.memory`. Each model request in flight holds
up to about 2 MiB of request and response body data outside the connection
budget, plus allocation overhead at peak, so lower the connection limit further
if most connections carry model calls at once.

This example halves the gateway. The 1Gi limit allows 1024 connections, whose
budgets take 256 MiB. The Go memory limit is 90% of 1Gi, about 922 MiB, so
about 666 MiB is left under it for model bodies, the gateway's own heap and
the garbage collector. The requests reserve
half a CPU and half the memory limit on the node:

```sh
if [ ! -e .local/install/gateway-capacity-patch.yaml ]; then
cat > .local/install/gateway-capacity-patch.yaml <<'EOF'
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
        - name: SPROOZI_GATEWAY_MAX_CONNECTIONS
          value: "1024"
        resources:
          requests:
            cpu: 500m
            memory: 512Mi
          limits:
            cpu: "1"
            memory: 1Gi
EOF
fi
(cd .local/install && ../../bin/kustomize edit add patch --group apps --version v1 \
  --kind Deployment --name sproozi-gateway --path gateway-capacity-patch.yaml)
```

The block writes the patch file only if it is missing; to change the values
later, edit `.local/install/gateway-capacity-patch.yaml`.

The patch merges into the gateway container by name, alongside the
`SPROOZI_ENABLED_CAPABILITIES` patch from step 4. Rebuild, apply and restart
the gateway as in
[Enable the model and GitHub modules](#enable-the-model-and-github-modules).
The install doesn't set `SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION`, so the
limit on inspected connections per execution is 16, or the connection limit if
that is lower. Add the variable to the same `env` list only to change it; see
[Required secrets and config](../reference/required-secrets-and-config.md).

> **Warning:** The gateway exits at startup on an invalid value, such as
> `SPROOZI_GATEWAY_MAX_CONNECTIONS` set to `-5`. Its Deployment uses the
> `Recreate` strategy
> ([ADR 0026](../adr/0026-single-gateway-refresh-owner-with-recreate.md)), so
> the old gateway is already gone and every run loses its gateway until you
> correct the value.

## Troubleshooting

| Symptom | Next step |
| --- | --- |
| Deployment has `ImagePullBackOff` | Describe its Pod. If its image is `example.com/sproozi`, the overlay has no image override: set `IMG` and rerun the overlay block from step 4, then apply. Otherwise check registry visibility, image digest, node architecture and image pull credentials. |
| Gateway has `CrashLoopBackOff`, or its `rollout status` times out | The gateway exits on bad configuration and isn't ready until its configuration is valid. Read `kubectl logs -n sproozi-system deployment/sproozi-gateway --previous`; the message names the Secret, key and fix. `GitHub App private key is missing` or `... is missing: create key ...`: add that key to `capability-proxy-github-app`, or remove `github.pull_request` from the overlay's `SPROOZI_ENABLED_CAPABILITIES`. `does not cover configured host`: reissue the certificate with that host in its subjectAltName. A ChatGPT credential error, such as `ChatGPT credential is incomplete; sign in again (key session.json in ...): run ... and load the new credential into that Secret`: run `make demo-chatgpt-login CHATGPT_REAUTHORIZE=1` and load the new ChatGPT credential into `model-gateway-chatgpt` as in [Add the provider credentials](#add-the-provider-credentials). `must be a single line`: recreate the provider key Secret from a file that holds only the key, on one line. The GitHub App key may be PKCS#1 or PKCS#8 RSA PEM. |
| Run stays `Queued` | Read its `Ready` condition. Reason `WaitingForConcurrencyCap` means a concurrency cap is full: the message names the policy cap, as `policy cap N (AgentPolicy NAME)`, the cluster cap from `--max-active-runs`, or both. It is admitted when an active run finishes; cancel other runs or raise the cap to admit it sooner. Without that reason, check the controller logs. See [Limit concurrent runs](#limit-concurrent-runs). |
| Run is `Running` with `Ready` reason `QuotaExceeded` | A ResourceQuota in `sproozi-agents` rejected the sandbox Pod. The controller retries until the run's deadline. Free or raise the quota, or cancel the run. |
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

## Cleanup

Remove the tutorial resources when finished:

```sh
kubectl delete agentrun getting-started -n sproozi-system --ignore-not-found
kubectl delete -f .local/install/first-run-resources.yaml
```

If you ran step 7, also remove its run and resources. Don't delete the whole
rendered file: it also declares the shared `sproozi-system` and
`sproozi-agents` namespaces.

```sh
kubectl delete agentrun manual-sre-remediation -n sproozi-system --ignore-not-found
kubectl delete agenttemplate sre-remediation -n sproozi-system
kubectl delete agentpolicy sre -n sproozi-system
kubectl delete agentruntime codex -n sproozi-system
kubectl delete namespace sproozi-demo
```

Close the demo PR and remove the repository and App installation through GitHub
when you no longer need them.

If this installation was created solely for the tutorial and has no other runs,
you can also uninstall it. The following removes Sproozi's CRDs and all their
custom resources, as well as its namespaces and trusted Secrets:

```sh
kubectl delete -f .local/install/install.yaml
kubectl delete -k config/agent-rbac
```

Keep the certificate files and rendered manifests if you intend to reinstall
with the same configuration.
