#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
demo_env_load
docker_bin="${CONTAINER_TOOL:-docker}"
registry_image=registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
registry_name=sproozi-demo-registry
image=localhost:5001/sproozi-codex:demo

# A loopback-only registry gives local builds a genuine manifest digest. Kind
# receives the image with kind load; no registry is installed in Kubernetes.
if "$docker_bin" container inspect "$registry_name" >/dev/null 2>&1; then
  configured_image="$("$docker_bin" container inspect "$registry_name" --format '{{.Config.Image}}')"
  binding="$("$docker_bin" container inspect "$registry_name" --format '{{json .HostConfig.PortBindings}}')"
  [[ "$configured_image" == "$registry_image" && "$binding" == '{"5000/tcp":[{"HostIp":"127.0.0.1","HostPort":"5001"}]}' ]] || {
    echo 'Existing sproozi-demo-registry has unexpected image or ports; refusing to reuse it' >&2
    exit 1
  }
  "$docker_bin" start "$registry_name" >/dev/null
else
  "$docker_bin" run -d --name "$registry_name" -p 127.0.0.1:5001:5000 "$registry_image" >/dev/null
fi
ready=0
for attempt in {1..30}; do
  if curl --silent --fail --max-time 2 http://127.0.0.1:5001/v2/ >/dev/null; then ready=1; break; fi
  sleep 1
done
[[ "$ready" == 1 ]] || { echo 'Local demo registry did not become ready' >&2; exit 1; }
architecture="$("$docker_bin" info --format '{{.Architecture}}')"
case "$architecture" in aarch64|arm64) architecture=arm64 ;; x86_64|amd64) architecture=amd64 ;; *) exit 1 ;; esac
"$docker_bin" build --build-arg TARGETARCH="$architecture" \
  -f "$demo_repo_root/hack/demo/codex.Dockerfile" -t "$image" "$demo_repo_root/hack/demo"
"$docker_bin" run --rm --read-only --tmpfs /tmp --entrypoint /bin/sh "$image" -ec '
  codex --version
  git --version
  gh --version
  kubectl version --client
  curl --version
  test ! -e /usr/local/bin/agent-runner
'
"$docker_bin" push "$image"
digest="$("$docker_bin" image inspect "$image" --format '{{index .RepoDigests 0}}')"
[[ "$digest" =~ ^localhost:5001/sproozi-codex@sha256:[a-f0-9]{64}$ ]] || {
  echo 'Could not resolve the local image manifest digest' >&2; exit 1;
}
demo_env_set CODEX_IMAGE "$digest"
echo 'Pinned stock Codex image configured locally; no credentials were baked into it'
