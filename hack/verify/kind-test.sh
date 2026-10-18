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
export KIND_TEST_SUITE_RAN="$test_dir/suite-ran"
touch "$test_dir/cni.yaml"

# Kind writes the kubeconfig named by its final --kubeconfig argument.
kind() { [[ "$1 $2" != 'create cluster' ]] || touch "${@: -1}"; }
docker() { :; }
go() { :; }
python3() { :; }
# Image builds run while the cluster starts; the suite must wait for the CNI.
make() {
  case "$1" in
    docker-build)
      [[ -z "${KIND_TEST_FAIL_BUILD:-}" ]] || { echo 'manager build exploded'; return 3; }
      ;;
    test-e2e-existing) test -f "$KIND_TEST_READY" && touch "$KIND_TEST_SUITE_RAN" ;;
    *) test -f "$KIND_TEST_READY" ;;
  esac
}
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
export -f kind docker go python3 make kubectl

SPROOZI_VERIFY_ID=readiness-regression SPROOZI_CNI_MANIFEST="$test_dir/cni.yaml" \
SPROOZI_CNI_SELECTOR=test-provider SPROOZI_KUBECONFIG="$test_dir/kubeconfig" \
KIND_NODE_IMAGE=test/node bash hack/verify/kind.sh
test -f "$KIND_TEST_CREATED"
test -f "$KIND_TEST_READY"
echo 'Kind readiness regression passed'

# A failed background image build stops the run and prints that job's log
# instead of starting the suite.
rm -f "$KIND_TEST_CREATED" "$KIND_TEST_READY" "$KIND_TEST_SUITE_RAN"
if KIND_TEST_FAIL_BUILD=1 SPROOZI_VERIFY_ID=build-failure SPROOZI_CNI_MANIFEST="$test_dir/cni.yaml" \
  SPROOZI_CNI_SELECTOR=test-provider SPROOZI_KUBECONFIG="$test_dir/kubeconfig" \
  KIND_NODE_IMAGE=test/node bash hack/verify/kind.sh >"$test_dir/failure.log" 2>&1; then
  echo 'kind.sh passed despite a failed image build' >&2
  exit 1
fi
grep -q 'Background job manager-image failed with exit status 3' "$test_dir/failure.log"
grep -q 'manager build exploded' "$test_dir/failure.log"
test ! -e "$KIND_TEST_SUITE_RAN"
echo 'Kind background job failure regression passed'

# --no-cni creates a private kubeconfig and never waits for CNI Pods.
rm -f "$KIND_TEST_CREATED" "$KIND_TEST_READY"
KIND_NODE_IMAGE=test/node bash hack/kind-up.sh no-cni-fixture "$test_dir/no-cni.kubeconfig" --no-cni
test "$(LC_ALL=C ls -l "$test_dir/no-cni.kubeconfig" | cut -c1-10)" = '-rw-------'
test ! -e "$KIND_TEST_CREATED"
echo 'Kind bootstrap without CNI passed'
