#!/usr/bin/env bash
# Prove Claude Code on the cluster: build this checkout's images, create an
# owned Kind cluster with an enforcing CNI, install the controller, gateway and
# the Claude Code executor example with a deterministic in-cluster Anthropic
# fixture, then drive the public CLI through every scenario in verify.py.
# See README.md. make verify-claude-remote-kind runs this.
#
# Inputs: KIND_NODE_IMAGE (the Makefile sets it), optional
# SPROOZI_CNI_MANIFEST (default: the pinned Calico, fetched),
# SPROOZI_CLAUDE_AUTH (sign-in, the default, or gateway-key),
# SPROOZI_PRESERVE_ON_FAILURE=1 to keep a failed run's cluster,
# SPROOZI_NO_CACHE=1 to rebuild the client layers from scratch, and
# SPROOZI_TOKEN_REFRESH=1 to run only the refresh scenario on a cluster whose
# API server caps ServiceAccount tokens at about an hour. Extra arguments go
# to verify.py, for example --scenario task.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$root"
source hack/kind-up.sh
: "${KIND_NODE_IMAGE:?Supply the pinned Kind node image; make verify-claude-remote-kind sets it}"
auth="${SPROOZI_CLAUDE_AUTH:-sign-in}"
[[ "$auth" == sign-in || "$auth" == gateway-key ]] || { echo 'SPROOZI_CLAUDE_AUTH must be sign-in or gateway-key' >&2; exit 1; }
# The refresh scenario waits for a sandbox's first gateway token to expire.
# The controller asks for two hours; the API server's own cap shortens that
# to its lowest allowed value, 3607 seconds while it extends kubelet tokens,
# so the proof needs no Sproozi setting. The example's 30-minute duration caps
# are raised so the session outlives that token.
refresh="${SPROOZI_TOKEN_REFRESH:-0}"
[[ "$refresh" == 0 || "$refresh" == 1 ]] || { echo 'SPROOZI_TOKEN_REFRESH must be 0 or 1' >&2; exit 1; }
token_cap=3607
render_args=()
verify_args=()
if (( refresh )); then
  render_args=(--max-duration 90m)
  verify_args=(--scenario refresh --token-lifetime "$token_cap")
fi
for tool in kind kubectl docker go python3 openssl git; do
  command -v "$tool" >/dev/null 2>&1 || { echo "Missing required tool: $tool" >&2; exit 1; }
done
python3 -c 'import yaml' 2>/dev/null || { echo "Install Python's PyYAML package" >&2; exit 1; }

umask 077
run_id="$(date -u +%Y%m%d%H%M%S)-$$"
# images.py loads only into clusters named for the mobility fixtures.
cluster="sproozi-mobility-claude-${run_id}"
work="$root/.local/verification/claude-remote-kind/$run_id"
mkdir -p "$work/bin" "$work/tls" "$work/cli"
chmod 700 "$work" "$work/bin" "$work/tls" "$work/cli"
kubeconfig="$work/kubeconfig"
context="$work/cli/sproozi.kubeconfig"
sessions=sproozi-claude-sessions
echo "Evidence and logs: $work"

cleanup() {
  status=$?
  trap - EXIT
  rm -f "$work"/tls/*.key
  if (( kind_cluster_created )); then
    for target in "$sessions/sproozi-work" sproozi-system/sproozi-gateway sproozi-system/sproozi-controller-manager \
      sproozi-fixture/claude-fixture; do
      kubectl --kubeconfig "$kubeconfig" -n "${target%%/*}" logs "deployment/${target#*/}" \
        >"$work/${target#*/}.log" 2>&1 || true
    done
    kubectl --kubeconfig "$kubeconfig" -n sproozi-agents get pods,agentruns -o wide >"$work/agents.txt" 2>&1 || true
  fi
  if (( kind_cluster_created )) && [[ "$status" -ne 0 && "${SPROOZI_PRESERVE_ON_FAILURE:-0}" == 1 ]]; then
    echo "Preserving failed cluster $cluster (kubeconfig: $kubeconfig)" >&2
  elif (( kind_cluster_created )); then
    kind delete cluster --name "$cluster" >/dev/null 2>&1 || echo "Could not delete cluster $cluster" >&2
  fi
  exit "$status"
}
trap cleanup EXIT

step() { echo "== $*"; }

step "Building the CLI, PTY helper and kustomize"
make kustomize >/dev/null
go build -o "$work/bin/sproozi" ./cmd/sproozi
go build -o "$work/bin/native-pty" ./hack/verify/native-pty

step "Building the native and control images (logs in $work)"
cache=()
[[ "${SPROOZI_NO_CACHE:-0}" != 1 ]] || cache=(--no-cache)
docker build --pull ${cache[@]+"${cache[@]}"} -f hack/demo/codex.Dockerfile \
  -t sproozi-session/clients:codex . >"$work/codex-build.log" 2>&1
