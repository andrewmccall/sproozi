#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -f "$test_dir/default.env" "$test_dir/custom.env" "$test_dir/custom.yaml" "$test_dir/registry-connected" "$test_dir/node-1-configured" "$test_dir/node-2-configured"; rmdir "$test_dir"' EXIT

# No Docker or provider access: exercise the real automatic networking path.
unset SPROOZI_CNI_MANIFEST SPROOZI_CNI_SELECTOR
export SPROOZI_DEMO_ENV_FILE="$test_dir/default.env"
bash "$repo_root/hack/demo/setup.sh" --network-only
source "$repo_root/hack/demo/env.sh"
demo_env_load
test -f "$SPROOZI_CNI_MANIFEST"
test "$SPROOZI_CNI_SELECTOR" = k8s-app=calico-node
test "$(LC_ALL=C ls -ld "$demo_env_file" | cut -c1-10)" = '-rw-------'

# Literal secrets, shell-sensitive paths and unrelated settings survive upserts.
fixture_secret='fixture-only-key with spaces $dollar `backticks`'
demo_env_set OPENAI_API_KEY "$fixture_secret"
demo_env_set UNRELATED_VALUE 'keep this'
demo_env_set CODEX_IMAGE "localhost:5001/sproozi-codex@sha256:$(printf '%064d' 0)"
before="$(shasum -a 256 "$demo_env_file")"
bash "$repo_root/hack/demo/setup.sh" --network-only
test "$(shasum -a 256 "$demo_env_file")" = "$before"
unset OPENAI_API_KEY UNRELATED_VALUE
demo_env_load
test "$OPENAI_API_KEY" = "$fixture_secret"
test "$UNRELATED_VALUE" = 'keep this'

# A caller-selected provider is preserved; Calico is only the default.
touch "$test_dir/custom.yaml"
export SPROOZI_DEMO_ENV_FILE="$test_dir/custom.env"
demo_env_load
demo_env_set SPROOZI_CNI_MANIFEST "$test_dir/custom.yaml"
demo_env_set SPROOZI_CNI_SELECTOR 'app=another-cni'
before="$(shasum -a 256 "$demo_env_file")"
bash "$repo_root/hack/demo/setup.sh" --network-only
test "$(shasum -a 256 "$demo_env_file")" = "$before"

python3 - "$repo_root" <<'PY'
import runpy
import sys
from pathlib import Path

pattern = runpy.run_path(str(Path(sys.argv[1]) / "hack/demo/render.py"))["IMAGE_PATTERN"]
assert pattern.fullmatch("localhost:5001/sproozi-codex@sha256:" + "a" * 64)
assert pattern.fullmatch("ghcr.io/example/codex@sha256:" + "b" * 64)
assert not pattern.fullmatch("localhost:5001/sproozi-codex:latest")
print("Local registry digest validation passed")
PY
echo 'Automatic setup and credential-preservation regressions passed'

# Ensure the demo configures every node rather than relying on archive import
# preserving digest references (the real Kind image check covers that failure).
export SPROOZI_DEMO_ENV_FILE="$test_dir/default.env"
export DEMO_SETUP_TEST_DIR="$test_dir"
kind() {
  [[ "$*" == 'get nodes --name isolated-fixture' ]] || return 1
  printf '%s\n' node-1 node-2
}
docker() {
  case "$1" in
    start) [[ "$2" == sproozi-demo-registry ]] ;;
    inspect) printf 'null\n' ;;
    network) [[ "$*" == 'network connect kind sproozi-demo-registry' ]] && touch "$DEMO_SETUP_TEST_DIR/registry-connected" ;;
    exec) [[ "$3 $4 $5" == 'mkdir -p /etc/containerd/certs.d/localhost:5001' ]] ;;
    cp)
      test -f "$2"
      case "$3" in
        node-1:/etc/containerd/certs.d/localhost:5001/hosts.toml) touch "$DEMO_SETUP_TEST_DIR/node-1-configured" ;;
        node-2:/etc/containerd/certs.d/localhost:5001/hosts.toml) touch "$DEMO_SETUP_TEST_DIR/node-2-configured" ;;
        *) return 1 ;;
      esac ;;
    *) return 1 ;;
  esac
}
export -f kind docker
bash "$repo_root/hack/demo/load-image.sh" isolated-fixture
test -f "$test_dir/registry-connected"
test -f "$test_dir/node-1-configured"
test -f "$test_dir/node-2-configured"
echo 'Kind registry wiring regression passed'
