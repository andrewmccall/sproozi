#!/usr/bin/env bash
set -euo pipefail

# A Kind cluster without an enforcing CNI is not evidence for Sproozi's
# network guarantees. Callers must provide a pinned CNI manifest and a label
# selector identifying its ready agents.
cd "$(git rev-parse --show-toplevel)"
source hack/kind-up.sh

for tool in kind kubectl docker make; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "Missing required tool: $tool" >&2
    exit 1
  }
done

if [[ "${SPROOZI_PRESERVE_ON_FAILURE:-0}" != 0 && "${SPROOZI_PRESERVE_ON_FAILURE:-0}" != 1 ]]; then
  echo 'SPROOZI_PRESERVE_ON_FAILURE must be 0 or 1' >&2
  exit 1
fi

run_id="${SPROOZI_VERIFY_ID:-$(date -u +%Y%m%d%H%M%S)-$$}"
cluster="kind-sproozi-verify-${run_id}"
kubeconfig="${SPROOZI_KUBECONFIG:-$(mktemp -p "${TMPDIR:-/tmp}" sproozi-kind.XXXXXX)}"
cleanup() {
  status=$?
  if (( kind_cluster_created )) && [[ "$status" -ne 0 && "${SPROOZI_PRESERVE_ON_FAILURE:-0}" == 1 ]]; then
    echo "Preserving failed verification cluster $cluster (kubeconfig: $kubeconfig)" >&2
  elif (( kind_cluster_created )); then
    kind delete cluster --name "$cluster" --kubeconfig "$kubeconfig" >/dev/null 2>&1 || true
    rm -f "$kubeconfig"
  else
    [[ -n "${SPROOZI_KUBECONFIG:-}" ]] || rm -f "$kubeconfig"
  fi
  exit "$status"
}
trap cleanup EXIT

kind_up "$cluster" "$kubeconfig"

# This script owns creation and cleanup of the cluster; the e2e suite must not
# delete it. The suite receives the explicit cluster name and kubeconfig and
# never falls back to the operator's default context. The cluster is deleted
# right after the suite, so the suite skips teardown that only removes
# resources.
SPROOZI_E2E_DISPOSABLE_CLUSTER=1 make test-e2e-existing KIND_CLUSTER="$cluster" KIND_NODE_IMAGE="${KIND_NODE_IMAGE}" KIND=kind KUBECTL="kubectl"
echo "Kind verification passed for $cluster"
