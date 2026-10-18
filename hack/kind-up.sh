#!/usr/bin/env bash
# Create one disposable Kind cluster from hack/kind-cluster.yaml, write a private
# kubeconfig and wait for the enforcing CNI. Every Kind-owning script and
# `make demo` use this so cluster bootstrap cannot drift between them.
#
#   bash hack/kind-up.sh <cluster> <kubeconfig> [--no-cni]
#   source hack/kind-up.sh; kind_up <cluster> <kubeconfig> [--no-cni]
#
# Inputs: KIND_NODE_IMAGE (required), SPROOZI_CNI_MANIFEST and
# SPROOZI_CNI_SELECTOR (required unless --no-cni), SPROOZI_CNI_TIMEOUT, and
# optional KIND and KUBECTL binaries. Sourcing callers own cleanup: kind_up
# refuses an existing cluster, then sets kind_cluster_created=1 just before
# creating it, and exports KUBECONFIG.
kind_cluster_created=0

kind_up() {
  local cluster="$1" kubeconfig="$2" cni=1 timeout="${SPROOZI_CNI_TIMEOUT:-180s}"
  local kind="${KIND:-kind}" kubectl="${KUBECTL:-kubectl}" root
  [[ "${3:-}" != --no-cni ]] || cni=0
  : "${KIND_NODE_IMAGE:?KIND_NODE_IMAGE must be set}"
  if (( cni )); then
    if [[ -z "${SPROOZI_CNI_MANIFEST:-}" || ! -f "$SPROOZI_CNI_MANIFEST" ]]; then
      echo 'Set SPROOZI_CNI_MANIFEST to a pinned, enforcing CNI manifest' >&2
      return 1
    fi
    : "${SPROOZI_CNI_SELECTOR:?Set SPROOZI_CNI_SELECTOR to the CNI agent label selector}"
  fi
  if "$kind" get clusters | grep -Fxq "$cluster"; then
    echo "Refusing to reuse existing cluster $cluster" >&2
    return 1
  fi
  root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  kind_cluster_created=1
  "$kind" create cluster --name "$cluster" --config "$root/hack/kind-cluster.yaml" \
    --image "$KIND_NODE_IMAGE" --kubeconfig "$kubeconfig"
  chmod 600 "$kubeconfig"
  export KUBECONFIG="$kubeconfig"
  "$kubectl" config use-context "kind-$cluster" >/dev/null
  (( cni )) || return 0
  "$kubectl" apply --filename "$SPROOZI_CNI_MANIFEST" >/dev/null
  # A newly applied DaemonSet may not have created any Pods yet. Readiness alone
  # fails immediately for an empty selector result instead of waiting for Pods.
  "$kubectl" wait --namespace kube-system --for=create pods --selector "$SPROOZI_CNI_SELECTOR" --timeout="$timeout"
  "$kubectl" wait --namespace kube-system --for=condition=Ready pods --selector "$SPROOZI_CNI_SELECTOR" --timeout="$timeout"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  [[ $# -ge 2 ]] || { echo "usage: $0 <cluster> <kubeconfig> [--no-cni]" >&2; exit 2; }
  kind_up "$@"
fi
