# Native session mobility fixture

Run these commands from the repository root in one administrator shell. They
exercise the production home daemon, Docker placement, authenticated remote
profile, Kubernetes placement and stock native terminal. The provider is a
deterministic local HTTPS fixture; it needs no personal credentials or paid model
account. Codex's extended production roundtrip and Claude's complete TUI/Bash/Git
roundtrip with both-placement denial probes passed on 12 October 2026. See
[recorded acceptance](../../../test/acceptance/reports/verified-session-mobility.md) for exact
coverage; rerun these commands against the images you intend to evaluate.

This setup is for Docker Desktop with the host reachable from containers at
`192.168.65.254`. Both renderers select that upstream address explicitly. Other
Docker installations need a separately verified address and renderer change.
Docker access and cluster administration are required. Use a fresh dedicated
cluster, private fixture directories and unused ports; do not use an existing
development or production cluster.

## Prepare the host and enforcing cluster

Install Docker, Kind, kubectl, Go, Git, OpenSSL, Python 3 and Python's PyYAML
package. See the [investigation-to-PR demo guide](../../../docs/guides/pr-demo.md)
for host prerequisites and the disposable Kind/CNI setup. The commands below
select the node image pinned in this checkout's Makefile. Keep the generated
keys, tokens, native histories, command logs and evidence private. Do not enable
shell tracing or print the generated configuration or credential files.

```sh
set -e
umask 077
work=$(mktemp -d /private/tmp/sproozi-mobility.XXXXXX)
cluster="sproozi-mobility-$(date +%Y%m%d)-$(python3 -c 'import secrets; print(secrets.token_hex(3))')"
mkdir -p "$work/bin" "$work/images"
chmod 700 "$work" "$work/bin" "$work/images"
python3 -c 'import yaml'
make kustomize
go build -o "$work/bin/sproozi" ./cmd/sproozi
go build -o "$work/bin/native-pty" ./hack/verify/native-pty
go build -o "$work/bin/checkpoint" ./cmd/checkpoint

cat >"$work/kind.yaml" <<'YAML'
apiVersion: kind.x-k8s.io/v1alpha4
kind: Cluster
nodes:
- role: control-plane
- role: worker
networking:
  disableDefaultCNI: true
  podSubnet: 10.244.0.0/16
YAML
kind create cluster --name "$cluster" --config "$work/kind.yaml" \
  --image kindest/node:v1.33.1@sha256:050072256b9a903bd914c0b2866828150cb229cea0efe5892e2b644d5dd3b34f \
  --kubeconfig "$work/kubeconfig"

bash hack/verify/fetch-calico.sh "$work/calico-original.yaml"
python3 - "$work/calico-original.yaml" "$work/calico.yaml" <<'PY'
import sys
from pathlib import Path
import yaml
documents = list(yaml.safe_load_all(Path(sys.argv[1]).read_text()))
found = 0
for document in documents:
    if document and document.get("kind") == "DaemonSet" and document["metadata"]["name"] == "calico-node":
        for container in document["spec"]["template"]["spec"]["containers"]:
            if container["name"] == "calico-node":
                env = container.setdefault("env", [])
                env[:] = [item for item in env if item["name"] != "CALICO_IPV4POOL_CIDR"]
                env.append({"name": "CALICO_IPV4POOL_CIDR", "value": "10.244.0.0/16"})
                found += 1
assert found == 1, "Expected exactly one Calico node container"
Path(sys.argv[2]).write_text(yaml.safe_dump_all(documents, sort_keys=False))
PY
kubectl --kubeconfig "$work/kubeconfig" apply --server-side -f "$work/calico.yaml"
kubectl --kubeconfig "$work/kubeconfig" -n kube-system rollout status daemonset/calico-node --timeout=180s
kubectl --kubeconfig "$work/kubeconfig" -n kube-system rollout status deployment/calico-kube-controllers --timeout=180s
kubectl --kubeconfig "$work/kubeconfig" wait node --all --for=condition=Ready --timeout=180s
kubectl --kubeconfig "$work/kubeconfig" get ippools.crd.projectcalico.org \
  -o custom-columns=NAME:.metadata.name,CIDR:.spec.cidr
```

Confirm the pool is `10.244.0.0/16`. Calico's default `192.168.0.0/16` overlaps
the Docker Desktop host route; leaving it unchanged can prevent gateway-to-host
provider traffic. Readiness establishes installation; the extended verifier's
actual forbidden network probes establish enforcement for this fixture.

