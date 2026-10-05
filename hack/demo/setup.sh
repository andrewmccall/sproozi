#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
demo_env_load

# This is a replaceable Kind-only default, never a product CNI dependency.
if [[ -z "${SPROOZI_CNI_MANIFEST:-}" && -z "${SPROOZI_CNI_SELECTOR:-}" ]]; then
  manifest="$demo_repo_root/.local/cni/calico-v3.30.4.yaml"
  expected=1e0f605a1f0a337a198a6ed8512c4ade67a2d8ab239f0bb67062a663fed84e6d
  mkdir -p "$(dirname "$manifest")"
  if [[ ! -f "$manifest" ]]; then
    download="$(mktemp "${manifest}.XXXXXX")"
    trap 'rm -f "$download"' EXIT
    curl --fail --location --output "$download" \
      https://raw.githubusercontent.com/projectcalico/calico/v3.30.4/manifests/calico.yaml
    actual="$(shasum -a 256 "$download")"
    [[ "${actual%% *}" == "$expected" ]] || { echo 'CNI checksum mismatch' >&2; exit 1; }
    mv "$download" "$manifest"
    trap - EXIT
  fi
  actual="$(shasum -a 256 "$manifest")"
  [[ "${actual%% *}" == "$expected" ]] || { echo 'CNI checksum mismatch' >&2; exit 1; }
  demo_env_set SPROOZI_CNI_MANIFEST "$manifest"
  demo_env_set SPROOZI_CNI_SELECTOR k8s-app=calico-node
else
  : "${SPROOZI_CNI_MANIFEST:?Custom CNI requires a manifest path}"
  : "${SPROOZI_CNI_SELECTOR:?Custom CNI requires its readiness selector}"
  [[ -f "$SPROOZI_CNI_MANIFEST" ]] || { echo 'Configured CNI manifest does not exist' >&2; exit 1; }
fi
echo 'Kind networking configured; no networking prompts are needed'

[[ "${1:-}" != --network-only ]] || exit 0

if [[ -z "${SPROOZI_DEMO_REPOSITORY:-}" ]]; then
  owner="$(gh api user --jq .login)"
  demo_env_set SPROOZI_DEMO_REPOSITORY "$owner/sproozi-demo"
  echo 'Configured dedicated sproozi-demo repository; it must exist and contain the demo workload'
fi
if [[ -z "${CODEX_IMAGE:-}" ]]; then
  bash "$demo_repo_root/hack/demo/prepare-image.sh"
fi
echo 'Automatic demo configuration saved; existing credentials and overrides preserved'
