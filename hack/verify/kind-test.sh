#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# Exercise the real provisioning script with delayed Pod creation. No cluster,
# provider API or image build is needed to catch the readiness ordering bug.
test_dir="$(mktemp -d)"
trap 'rm -f "$test_dir/pods-created" "$test_dir/pods-ready" "$test_dir/cni.yaml"; rmdir "$test_dir"' EXIT
export KIND_TEST_CREATED="$test_dir/pods-created"
export KIND_TEST_READY="$test_dir/pods-ready"
touch "$test_dir/cni.yaml"

kind() { :; }
docker() { :; }
make() { test -f "$KIND_TEST_READY"; }
kubectl() {
  case "$1" in
    config|apply) return 0 ;;
    wait)
      case " $* " in
        *' --for=create '*) touch "$KIND_TEST_CREATED" ;;
        *' --for=condition=Ready '*)
          if [[ ! -f "$KIND_TEST_CREATED" ]]; then
            echo 'error: no matching resources found' >&2
            return 1
          fi
          touch "$KIND_TEST_READY"
          ;;
        *) return 1 ;;
      esac
      ;;
    *) return 1 ;;
  esac
}
export -f kind docker make kubectl

SPROOZI_VERIFY_ID=readiness-regression SPROOZI_CNI_MANIFEST="$test_dir/cni.yaml" \
SPROOZI_CNI_SELECTOR=test-provider SPROOZI_KUBECONFIG="$test_dir/kubeconfig" \
KIND_NODE_IMAGE=test/node bash hack/verify/kind.sh
test -f "$KIND_TEST_CREATED"
test -f "$KIND_TEST_READY"
rm -f "$KIND_TEST_CREATED" "$KIND_TEST_READY" "$test_dir/cni.yaml"
echo 'Kind readiness regression passed'
