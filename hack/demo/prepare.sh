#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
env_file="${SPROOZI_DEMO_ENV_FILE:-$repo_root/.local/demo.env}"
if [[ -f "$env_file" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$env_file"
  set +a
fi
bash "$repo_root/hack/demo/check.sh" >/dev/null
kubectl_bin="${KUBECTL:-kubectl}"

work_dir="$repo_root/.local/demo/tls"
mkdir -p "$work_dir"
umask 077
if [[ ! -s "$work_dir/ca.crt" || ! -s "$work_dir/ca.key" || ! -s "$work_dir/tls.crt" || ! -s "$work_dir/tls.key" ]] || \
  ! openssl x509 -checkend 3600 -noout -in "$work_dir/tls.crt" >/dev/null 2>&1 || \
  ! openssl x509 -checkhost sproozi-gateway.sproozi-system.svc.cluster.local -noout -in "$work_dir/tls.crt" >/dev/null 2>&1; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -subj "/CN=Sproozi demo CA" \
    -keyout "$work_dir/ca.key" -out "$work_dir/ca.crt" >/dev/null 2>&1
  openssl req -newkey rsa:2048 -nodes \
    -subj "/CN=sproozi-gateway.sproozi-system.svc" \
    -keyout "$work_dir/tls.key" -out "$work_dir/tls.csr" >/dev/null 2>&1
  cat >"$work_dir/extensions.cnf" <<'EOF'
subjectAltName=DNS:sproozi-gateway.sproozi-system.svc,DNS:sproozi-gateway.sproozi-system.svc.cluster.local,DNS:kubernetes.default.svc,DNS:api.openai.com,DNS:api.anthropic.com,DNS:github.com,DNS:api.github.com,DNS:pypi.org,DNS:files.pythonhosted.org,DNS:proxy.golang.org,DNS:sum.golang.org,DNS:egress.sproozi.internal
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EOF
  openssl x509 -req -days 2 -sha256 \
    -in "$work_dir/tls.csr" -CA "$work_dir/ca.crt" -CAkey "$work_dir/ca.key" -CAcreateserial \
    -extfile "$work_dir/extensions.cnf" -out "$work_dir/tls.crt" >/dev/null 2>&1
fi

openai_key_file="$(mktemp "$work_dir/openai-api-key.XXXXXX")"
trap 'rm -f "$openai_key_file"' EXIT
if [[ "${MODEL_AUTH_MODE:-api_key}" == chatgpt ]]; then
  "$kubectl_bin" create secret generic model-gateway-chatgpt -n sproozi-system \
    --from-file=session.json="$CHATGPT_SESSION_FILE" --dry-run=client -o yaml | "$kubectl_bin" apply -f -
else
  printf '%s' "$OPENAI_API_KEY" >"$openai_key_file"
  "$kubectl_bin" create secret generic model-gateway-credentials -n sproozi-system \
    --from-file=openai-api-key="$openai_key_file" --dry-run=client -o yaml | "$kubectl_bin" apply -f -
fi
"$kubectl_bin" create configmap model-gateway-auth -n sproozi-system \
  --from-literal=mode="${MODEL_AUTH_MODE:-api_key}" --dry-run=client -o yaml | "$kubectl_bin" apply -f -
"$kubectl_bin" create secret generic capability-proxy-github-app -n sproozi-system \
  --from-literal=app-id="$GITHUB_APP_ID" \
  --from-literal=installation-id="$GITHUB_APP_INSTALLATION_ID" \
  --from-file=private-key.pem="$GITHUB_APP_PRIVATE_KEY_FILE" \
  --dry-run=client -o yaml | "$kubectl_bin" apply -f -
"$kubectl_bin" create secret tls sproozi-gateway-tls -n sproozi-system \
  --cert="$work_dir/tls.crt" --key="$work_dir/tls.key" --dry-run=client -o yaml | "$kubectl_bin" apply -f -
"$kubectl_bin" create configmap sproozi-controller-trust -n sproozi-system \
  --from-file=ca.crt="$work_dir/ca.crt" --dry-run=client -o yaml | "$kubectl_bin" apply -f -
"$kubectl_bin" delete configmap sproozi-sandbox-trust -n sproozi-agents --ignore-not-found --wait=true >/dev/null
"$kubectl_bin" create configmap sproozi-sandbox-trust -n sproozi-agents \
  --from-file=ca-bundle.pem="$work_dir/ca.crt" --dry-run=client -o json | \
  python3 -c 'import json,sys; obj=json.load(sys.stdin); obj["immutable"]=True; json.dump(obj,sys.stdout)' | \
  "$kubectl_bin" create -f -
