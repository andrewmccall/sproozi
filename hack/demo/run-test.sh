#!/usr/bin/env bash
# Offline regressions for hack/demo/run.sh: cheap inputs and missing
# credentials fail before a cluster exists, the caller's environment wins over
# the demo environment file, a cluster this run created is deleted after a
# setup failure or an INT, TERM or HUP signal, a submitted run keeps its
# cluster, a pre-existing cluster is never deleted, the demo environment file
# is validated before it is sourced, and a second run with the same cluster
# name fails without touching the first. Kind, Docker, make,
# kubectl, the prepare and live scripts and the renderer are shell-function
# fakes; nothing leaves the host.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
export RUN_TEST_LOG="$test_dir/calls.log"
printf 'kind: DaemonSet\n' >"$test_dir/cni.yaml"
printf '# comment only\n\n' >"$test_dir/empty-cni.yaml"

kind() {
  echo "kind $*" >>"$RUN_TEST_LOG"
  case "$1 ${2:-}" in
    'get clusters') [[ -z "${RUN_TEST_EXISTING:-}" ]] || echo "$RUN_TEST_EXISTING" ;;
    'create cluster')
      [[ -z "${RUN_TEST_CREATE_FAILS:-}" ]] || { echo 'ERROR: node(s) already exist for a cluster with the name' >&2; return 1; }
      touch "${@: -1}" ;;
  esac
}
docker() { echo "docker $*" >>"$RUN_TEST_LOG"; [[ "$1" != pull || -z "${RUN_TEST_PULL_FAILS:-}" ]]; }
# RUN_TEST_SIGNAL_AT=<step> sends RUN_TEST_SIGNAL to run.sh itself (the fakes
# run in its shell, so $$ is run.sh) as if the user pressed Ctrl-C, closed the
# terminal or ran kill.
signal_at() {
  [[ "${RUN_TEST_SIGNAL_AT:-}" != "$1" ]] || kill -s "$RUN_TEST_SIGNAL" $$
}
make() {
  echo "make $*" >>"$RUN_TEST_LOG"
  signal_at "make-$1"
  # RUN_TEST_HOLD_AT=<step> blocks until RUN_TEST_RELEASE exists, so a second
  # run can start while this one holds the cluster name.
  if [[ "${RUN_TEST_HOLD_AT:-}" == "make-$1" ]]; then
    touch "$RUN_TEST_HELD"
    while [[ ! -e "$RUN_TEST_RELEASE" ]]; do sleep 0.1; done
  fi
  [[ "$1" != install || -n "${RUN_TEST_INSTALL_OK:-}" ]]
}
kubectl() { echo "kubectl $*" >>"$RUN_TEST_LOG"; }
bash() {
  case "$1" in
    hack/demo/prepare.sh|hack/verify/live.sh) echo "bash $*" >>"$RUN_TEST_LOG"; signal_at "$1" ;;
    *) command bash "$@" ;;
  esac
}
python3() { echo "python3 $*" >>"$RUN_TEST_LOG"; }
export -f kind docker make kubectl bash python3 signal_at

run_demo() {
  : >"$RUN_TEST_LOG"
  env SPROOZI_DEMO_ENV_FILE="$test_dir/none.env" SPROOZI_DEMO_CHECKED=1 MAKE=make KIND=kind KUBECTL=kubectl CONTAINER_TOOL=docker \
    KIND_NODE_IMAGE=test/node IMG=controller:fixture CODEX_IMAGE="example/codex@sha256:$(printf '%064d' 0)" \
    SPROOZI_DEMO_REPOSITORY=owner/name SPROOZI_CNI_MANIFEST="$test_dir/cni.yaml" SPROOZI_CNI_SELECTOR=k8s-app=fixture \
    "$@" /bin/bash "$repo_root/hack/demo/run.sh" fix-run-test >"$test_dir/out.log" 2>&1
}
expect_failure_without_cluster() {
  if run_demo "$@"; then echo "run.sh passed with $*" >&2; exit 1; fi
  if grep -q 'kind create\|kind delete' "$RUN_TEST_LOG"; then
    echo "run.sh touched a cluster with $*" >&2; cat "$RUN_TEST_LOG" >&2; exit 1
  fi
}

expect_failure_without_cluster SPROOZI_CNI_MANIFEST="$test_dir/empty-cni.yaml"
grep -q 'SPROOZI_CNI_MANIFEST has no content' "$test_dir/out.log"
expect_failure_without_cluster IMG=controller:latest
expect_failure_without_cluster IMG=
grep -q 'IMG is empty or unset' "$test_dir/out.log"
expect_failure_without_cluster 'IMG=controller:a b'
grep -q 'IMG must be an image reference without spaces' "$test_dir/out.log"
expect_failure_without_cluster RUN_TEST_PULL_FAILS=1
expect_failure_without_cluster RUN_TEST_EXISTING=fix-run-test
grep -q 'Refusing to reuse existing cluster' "$test_dir/out.log"
# A direct run checks credentials before pulling, building or creating anything.
expect_failure_without_cluster SPROOZI_DEMO_CHECKED=0 OPENAI_API_KEY= GITHUB_APP_ID=
grep -q 'GITHUB_APP_ID' "$test_dir/out.log"
if grep -q '^docker\|^make' "$RUN_TEST_LOG"; then echo 'run.sh built before checking credentials' >&2; exit 1; fi
if env KIND_CLUSTER= /bin/bash "$repo_root/hack/kind-up.sh" '' "$test_dir/k" --no-cni 2>/dev/null; then
  echo 'kind-up.sh accepted an empty cluster name' >&2; exit 1