## Build and resolve the images

The client Dockerfiles install the Codex and Claude Code releases pinned in
[`hack/clients`](../../clients/README.md), with install scripts disabled. The
first build pulls its public base. Both client builds use
`--no-cache` to run the installers again; the second uses the local image from
the first build. The native image adds the supervisor, checkpoint helper and
network command.
Build logs retain the parent resolution. `images.py` exports and imports the
final images into every Kind node, verifies CRI resolution, and records canonical
`repository@sha256` references in `images.json`.

```sh
docker build --pull --no-cache -f hack/demo/codex.Dockerfile \
  -t sproozi-session/clients:codex . >"$work/codex-build.log" 2>&1
docker image inspect sproozi-session/clients:codex >"$work/codex-image.json"
docker build --no-cache -f internal/runtime/docker/image/clients.Dockerfile \
  --build-arg CODEX_CLIENTS_IMAGE=sproozi-session/clients:codex \
  -t sproozi-session/clients:mobility . >"$work/clients-build.log" 2>&1
docker image inspect sproozi-session/clients:mobility >"$work/clients-image.json"
docker build -f internal/runtime/docker/image/Dockerfile \
  --build-arg CLIENTS_IMAGE=sproozi-session/clients:mobility \
  -t docker.io/sproozi-session/native:fixture . >"$work/native-build.log" 2>&1
docker build -t docker.io/sproozi-session/control:fixture . >"$work/control-build.log" 2>&1
python3 hack/verify/session-mobility/images.py \
  --cluster "$cluster" --output-directory "$work/images"

native_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["native"]["image"])' "$work/images/images.json")
control_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["control"]["image"])' "$work/images/images.json")
native_id=$(docker image inspect docker.io/sproozi-session/native:fixture --format '{{.Id}}')
docker image inspect docker.io/sproozi-session/native:fixture >"$work/native-image.json"
docker image inspect docker.io/sproozi-session/control:fixture >"$work/control-image.json"
docker_binary=$(command -v docker)
docker_endpoint=$(docker context inspect --format '{{(index .Endpoints "docker").Host}}')
case "$docker_endpoint" in
  unix://*) docker_socket=${docker_endpoint#unix://} ;;
  *) echo 'Use the local Docker Desktop Unix socket for this fixture' >&2; exit 1 ;;
esac
```

Resolve `native_id` **after** `images.py`. Export/load can change the local Docker
image identity; a BuildKit log's config digest is not the authoritative Docker
image ID. Local placement takes the actual Docker `.Id`; Kubernetes takes the
canonical reference verified on all Kind nodes. The native image supplies all
three local roles: agent, checkpoint helper and network anchor.

## Configure Codex and start the owned services

`host.py prepare` creates a fresh CA and short-lived leaf certificate covering
OpenAI, gateway and executor DNS names, loopback and the Docker Desktop host
IP. The renderer writes public manifests, including the remote executor from
the [retained-work example](../../../examples/portable-workflows/kubernetes/owner.yaml)
and the CLI's ServiceAccount, and the executor's configuration. Create the two
TLS Secrets separately; the fixture leaf serves both the gateway and the
executor.

```sh
codex="$work/codex"
mkdir -m 700 "$codex"
python3 hack/verify/session-mobility/host.py prepare \
  --output-directory "$codex" --docker-binary "$docker_binary" --docker-socket "$docker_socket" \
  --native-image-id "$native_id" --helper-image-id "$native_id" --anchor-image-id "$native_id" \
  --network-name kind --gateway-ip 192.168.65.254 --provider-tls --duration 20m
python3 hack/verify/session-mobility/kubernetes.py \
  --control-image "$control_ref" --native-image "$native_ref" \
  --hints "$codex/profile-hints.json" --ca-file "$codex/ca.pem" \
  --gateway-cert "$codex/gateway.crt" --gateway-key "$codex/gateway.key" \
  --profile-config-output "$codex/profile-config.json" >"$codex/kubernetes.yaml"
export KUBECONFIG="$work/kubeconfig"
kubectl apply --server-side -f config/crd/bases
kubectl create namespace sproozi-system
kubectl -n sproozi-system create secret tls sproozi-gateway-tls \
  --cert="$codex/gateway.crt" --key="$codex/gateway.key"
kubectl -n sproozi-system create secret generic sproozi-work-credentials \
  --from-file=tls.crt="$codex/gateway.crt" --from-file=tls.key="$codex/gateway.key"
kubectl apply --server-side -f "$codex/kubernetes.yaml"
for name in sproozi-controller-manager sproozi-gateway sproozi-work; do
  kubectl -n sproozi-system rollout status "deployment/$name" --timeout=180s
done
```

