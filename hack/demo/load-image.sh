#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
demo_env_load
cluster="${1:?Supply the disposable Kind cluster name}"
: "${CODEX_IMAGE:?Run make demo-setup first}"
kind_bin="${KIND:-kind}"
docker_bin="${CONTAINER_TOOL:-docker}"

if [[ "$CODEX_IMAGE" == localhost:5001/sproozi-codex@sha256:* ]]; then
  registry_name=sproozi-demo-registry
  "$docker_bin" start "$registry_name" >/dev/null
  network="$("$docker_bin" inspect "$registry_name" --format '{{json .NetworkSettings.Networks.kind}}')"
  if [[ "$network" == null ]]; then
    "$docker_bin" network connect kind "$registry_name"
  fi
  nodes="$("$kind_bin" get nodes --name "$cluster")"
  [[ -n "$nodes" ]] || { echo 'The selected Kind cluster has no nodes' >&2; exit 1; }
  while IFS= read -r node; do
    "$docker_bin" exec "$node" mkdir -p /etc/containerd/certs.d/localhost:5001
    "$docker_bin" cp "$demo_repo_root/hack/demo/registry-hosts.toml" \
      "$node:/etc/containerd/certs.d/localhost:5001/hosts.toml"
  done <<<"$nodes"
  echo 'Configured exact-digest pulls from the local registry in the disposable Kind nodes'
else
  "$kind_bin" load docker-image "$CODEX_IMAGE" --name "$cluster"
fi
