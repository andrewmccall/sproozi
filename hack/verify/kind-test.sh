#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# Exercise the real provisioning script, and through it hack/kind-up.sh, with
# delayed Pod creation. No cluster, provider API or image build is needed to
# catch the readiness ordering bug.
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
export KIND_TEST_CREATED="$test_dir/pods-created"
export KIND_TEST_READY="$test_dir/pods-ready"
touch "$test_dir/cni.yaml"

# Kind writes the kubeconfig named by its final --kubeconfig argument.
kind() { [[ "$1 $2" != 'create cluster' ]] || touch "${@: -1}"; }
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
echo 'Kind readiness regression passed'

# --no-cni creates a private kubeconfig and never waits for CNI Pods.
rm -f "$KIND_TEST_CREATED" "$KIND_TEST_READY"
KIND_NODE_IMAGE=test/node bash hack/kind-up.sh no-cni-fixture "$test_dir/no-cni.kubeconfig" --no-cni
test "$(LC_ALL=C ls -l "$test_dir/no-cni.kubeconfig" | cut -c1-10)" = '-rw-------'
test ! -e "$KIND_TEST_CREATED"
echo 'Kind bootstrap without CNI passed'