Write a `sproozi` context for the `sproozi-cli` ServiceAccount into its own
file, as [remote executor access](../../../docs/reference/remote-executors.md#create-the-sproozi-context)
shows with `--kubeconfig "$work/sproozi.kubeconfig"`, then start the provider
and this machine's service:

```sh
unset KUBECONFIG
python3 hack/verify/session-mobility/host.py provider \
  --output-directory "$codex" --tls \
  --certificate-file "$codex/gateway.crt" --key-file "$codex/gateway.key" \
  >"$codex/provider.log" 2>&1 &
codex_provider_pid=$!
"$work/bin/sproozi" service run --config "$codex/home-config.json" >"$codex/home.log" 2>&1 &
codex_home_pid=$!
```

Server-side CRD apply avoids the oversized last-applied annotation on the runtime
schema. Only the executor holds Kubernetes credentials for the placement; this
machine's service reaches it through the `sproozi-cli` context, which can only
find the executor, port-forward to it and act as its principal. The controller
and executor remain separate owners of admission and runtime observation.

Wait for the private control socket and the provider before running the driver.
The provider readiness response contains fixture facts only. If a process or
the executor Pod exits, inspect its private log; do not proceed with a missing
owner. Pass the context file with `--context`, or export
`KUBECONFIG="$work/sproozi.kubeconfig"` to use the default `sproozi` context.

```sh
python3 - "$codex" 18765 30 <<'PY'
import ssl, stat, sys, time, urllib.request
from pathlib import Path
directory, provider_port, timeout = sys.argv[1:]
context = ssl.create_default_context(cafile=str(Path(directory) / "ca.pem"))
deadline = time.monotonic() + float(timeout)
while time.monotonic() < deadline:
    try:
        assert stat.S_ISSOCK((Path(directory) / "home/control.sock").stat().st_mode)
        with urllib.request.urlopen("https://127.0.0.1:" + provider_port + "/status", context=context, timeout=2) as response:
            assert response.status == 200
        break
    except (OSError, AssertionError):
        time.sleep(0.1)
else:
    raise RuntimeError("Fixture readiness timed out; inspect its private service logs")
PY
python3 hack/verify/session-mobility/extended.py \
  --output-directory "$codex/proof" --home-config "$codex/home-config.json" \
  --session-binary "$work/bin/sproozi" --pty-helper "$work/bin/native-pty" \
  --checkpoint-binary "$work/bin/checkpoint" --provider-directory "$codex" \
  --context "$work/sproozi.kubeconfig" \
  --kubeconfig "$work/kubeconfig" --kubectl "$(command -v kubectl)" \
  --namespace sproozi-system --agent-namespace sproozi-agents \
  --home-log "$codex/home.log"
```

`verify.py` runs the shorter Docker, cluster and Docker round trip with the
same arguments, without `--provider-directory`, `--kubeconfig`, `--kubectl` and
the namespaces.

Read `$codex/proof/report.json` privately. Success covers genuine native terminal
turns, same native UUID and original deadline across placements, workspace and
selected ignored-file continuity, real tool execution, forbidden network and
mount probes, a held graceful move, forced held-turn cancellation and accounting
replay. The driver also writes the user's [profile](../../../docs/reference/profile.md)
beside the home configuration, changes its marker before each move, and reads
the rendered `AGENTS.md` and profile skill in the exact Docker container and
Kubernetes Pod: each visit must carry the profile as it was when it began, and
the Kubernetes registration must name the stored archive. Controlled tools, histories and provider usage are the fixture's scope;
arbitrary tools, background execution and compaction are not established by it.
Retain failed reports and logs rather than reusing their provider state for a
fresh run. The driver ends a successful run with the session cancelled.

## Run the Claude production follow-up

Finish or cancel every Codex visit before switching the gateway certificate,
provider and executor. Stop the two owned processes using the PIDs captured
above. Keep the same dedicated cluster, controller, namespaces, `sproozi`
context and resolved images. Claude uses a fresh private directory, trust
material and executor principal, with ports 18775 (provider) and 18453 (home
gateway). Its renderer updates the shared gateway, adds Claude-specific
runtime, template and trust resources, and reconfigures the one executor in the
namespace for Claude, which restarts its Pod. It does not deploy a second
controller.

```sh
kill -TERM "$codex_home_pid" "$codex_provider_pid"
wait "$codex_home_pid" "$codex_provider_pid" || true
claude="$work/claude"
mkdir -m 700 "$claude"
python3 hack/verify/session-mobility/claude.py prepare \
  --output-directory "$claude" --docker-binary "$docker_binary" --docker-socket "$docker_socket" \
  --native-image-id "$native_id" --helper-image-id "$native_id" --anchor-image-id "$native_id" \
  --network-name kind --gateway-ip 192.168.65.254
python3 hack/verify/session-mobility/claude.py render \
  --control-image "$control_ref" --native-image "$native_ref" \
  --hints "$claude/profile-hints.json" --ca-file "$claude/ca.pem" \
  --gateway-cert "$claude/gateway.crt" --gateway-key "$claude/gateway.key" \
  --profile-config-output "$claude/profile-config.json" >"$claude/kubernetes.yaml"
export KUBECONFIG="$work/kubeconfig"
kubectl -n sproozi-system create secret tls sproozi-gateway-tls \
  --cert="$claude/gateway.crt" --key="$claude/gateway.key" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n sproozi-system create secret generic sproozi-work-credentials \
  --from-file=tls.crt="$claude/gateway.crt" --from-file=tls.key="$claude/gateway.key" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply --server-side -f "$claude/kubernetes.yaml"
kubectl -n sproozi-system rollout status deployment/sproozi-gateway --timeout=180s
kubectl -n sproozi-system rollout restart deployment/sproozi-work
kubectl -n sproozi-system rollout status deployment/sproozi-work --timeout=180s
unset KUBECONFIG
python3 hack/verify/session-mobility/claude.py serve \
  --output-directory "$claude" >"$claude/provider.log" 2>&1 &
claude_provider_pid=$!
"$work/bin/sproozi" service run --config "$claude/home-config.json" >"$claude/home.log" 2>&1 &
claude_home_pid=$!
python3 hack/verify/session-mobility/claude.py verify \
  --output-directory "$claude/proof" --home-config "$claude/home-config.json" \
  --session-binary "$work/bin/sproozi" --pty-helper "$work/bin/native-pty" \
  --checkpoint-binary "$work/bin/checkpoint" --context "$work/sproozi.kubeconfig" \
  --admin-probes --kubeconfig "$work/kubeconfig" \
  --namespace sproozi-system --agent-namespace sproozi-agents
```

Wait for Claude's socket and provider as above, with port 18775, before the
driver.

The Claude driver changes the profile marker before each move in the same way.
With `--admin-probes` it reads the rendered profile skill in each visit's exact
container or Pod and requires that visit's marker. Claude Code resumes a
conversation with the system prompt and skill listing it started with, so every
model request must carry the first visit's marker and the
`profile:mobility-check` skill.

Claude's preparation generates a fresh certificate covering Anthropic as well
as OpenAI, gateway, executor and host names. The TLS Secret
pipeline sends private key material directly to the dedicated cluster without
printing it. Retain `$claude/proof/report.json`; do not describe a rendered
manifest or successful deployment as a completed native round trip.
The renderers derive immutable trust ConfigMap names from the CA bytes and update
both gateway and native mounts. Changing fixture trust therefore creates a new
gateway Pod with the matching certificate and outbound trust pool, without
reusing a deleted immutable ConfigMap's cached mount.

## Shut down only this fixture

After a passing driver, stop the owned service and provider processes and
delete the named dedicated cluster. For an interrupted run, first cancel the
session ID recorded in its private report through its own home socket, preserving
the daemon's actual placement stop and accounting behavior. If cancellation
fails, retain the failure and exact resource records; do not assume killing a
client or home process stopped all workers. Their original deadlines still
apply. Inspect only the exact fixture allocation IDs when resolving a failed
cleanup; avoid global Docker pruning or deletion of unrelated namespaces.

```sh
kill -TERM "$claude_home_pid" "$claude_provider_pid"
wait "$claude_home_pid" "$claude_provider_pid" || true
kind delete cluster --name "$cluster"
```

Skip Claude's shutdown commands when it was never started. Preserve `$work` as
private evidence, including build identities, canonical image resolution,
generated configuration and reports. Its short-lived certificates are fixture
trust only; do not reuse them for another run or publish their private keys.