fi
echo 'Demo input validation before cluster creation passed'

# The demo environment file is checked before anything sources it: a value
# that would run a command, or a variable bash or a tool would act on, fails
# before any build or cluster, and the command never runs.
lock="$repo_root/.local/demo/fix-run-test.lock"
marker="$test_dir/marker"
printf '#!/bin/sh\ntouch %s\n' "$marker" >"$test_dir/hook.sh"
for line in "IMG=\$(touch $marker)" "IMG=\`touch $marker\`" "CODEX_IMAGE=x; touch $marker" "BASH_ENV=$test_dir/hook.sh" \
    "export PROMPT_COMMAND=x" "PATH=$test_dir" "export IFS"; do
  printf '%s\n' "$line" >"$test_dir/bad.env"
  expect_failure_without_cluster SPROOZI_DEMO_ENV_FILE="$test_dir/bad.env"
  grep -q 'bad.env line 1 ' "$test_dir/out.log" || { echo "run.sh did not name the bad line for: $line" >&2; cat "$test_dir/out.log" >&2; exit 1; }
  if grep -q 'touch' "$test_dir/out.log"; then echo 'run.sh echoed an env file value' >&2; exit 1; fi
  test ! -e "$marker" || { echo "run.sh ran a command from the env file: $line" >&2; exit 1; }
  if grep -q '^docker\|^make' "$RUN_TEST_LOG"; then echo 'run.sh built before checking the env file' >&2; exit 1; fi
  for script in prepare.sh check.sh; do
    if env SPROOZI_DEMO_ENV_FILE="$test_dir/bad.env" /bin/bash "$repo_root/hack/demo/$script" >/dev/null 2>"$test_dir/script.log"; then
      echo "$script accepted: $line" >&2; exit 1
    fi
    grep -q 'bad.env line 1 ' "$test_dir/script.log" || { echo "$script did not name the bad line" >&2; cat "$test_dir/script.log" >&2; exit 1; }
    test ! -e "$marker" || { echo "$script ran a command from the env file: $line" >&2; exit 1; }
  done
done
grep -q 'sets IFS, which the demo refuses' "$test_dir/script.log"
test ! -e "$lock"
echo 'Demo environment file validation passed'

# make install fails after the cluster exists: the run deletes only its own cluster.
if run_demo; then echo 'run.sh passed despite a failed install' >&2; exit 1; fi
grep -q '^kind create cluster --name fix-run-test' "$RUN_TEST_LOG"
grep -q '^kind delete cluster --name fix-run-test' "$RUN_TEST_LOG"
test ! -e "$repo_root/.local/demo/fix-run-test.kubeconfig"
if run_demo SPROOZI_PRESERVE_ON_FAILURE=1; then echo 'run.sh passed despite a failed install' >&2; exit 1; fi
if grep -q '^kind delete' "$RUN_TEST_LOG"; then echo 'run.sh deleted a preserved cluster' >&2; exit 1; fi
rm -f "$repo_root/.local/demo/fix-run-test.kubeconfig"
test ! -e "$lock" || { echo 'run.sh left its cluster lock behind' >&2; exit 1; }
# A create that fails, for example because another run took the name between
# the existence check and the create, must not delete that cluster.
if run_demo RUN_TEST_CREATE_FAILS=1; then echo 'run.sh passed despite a failed create' >&2; exit 1; fi
if grep -q '^kind delete' "$RUN_TEST_LOG"; then echo 'run.sh deleted a cluster it did not create' >&2; exit 1; fi
echo 'Demo failure cleanup passed'