docker build ${cache[@]+"${cache[@]}"} -f internal/runtime/docker/image/clients.Dockerfile \
  --build-arg CODEX_CLIENTS_IMAGE=sproozi-session/clients:codex \
  -t sproozi-session/clients:claude-remote . >"$work/clients-build.log" 2>&1
docker build -f internal/runtime/docker/image/Dockerfile \
  --build-arg CLIENTS_IMAGE=sproozi-session/clients:claude-remote \
  -t docker.io/sproozi-session/native:fixture . >"$work/native-build.log" 2>&1
docker build -t docker.io/sproozi-session/control:fixture . >"$work/control-build.log" 2>&1
# The fixture provider runs on the native image's Python.
docker run --rm --entrypoint python3 docker.io/sproozi-session/native:fixture --version >/dev/null

step "Creating Kind cluster $cluster"
if [[ -z "${SPROOZI_CNI_MANIFEST:-}" ]]; then
  bash hack/verify/fetch-calico.sh "$work/calico.yaml"
  export SPROOZI_CNI_MANIFEST="$work/calico.yaml"
fi
export SPROOZI_CNI_SELECTOR="${SPROOZI_CNI_SELECTOR:-k8s-app=calico-node}"
SPROOZI_CNI_TIMEOUT="${SPROOZI_CNI_TIMEOUT:-300s}" kind_up "$cluster" "$kubeconfig"
export KUBECONFIG="$kubeconfig"

# cap_token_lifetime adds the API server's token cap to its static Pod
# manifest. The edit is renamed into place from outside the manifest
# directory, so the kubelet never reads a partial file. Only a token the
# restarted API server issues shows the cap is in force.
cap_token_lifetime() {
  local node="$cluster-control-plane" manifest=/etc/kubernetes/manifests/kube-apiserver.yaml lifetime
  local staged=/etc/kubernetes/kube-apiserver.yaml.capped
  docker exec "$node" sh -ec "cp $manifest $staged
    sed -i 's|^    - kube-apiserver\$|&\n    - --service-account-max-token-expiration=${token_cap}s|' $staged
    grep -q -- '--service-account-max-token-expiration=${token_cap}s' $staged
    mv $staged $manifest"
  for _ in $(seq 1 120); do
    lifetime="$(kubectl -n default create token default --duration=2h 2>/dev/null | python3 -c '
import base64, json, sys
payload = sys.stdin.read().strip().split(".")[1]
claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
print(claims["exp"] - claims["iat"])' 2>/dev/null)" || lifetime=
    if [[ -n "$lifetime" ]] && (( lifetime <= token_cap )); then
      echo "The API server issues a token asked for 2h with a ${lifetime}s lifetime"
      return 0
    fi
    sleep 5
  done
  echo "The API server did not cap token lifetimes at ${token_cap}s" >&2
  return 1
}
if (( refresh )); then
  step "Capping ServiceAccount token lifetimes at ${token_cap}s"
  cap_token_lifetime
fi

step "Loading images"
python3 hack/verify/session-mobility/images.py --cluster "$cluster" --output-directory "$work/images" >/dev/null
image() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]]["image"])' "$work/images/images.json" "$1"; }
native_ref="$(image native)"
control_ref="$(image control)"

step "Issuing fixture certificates"
tls="$work/tls"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=Claude remote proof CA' \
  -addext basicConstraints=critical,CA:TRUE -keyout "$tls/ca.key" -out "$tls/ca.pem" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj '/CN=sproozi-gateway.sproozi-system.svc' \
  -keyout "$tls/leaf.key" -out "$tls/leaf.csr" >/dev/null 2>&1
# In workload mode the gateway inspects every provider's signed-in hosts
# (cmd/gateway pruneModelHosts), so its certificate must cover them all.
workload_names=
[[ "$auth" != sign-in ]] || workload_names=,DNS:platform.claude.com,DNS:chatgpt.com,DNS:auth.openai.com,DNS:api.individual.githubcopilot.com,DNS:api.business.githubcopilot.com,DNS:api.enterprise.githubcopilot.com,DNS:github.com,DNS:api.github.com
cat >"$tls/leaf.ext" <<EOF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:sproozi-gateway.sproozi-system.svc,DNS:sproozi-gateway.sproozi-system.svc.cluster.local,DNS:api.anthropic.com,DNS:api.openai.com,DNS:sproozi-work.$sessions.svc,DNS:claude-fixture.sproozi-fixture.svc${workload_names}
EOF
openssl x509 -req -sha256 -days 1 -in "$tls/leaf.csr" -CA "$tls/ca.pem" -CAkey "$tls/ca.key" \
  -CAcreateserial -CAserial "$tls/ca.srl" -extfile "$tls/leaf.ext" -out "$tls/leaf.crt" >/dev/null 2>&1
