#!/usr/bin/env bash
set -euo pipefail

# A Kind cluster without an enforcing CNI is not evidence for Sproozi's
# network guarantees. Callers must provide a pinned CNI manifest and a label
# selector identifying its ready agents.
cd "$(git rev-parse --show-toplevel)"

for tool in kind kubectl docker make; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "Missing required tool: $tool" >&2
    exit 1
  }
done

cni_manifest="${SPROOZI_CNI_MANIFEST:-}"
cni_selector="${SPROOZI_CNI_SELECTOR:-}"
if [[ -z "$cni_manifest" || ! -f "$cni_manifest" ]]; then
  echo 'Set SPROOZI_CNI_MANIFEST to a pinned, enforcing CNI manifest' >&2
  exit 1
fi
if [[ -z "$cni_selector" ]]; then
  echo 'Set SPROOZI_CNI_SELECTOR to the CNI agent label selector' >&2
  exit 1
fi

if [[ "${SPROOZI_PRESERVE_ON_FAILURE:-0}" != 0 && "${SPROOZI_PRESERVE_ON_FAILURE:-0}" != 1 ]]; then
  echo 'SPROOZI_PRESERVE_ON_FAILURE must be 0 or 1' >&2
  exit 1
fi

run_id="${SPROOZI_VERIFY_ID:-$(date -u +%Y%m%d%H%M%S)-$$}"
cluster="kind-sproozi-verify-${run_id}"
kubeconfig="${SPROOZI_KUBECONFIG:-$(mktemp -p "${TMPDIR:-/tmp}" sproozi-kind.XXXXXX)}"
created=0
cleanup() {
  status=$?
  if (( created )) && [[ "$status" -ne 0 && "${SPROOZI_PRESERVE_ON_FAILURE:-0}" == 1 ]]; then
    echo "Preserving failed verification cluster $cluster (kubeconfig: $kubeconfig)" >&2
  elif (( created )); then
    kind delete cluster --name "$cluster" --kubeconfig "$kubeconfig" >/dev/null 2>&1 || true
    rm -f "$kubeconfig"
  else
    [[ -n "${SPROOZI_KUBECONFIG:-}" ]] || rm -f "$kubeconfig"
  fi
  exit "$status"
}
trap cleanup EXIT

if kind get clusters | grep -Fxq "$cluster"; then
  echo "Refusing to reuse existing cluster $cluster; choose a new SPROOZI_VERIFY_ID" >&2
  exit 1
fi

kind create cluster --name "$cluster" --config hack/kind-cluster.yaml --image "${KIND_NODE_IMAGE:?KIND_NODE_IMAGE must be set}" --kubeconfig "$kubeconfig"
created=1
export KUBECONFIG="$kubeconfig"
kubectl config use-context "kind-$cluster" >/dev/null
kubectl apply --filename "$cni_manifest"
# A newly applied DaemonSet may not have created any Pods yet. Readiness alone
# fails immediately for an empty selector result instead of waiting for Pods.
kubectl wait --namespace kube-system --for=create pods --selector "$cni_selector" --timeout="${SPROOZI_CNI_TIMEOUT:-180s}"
kubectl wait --namespace kube-system --for=condition=Ready pods --selector "$cni_selector" --timeout="${SPROOZI_CNI_TIMEOUT:-180s}"

# This script owns creation and cleanup of the cluster. The test target below
# therefore must not invoke setup-test-e2e or delete the cluster behind us.
# The e2e suite receives the explicit cluster name and kubeconfig; it never
# falls back to the operator's default context.
make test-e2e-existing KIND_CLUSTER="$cluster" KIND_NODE_IMAGE="${KIND_NODE_IMAGE}" KIND=kind KUBECTL="kubectl"
echo "Kind verification passed for $cluster"
