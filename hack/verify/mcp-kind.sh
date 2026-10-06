#!/usr/bin/env bash
# Isolated, model-backed configured MCP acceptance. The caller owns the session.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
: "${MCP_PROOF_IMAGE:?Supply the freshly built controller/gateway image}"
: "${MCP_PROVIDER_IMAGE:?Build hack/verify/mcp-provider/Dockerfile first}"
: "${CODEX_IMAGE:?Supply a pinned stock Codex image}"
: "${CHATGPT_SESSION_FILE:?Supply a dedicated single-owner ChatGPT session file}"
: "${SPROOZI_CNI_MANIFEST:?Supply the pinned Calico v3.30.4 manifest}"
: "${MCP_PROOF_REPORT:?Supply the redacted report path}"
[[ -f "$CHATGPT_SESSION_FILE" && "$CODEX_IMAGE" == *@sha256:* ]]
expected_cni=1e0f605a1f0a337a198a6ed8512c4ade67a2d8ab239f0bb67062a663fed84e6d
actual_cni="$(shasum -a 256 "$SPROOZI_CNI_MANIFEST" | cut -d' ' -f1)"
[[ "$actual_cni" == "$expected_cni" ]] || { echo 'Calico manifest checksum mismatch' >&2; exit 1; }
cluster="sproozi-mcp-verify-$(date -u +%Y%m%d%H%M%S)-$$"
umask 077
work_dir="$(mktemp -d)"
export KUBECONFIG="$work_dir/kubeconfig"
created=false
session_loaded=false
cleanup() {
  result=$?
  trap - EXIT
  if [[ "$session_loaded" == true ]]; then
    # Preserve the newest rotating credential even if acceptance failed. Never
    # print Secret bodies or put them in the public report.
    if ! python3 - "$CHATGPT_SESSION_FILE" <<'PY'
import base64,json,os,subprocess,sys,tempfile
from pathlib import Path
result=subprocess.run(['kubectl','get','secret','model-gateway-chatgpt','-n','sproozi-system','-o','json'],capture_output=True,text=True,timeout=30)
if result.returncode:
    raise SystemExit('Could not recover the rotating session from the proof cluster')
data=base64.b64decode(json.loads(result.stdout)['data']['session.json'])
target=Path(sys.argv[1])
fd,name=tempfile.mkstemp(dir=target.parent,prefix='.mcp-session-')
with os.fdopen(fd,'wb') as stream: stream.write(data)
os.replace(name,target)
PY
    then
      echo "Session recovery failed; preserved cluster $cluster and private directory $work_dir" >&2
      exit 1
    fi
  fi
  if [[ "$created" == true ]]; then
    if ! kind delete cluster --name "$cluster"; then
      echo "Could not remove proof cluster $cluster; private directory $work_dir retained" >&2
      exit 1
    fi
  fi
  rm -rf "$work_dir"
  exit "$result"
}
trap cleanup EXIT
created=true
kind create cluster --name "$cluster" --config "$repo_root/hack/kind-cluster.yaml" \
  --image kindest/node@sha256:050072256b9a903bd914c0b2866828150cb229cea0efe5892e2b644d5dd3b34f
kubectl apply -f "$SPROOZI_CNI_MANIFEST" >/dev/null
kubectl wait pods -n kube-system -l k8s-app=calico-node --for=condition=Ready --timeout=180s
kind load docker-image "$MCP_PROOF_IMAGE" "$MCP_PROVIDER_IMAGE" --name "$cluster"
bash "$repo_root/hack/demo/load-image.sh" "$cluster"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=MCP proof CA' \
  -keyout "$work_dir/ca.key" -out "$work_dir/ca.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj '/CN=sproozi-gateway.sproozi-system.svc' \
  -keyout "$work_dir/tls.key" -out "$work_dir/tls.csr" >/dev/null 2>&1
cat > "$work_dir/extensions.cnf" <<'EOF'
subjectAltName=DNS:sproozi-gateway.sproozi-system.svc,DNS:sproozi-gateway.sproozi-system.svc.cluster.local,DNS:api.openai.com,DNS:mcp-docs.sproozi-system.svc,DNS:mcp-inventory.sproozi-system.svc
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EOF
openssl x509 -req -days 1 -sha256 -in "$work_dir/tls.csr" -CA "$work_dir/ca.crt" -CAkey "$work_dir/ca.key" \
  -CAcreateserial -extfile "$work_dir/extensions.cnf" -out "$work_dir/tls.crt" >/dev/null 2>&1
# The scratch gateway still needs public trust for the model provider.
docker run --rm --entrypoint /bin/cat golang:1.26-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 \
  /etc/ssl/certs/ca-certificates.crt > "$work_dir/ca-bundle.pem"
cat "$work_dir/ca.crt" >> "$work_dir/ca-bundle.pem"
openssl rand -hex 32 > "$work_dir/docs-token"
openssl rand -hex 32 > "$work_dir/inventory-token"
kubectl create namespace sproozi-system
kubectl create namespace sproozi-agents
kubectl create secret generic model-gateway-chatgpt -n sproozi-system --from-file=session.json="$CHATGPT_SESSION_FILE"
session_loaded=true
kubectl create configmap model-gateway-auth -n sproozi-system --from-literal=mode=chatgpt
kubectl create secret tls sproozi-gateway-tls -n sproozi-system --cert="$work_dir/tls.crt" --key="$work_dir/tls.key"
kubectl create secret generic mcp-provider-credentials -n sproozi-system \
  --from-file=docs-token="$work_dir/docs-token" --from-file=inventory-token="$work_dir/inventory-token"
kubectl create configmap mcp-provider-trust -n sproozi-system --from-file=ca-bundle.pem="$work_dir/ca-bundle.pem"
kubectl create configmap sproozi-controller-trust -n sproozi-system --from-file=ca.crt="$work_dir/ca.crt"
kubectl create configmap sproozi-sandbox-trust -n sproozi-agents --from-file=ca-bundle.pem="$work_dir/ca.crt"
kubectl patch configmap sproozi-sandbox-trust -n sproozi-agents --type=merge -p '{"immutable":true}'
python3 "$repo_root/hack/verify/render-mcp-deployment.py" --image "$MCP_PROOF_IMAGE" --provider-image "$MCP_PROVIDER_IMAGE" | kubectl apply --server-side --field-manager=sproozi-mcp-proof -f - >/dev/null
kubectl wait crd/agentpolicies.sproozi.com crd/agentruns.sproozi.com crd/agentruntimes.sproozi.com crd/agenttemplates.sproozi.com --for=condition=Established --timeout=60s
for deployment in sproozi-controller-manager sproozi-gateway mcp-docs mcp-inventory; do
  kubectl rollout status "deployment/$deployment" -n sproozi-system --timeout=180s
done
python3 "$repo_root/hack/verify/render-mcp-fixture.py" --mode configured --model-auth chatgpt --codex-image "$CODEX_IMAGE" | kubectl apply -f - >/dev/null
python3 "$repo_root/hack/verify/mcp-live.py" --mode configured --kubeconfig "$KUBECONFIG" --cluster "$cluster" --report "$MCP_PROOF_REPORT"