openssl rand -hex 32 >"$tls/work-token"

step "Installing the controller, gateway, Claude Code executor example and fixture"
kubectl apply --server-side -f config/crd/bases >/dev/null
kubectl wait --for=condition=Established --timeout=60s \
  crd/agentpolicies.sproozi.com crd/agentruns.sproozi.com crd/agentruntimes.sproozi.com crd/agenttemplates.sproozi.com >/dev/null
for ns in sproozi-system sproozi-agents "$sessions" sproozi-fixture; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done
kubectl -n sproozi-system create secret tls sproozi-gateway-tls --cert="$tls/leaf.crt" --key="$tls/leaf.key" >/dev/null
kubectl -n sproozi-fixture create secret tls claude-fixture-tls --cert="$tls/leaf.crt" --key="$tls/leaf.key" >/dev/null
kubectl -n "$sessions" create secret generic sproozi-work-credentials --from-file=tls.crt="$tls/leaf.crt" \
  --from-file=tls.key="$tls/leaf.key" --from-file=work-token="$tls/work-token" >/dev/null
if [[ "$auth" == gateway-key ]]; then
  key="$(python3 -c 'import importlib.util,sys; s=importlib.util.spec_from_file_location("p",sys.argv[1]); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); print(m.API_KEY)' hack/verify/claude-remote-kind/provider.py)"
  kubectl -n sproozi-system create secret generic claude-remote-model-placeholder \
    --from-literal=anthropic-api-key="$key" >/dev/null
fi
python3 hack/verify/claude-remote-kind/render.py --control-image "$control_ref" --native-image "$native_ref" \
  --ca-file "$tls/ca.pem" --auth "$auth" ${render_args[@]+"${render_args[@]}"} >"$work/cluster.yaml"
kubectl apply --server-side -f "$work/cluster.yaml" >/dev/null
for target in sproozi-system/sproozi-controller-manager sproozi-system/sproozi-gateway \
  sproozi-fixture/claude-fixture "$sessions/sproozi-work"; do
  kubectl -n "${target%%/*}" rollout status "deployment/${target#*/}" --timeout=300s
done
if [[ "$auth" == sign-in ]]; then
  kubectl -n sproozi-agents wait pod/claude-login-seed --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
  kubectl -n sproozi-agents delete pod claude-login-seed --wait=true >/dev/null
fi

step "Writing the sproozi-cli context"
kubectl -n "$sessions" wait --for=jsonpath='{.data.token}' secret/sproozi-cli-token --timeout=60s >/dev/null
token="$(kubectl -n "$sessions" get secret sproozi-cli-token -o jsonpath='{.data.token}' | python3 -c 'import base64,sys; print(base64.b64decode(sys.stdin.read()).decode())')"
server="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')"
kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' \
  | python3 -c 'import base64,sys; sys.stdout.buffer.write(base64.b64decode(sys.stdin.read()))' >"$work/cli/cluster-ca.crt"
kubectl --kubeconfig "$context" config set-cluster proof --server="$server" \
  --certificate-authority="$work/cli/cluster-ca.crt" --embed-certs >/dev/null
kubectl --kubeconfig "$context" config set-credentials sproozi-cli --token="$token" >/dev/null
kubectl --kubeconfig "$context" config set-context sproozi --cluster=proof --user=sproozi-cli --namespace="$sessions" >/dev/null
kubectl --kubeconfig "$context" config use-context sproozi >/dev/null
chmod 600 "$context"
[[ "$(kubectl --kubeconfig "$context" -n "$sessions" auth can-i create pods --subresource=portforward)" == yes ]]

# The CLI needs no installation for remote commands; this configuration only
# places the profile it sends beside it.
python3 - "$work/cli/config.json" "$work/cli/state" <<'PY'
import json, os, sys
os.makedirs(sys.argv[2], mode=0o700, exist_ok=True)
config = {"directory": sys.argv[2], "local": {"Name": "laptop", "Harness": "claude-code"},
          "allowances": {"model.inference": {"units": {"bounded": True, "value": 100000},
                                             "costMicros": {"bounded": False, "value": 0}}}}
with open(sys.argv[1], "w") as stream:
    json.dump(config, stream)
os.chmod(sys.argv[1], 0o600)
PY

step "Running the proof"
python3 hack/verify/claude-remote-kind/verify.py --sproozi "$work/bin/sproozi" --pty-helper "$work/bin/native-pty" \
  --context "$context" --home-config "$work/cli/config.json" --kubeconfig "$kubeconfig" \
  --namespace "$sessions" --auth "$auth" --output-directory "$work/proof" ${verify_args[@]+"${verify_args[@]}"} "$@"
echo "Claude remote proof passed; report: $work/proof/report.json"
