#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
demo_env_load
: "${CODEX_IMAGE:?Run make demo-setup first}"
: "${KIND_NODE_IMAGE:?Supply the pinned Kind node image}"
cluster="sproozi-image-check-$(date -u +%Y%m%d%H%M%S)-$$"
test_dir="$(mktemp -d)"
kubeconfig="$test_dir/kubeconfig"
created=0
cleanup() {
  status=$?
  if [[ "$created" == 1 ]]; then
    if [[ "$status" != 0 && -f "$kubeconfig" ]]; then
      kubectl --kubeconfig "$kubeconfig" get pod sproozi-image-check -o wide || true
      kubectl --kubeconfig "$kubeconfig" describe pod sproozi-image-check || true
      kubectl --kubeconfig "$kubeconfig" logs sproozi-image-check || true
    fi
    if ! kind delete cluster --name "$cluster" --kubeconfig "$kubeconfig"; then
      echo "Could not clean up image-check cluster $cluster; kubeconfig retained at $kubeconfig" >&2
      exit 1
    fi
  fi
  rm -f "$kubeconfig"
  rmdir "$test_dir"
  exit "$status"
}
trap cleanup EXIT
kind get clusters | grep -Fxq "$cluster" && { echo 'Refusing cluster reuse' >&2; exit 1; }
# No CNI is needed for this host-networked, explicitly node-bound tool check.
# This does not demonstrate AgentRun execution or NetworkPolicy enforcement.
created=1
kind create cluster --name "$cluster" --config "$demo_repo_root/hack/kind-cluster.yaml" \
  --image "$KIND_NODE_IMAGE" --kubeconfig "$kubeconfig"
docker pull "$CODEX_IMAGE"
bash "$demo_repo_root/hack/demo/load-image.sh" "$cluster"
kubectl --kubeconfig "$kubeconfig" wait --for=create serviceaccount/default --timeout=30s
kubectl --kubeconfig "$kubeconfig" run sproozi-image-check --restart=Never \
  --image "$CODEX_IMAGE" --image-pull-policy=Always \
  --overrides "{\"spec\":{\"nodeName\":\"$cluster-control-plane\",\"hostNetwork\":true,\"automountServiceAccountToken\":false}}" \
  --command -- /bin/sh -ec 'codex --version; git --version; gh --version; kubectl version --client; curl --version; test ! -e /usr/local/bin/agent-runner'
passed=0
for attempt in {1..45}; do
  phase="$(kubectl --kubeconfig "$kubeconfig" get pod sproozi-image-check -o jsonpath='{.status.phase}')"
  waiting="$(kubectl --kubeconfig "$kubeconfig" get pod sproozi-image-check -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}')"
  case "$waiting" in ErrImageNeverPull|ImagePullBackOff|CreateContainerConfigError) exit 1 ;; esac
  case "$phase" in Succeeded) passed=1; break ;; Failed) exit 1 ;; esac
  sleep 2
done
[[ "$passed" == 1 ]] || { echo 'Timed out waiting for the Kind tools check' >&2; exit 1; }
kubectl --kubeconfig "$kubeconfig" logs sproozi-image-check
echo 'Pinned Codex tools image ran successfully inside disposable Kind'