# Two runs with the same cluster name: the second fails early and leaves the
# first run's lock, cluster and kubeconfig alone.
export RUN_TEST_HELD="$test_dir/held" RUN_TEST_RELEASE="$test_dir/release"
first_log="$test_dir/first.log"
RUN_TEST_LOG="$first_log" run_demo RUN_TEST_HOLD_AT=make-install RUN_TEST_INSTALL_OK=1 &
first=$!
for _ in $(seq 100); do [[ -e "$RUN_TEST_HELD" ]] && break; sleep 0.1; done
test -e "$RUN_TEST_HELD" || { echo 'first demo run never reached make install' >&2; kill "$first"; exit 1; }
second_out="$test_dir/second.out"
: >"$test_dir/second.log"
if env RUN_TEST_LOG="$test_dir/second.log" SPROOZI_DEMO_ENV_FILE="$test_dir/none.env" SPROOZI_DEMO_CHECKED=1 MAKE=make KIND=kind \
    KUBECTL=kubectl CONTAINER_TOOL=docker KIND_NODE_IMAGE=test/node IMG=controller:fixture \
    CODEX_IMAGE="example/codex@sha256:$(printf '%064d' 0)" SPROOZI_DEMO_REPOSITORY=owner/name \
    SPROOZI_CNI_MANIFEST="$test_dir/cni.yaml" SPROOZI_CNI_SELECTOR=k8s-app=fixture \
    /bin/bash "$repo_root/hack/demo/run.sh" fix-run-test >"$second_out" 2>&1; then
  echo 'a second demo run with the same cluster name passed' >&2; touch "$RUN_TEST_RELEASE"; exit 1
fi
grep -q 'Another demo run is using cluster fix-run-test' "$second_out" || { cat "$second_out" >&2; touch "$RUN_TEST_RELEASE"; exit 1; }
if grep -q '^kind \(create\|delete\)\|^docker\|^make' "$test_dir/second.log"; then
  echo 'the second demo run built or touched a cluster' >&2; touch "$RUN_TEST_RELEASE"; exit 1
fi
test -d "$lock" && test -e "$repo_root/.local/demo/fix-run-test.kubeconfig" || {
  echo "the second demo run removed the first run's lock or kubeconfig" >&2; touch "$RUN_TEST_RELEASE"; exit 1
}
touch "$RUN_TEST_RELEASE"
wait "$first" || true
if grep -q '^kind delete' "$first_log"; then echo 'the first demo run deleted its own submitted cluster' >&2; exit 1; fi
test ! -e "$lock" || { echo 'the first demo run left its lock behind' >&2; exit 1; }
rm -f "$repo_root/.local/demo/fix-run-test.kubeconfig" "$RUN_TEST_HELD" "$RUN_TEST_RELEASE"
unset RUN_TEST_HELD RUN_TEST_RELEASE
echo 'Concurrent demo runs with one cluster name passed'

# The command line and environment win over the demo environment file.
printf 'IMG=controller:from-file\nSPROOZI_CNI_SELECTOR=k8s-app=from-file\n' >"$test_dir/demo.env"
run_demo SPROOZI_DEMO_ENV_FILE="$test_dir/demo.env" RUN_TEST_SIGNAL_AT=make-docker-build RUN_TEST_SIGNAL=TERM || true
grep -q '^make docker-build IMG=controller:fixture$' "$RUN_TEST_LOG" || { echo 'demo.env overrode IMG from the caller' >&2; cat "$RUN_TEST_LOG" >&2; exit 1; }
run_demo SPROOZI_DEMO_ENV_FILE="$test_dir/demo.env" IMG= RUN_TEST_SIGNAL_AT=make-docker-build RUN_TEST_SIGNAL=TERM || true
grep -q '^make docker-build IMG=controller:from-file$' "$RUN_TEST_LOG" || { echo 'demo.env IMG was not used when the caller left it empty' >&2; exit 1; }
echo 'Demo environment precedence passed'

# Signals: bash skips EXIT traps for fatal signals, so each is trapped. Before
# submission the run deletes its own cluster and exits 128+signal.
expect_signal() {
  local signal="$1" step="$2" status="$3"; shift 3
  local actual=0
  run_demo RUN_TEST_SIGNAL="$signal" RUN_TEST_SIGNAL_AT="$step" "$@" || actual=$?
  [[ "$actual" == "$status" ]] || { echo "run.sh exited $actual, not $status, for $signal at $step" >&2; cat "$test_dir/out.log" >&2; exit 1; }
  grep -q 'Demo interrupted by a signal' "$test_dir/out.log"
}
for case in INT:130 TERM:143 HUP:129; do
  expect_signal "${case%%:*}" make-install "${case#*:}"
  grep -q '^kind delete cluster --name fix-run-test' "$RUN_TEST_LOG"
  test ! -e "$repo_root/.local/demo/fix-run-test.kubeconfig"
done
# Before the cluster exists there is nothing to delete.
expect_signal TERM make-docker-build 143
if grep -q '^kind create\|^kind delete' "$RUN_TEST_LOG"; then echo 'run.sh touched a cluster after an early signal' >&2; exit 1; fi
# Once the run is submitted the cluster is kept for inspection.
expect_signal TERM hack/verify/live.sh 143 RUN_TEST_INSTALL_OK=1
if grep -q '^kind delete' "$RUN_TEST_LOG"; then echo 'run.sh deleted a submitted cluster' >&2; exit 1; fi
grep -q 'Kept demo cluster fix-run-test' "$test_dir/out.log"
grep -q '^bash hack/demo/prepare.sh' "$RUN_TEST_LOG"
rm -f "$repo_root/.local/demo/fix-run-test.kubeconfig"
echo 'Demo signal cleanup passed'
