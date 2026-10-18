#!/usr/bin/env bash
set -euo pipefail

# A Kind cluster without an enforcing CNI is not evidence for Sproozi's
# network guarantees. Callers must provide a pinned CNI manifest and a label
# selector identifying its ready agents.
cd "$(git rev-parse --show-toplevel)"
source hack/kind-up.sh

for tool in kind kubectl docker make go python3; do
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
manager_image="${SPROOZI_E2E_MANAGER_IMAGE:-example.com/sproozi:v0.0.1}"
logs="$(mktemp -d -p "${TMPDIR:-/tmp}" sproozi-kind-jobs.XXXXXX)"

# Independent setup runs as background jobs: the cluster, the image builds,
# the nodes' Hermes pull, and the e2e test compile. Each job writes its own log, and
# the first job to fail stops the run and prints that log.
job_pids=()
job_names=()

start_job() {
  local name="$1"
  shift
  "$@" >"$logs/$name.log" 2>&1 &
  job_pids+=("$!")
  job_names+=("$name")
}

stop_jobs() {
  local pid
  for pid in ${job_pids[@]+"${job_pids[@]}"}; do
    [[ -z "$pid" ]] || kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
  done
}

# wait_jobs NAME... returns when the named jobs have succeeded. Every job is
# checked on each pass, so a failure in any of them ends the run at once.
wait_jobs() {
  local i pid name status pending wanted
  while :; do
    pending=0
    for i in "${!job_pids[@]}"; do
      pid="${job_pids[$i]}"
      name="${job_names[$i]}"
      [[ -n "$pid" ]] || continue
      wanted=0
      for want in "$@"; do [[ "$want" != "$name" ]] || wanted=1; done
      if kill -0 "$pid" 2>/dev/null; then
        (( ! wanted )) || pending=1
        continue
      fi
      status=0
      wait "$pid" || status=$?
      job_pids[$i]=''
      if (( status != 0 )); then
        echo "Background job $name failed with exit status $status; its log follows:" >&2
        cat "$logs/$name.log" >&2
        exit "$status"
      fi
      echo "Finished $name"
    done
    (( pending )) || return 0
    sleep 1
  done
}

cleanup() {
  status=$?
  stop_jobs
  rm -rf "$logs"
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

# Each job gets its own process group, so stopping a job also stops the
# docker and kind processes it started.
set -m

orchestration=1
[[ "${SPROOZI_E2E_ORCHESTRATION:-}" != 0 ]] || orchestration=0

start_job manager-image make docker-build IMG="$manager_image"
if (( orchestration )); then
  start_job acceptance-images python3 hack/verify/orchestration-kind/build.py build
fi
start_job e2e-compile go test -tags=e2e -c -o /dev/null ./test/e2e/

# kind_up refuses an existing cluster in its own process; check here as well
# so that cleanup never deletes a cluster this script did not create.
if kind get clusters | grep -Fxq "$cluster"; then
  echo "Refusing to reuse existing cluster $cluster" >&2
  exit 1
fi
kind_cluster_created=1
start_job cluster kind_up "$cluster" "$kubeconfig"

wait_jobs cluster
cat "$logs/cluster.log"
export KUBECONFIG="$kubeconfig"
if (( orchestration )); then
  # The nodes pull the digest-pinned Hermes image themselves while the
  # remaining images build and load.
  pull_hermes() {
    KIND_CLUSTER="$cluster" KIND=kind python3 hack/verify/orchestration-kind/build.py pull-hermes
  }
  start_job hermes-pull pull_hermes
fi

wait_jobs manager-image
start_job manager-load kind load docker-image "$manager_image" --name "$cluster"
if (( orchestration )); then
  wait_jobs acceptance-images hermes-pull
  load_acceptance_images() {
    KIND_CLUSTER="$cluster" KIND=kind python3 hack/verify/orchestration-kind/build.py load
  }
  start_job acceptance-load load_acceptance_images
fi
wait_jobs ${job_names[@]+"${job_names[@]}"}
set +m

# This script owns creation and cleanup of the cluster; the e2e suite must not
# delete it. The suite receives the explicit cluster name and kubeconfig and
# never falls back to the operator's default context. The cluster is deleted
# right after the suite, so the suite skips teardown that only removes
# resources. The images are already built and loaded.
SPROOZI_E2E_DISPOSABLE_CLUSTER=1 SPROOZI_E2E_IMAGES_LOADED=1 SPROOZI_E2E_MANAGER_IMAGE="$manager_image" \
  make test-e2e-existing KIND_CLUSTER="$cluster" KIND_NODE_IMAGE="${KIND_NODE_IMAGE}" KIND=kind KUBECTL="kubectl"
echo "Kind verification passed for $cluster"
