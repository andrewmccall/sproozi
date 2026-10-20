#!/usr/bin/env bash
# Run the manual AgentRun demo on a new disposable Kind cluster. `make demo`
# calls this after `make demo-check`.
#
#   bash hack/demo/run.sh [cluster]
#
# Inputs: IMG, KIND_NODE_IMAGE, optional MAKE, KIND, KUBECTL and
# CONTAINER_TOOL, plus the demo environment file. Values from the command line
# or the environment win over the file. The cluster defaults to sproozi-demo.
# hack/demo/check.sh runs first, so missing credentials fail before any build
# or cluster; `make demo` sets SPROOZI_DEMO_CHECKED=1 because demo-check has
# just run it. If setup fails or is interrupted (INT, TERM or HUP) before the
# run is submitted, the cluster is deleted, but only when this run created it;
# set SPROOZI_PRESERVE_ON_FAILURE=1 to keep it. Once the run is submitted the
# cluster is kept, so a failed acceptance check can be inspected. While it
# runs, .local/demo/<cluster>.lock stops a second run with the same name.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$repo_root/hack/kind-up.sh"
# The shared loader validates the file before sourcing it. Non-empty values
# the caller set win over the file, so `make demo IMG=...` beats an IMG in it.
source "$repo_root/hack/demo/env.sh"
demo_env_load
cluster="${1:-sproozi-demo}"
make_bin="${MAKE:-make}"
kind_bin="${KIND:-kind}"
kubectl_bin="${KUBECTL:-kubectl}"
container_tool="${CONTAINER_TOOL:-docker}"
img="${IMG:-}"

[[ "$cluster" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || {
  echo 'KIND_CLUSTER must be lowercase letters, digits and hyphens, for example sproozi-demo' >&2
  exit 1
}
# Never fall back to the kubebuilder default image: an empty IMG would make
# `make deploy` write an image reference with no name or tag.
case "$img" in
  '') echo 'IMG is empty or unset; set IMG to a non-latest local tag, for example make demo IMG=controller:sproozi-demo' >&2; exit 1 ;;
  *:latest|latest) echo 'Set IMG to a non-latest local tag, for example controller:sproozi-demo' >&2; exit 1 ;;
esac
[[ "$img" =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]] || { echo 'IMG must be an image reference without spaces, for example controller:sproozi-demo' >&2; exit 1; }
if [[ "${SPROOZI_DEMO_CHECKED:-0}" != 1 ]]; then
  bash "$repo_root/hack/demo/check.sh"
fi
: "${CODEX_IMAGE:?Run make demo-setup first}"
: "${SPROOZI_DEMO_REPOSITORY:?Run make demo-setup first}"
: "${SPROOZI_CNI_SELECTOR:?Run make demo-setup first}"
[[ -n "${SPROOZI_CNI_MANIFEST:-}" && -f "$SPROOZI_CNI_MANIFEST" ]] || { echo 'Run make demo-setup first: SPROOZI_CNI_MANIFEST is missing' >&2; exit 1; }
grep -Eqv '^[[:space:]]*(#.*)?$' "$SPROOZI_CNI_MANIFEST" || { echo "SPROOZI_CNI_MANIFEST has no content: $SPROOZI_CNI_MANIFEST" >&2; exit 1; }
mkdir -p "$repo_root/.local/demo"
kubeconfig="$repo_root/.local/demo/$cluster.kubeconfig"
# One run per cluster name: mkdir is atomic, so a second run with the same
# name stops here instead of racing the existence check below and deleting
# the first run's cluster and kubeconfig during its own cleanup.
lock="$repo_root/.local/demo/$cluster.lock"
locked=0

submitted=0
# Bash runs EXIT traps on errors and normal exit, not on fatal signals, so
# INT, TERM and HUP clean up explicitly and exit with 128+signal. A signal
# sent only to this script takes effect when the current command returns.
cleanup() {
  local status="$1"
  trap - EXIT
  trap '' INT TERM HUP
  (( status <= 128 )) || echo "Demo interrupted by a signal (exit $status)" >&2
  if (( submitted )) && (( status != 0 )); then
    echo "Kept demo cluster $cluster for inspection: export KUBECONFIG=$kubeconfig" >&2
  elif (( kind_cluster_created )) && (( status != 0 )); then
    if [[ "${SPROOZI_PRESERVE_ON_FAILURE:-0}" == 1 ]]; then
      echo "Preserving failed demo cluster $cluster (kubeconfig: $kubeconfig)" >&2
    else
      echo "Deleting demo cluster $cluster after the failure; set SPROOZI_PRESERVE_ON_FAILURE=1 to keep it" >&2
      "$kind_bin" delete cluster --name "$cluster" --kubeconfig "$kubeconfig" >/dev/null 2>&1 || true
      rm -f "$kubeconfig"
    fi
  fi
  (( ! locked )) || rm -rf "$lock"
  exit "$status"
}
trap 'cleanup $?' EXIT
trap 'cleanup 129' HUP
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

if ! mkdir "$lock" 2>/dev/null; then
  echo "Another demo run is using cluster $cluster; wait for it to finish or choose another KIND_CLUSTER. If no run is active, remove $lock" >&2
  exit 1
fi
locked=1
echo "$$" >"$lock/pid"
if "$kind_bin" get clusters | grep -Fxq "$cluster"; then
  echo "Refusing to reuse existing cluster $cluster; choose another KIND_CLUSTER or delete it" >&2
  exit 1
fi

"$container_tool" pull "$CODEX_IMAGE"
"$make_bin" docker-build IMG="$img"

kind_up "$cluster" "$kubeconfig"
export KUBECONFIG="$kubeconfig"
cd "$repo_root"
"$kind_bin" load docker-image "$img" --name "$cluster"
bash hack/demo/load-image.sh "$cluster"
"$make_bin" install
"$kubectl_bin" apply -f examples/sre-demo/namespaces.yaml
SPROOZI_DEMO_CHECKED=1 bash hack/demo/prepare.sh
"$make_bin" deploy IMG="$img"
"$kubectl_bin" rollout status deployment/sproozi-controller-manager -n sproozi-system --timeout=180s
"$kubectl_bin" rollout status deployment/sproozi-gateway -n sproozi-system --timeout=180s
python3 hack/demo/render.py --codex-image "$CODEX_IMAGE" --repository "$SPROOZI_DEMO_REPOSITORY" \
  --model-auth "${MODEL_AUTH_MODE:-api_key}" | "$kubectl_bin" apply -f -
submitted=1
SPROOZI_LIVE_ACCEPT=1 SPROOZI_DEMO_REPO="$SPROOZI_DEMO_REPOSITORY" SPROOZI_KUBECONFIG="$kubeconfig" \
  bash hack/verify/live.sh
echo "Export the isolated context with: export KUBECONFIG=$kubeconfig"
