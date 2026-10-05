#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

env_file="${SPROOZI_DEMO_ENV_FILE:-$PWD/.local/demo.env}"
if [[ -f "$env_file" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$env_file"
  set +a
fi

export SPROOZI_DEMO_REPO="${SPROOZI_DEMO_REPO:-${SPROOZI_DEMO_REPOSITORY:-}}"
export SPROOZI_KUBECONFIG="${SPROOZI_KUBECONFIG:-${KUBECONFIG:-}}"
export SPROOZI_LIVE_NAMESPACE="${SPROOZI_LIVE_NAMESPACE:-sproozi-system}"
export SPROOZI_LIVE_TEMPLATE="${SPROOZI_LIVE_TEMPLATE:-sre-remediation}"
export SPROOZI_LIVE_BASE_BRANCH="${SPROOZI_LIVE_BASE_BRANCH:-main}"
export SPROOZI_LIVE_TASK="${SPROOZI_LIVE_TASK:-Investigate the failing demo workload and propose a safe correction.}"
export SPROOZI_LIVE_CAPABILITIES="${SPROOZI_LIVE_CAPABILITIES:-kubernetes.read,github.pull_request,model.inference}"

# Live acceptance creates a branch and pull request in a disposable external
# repository against an explicitly provisioned Kind cluster. Never silently
# downgrade this to a fixture or unit test.
repo="$SPROOZI_DEMO_REPO"
if [[ -z "$repo" ]]; then
  echo 'Set SPROOZI_DEMO_REPO=owner/disposable-repository for live acceptance' >&2
  exit 1
fi
if [[ "${SPROOZI_LIVE_ACCEPT:-0}" != 1 ]]; then
  echo 'Live acceptance is opt-in: set SPROOZI_LIVE_ACCEPT=1 (creates a real PR)' >&2
  exit 1
fi
for tool in git gh kubectl kind docker make; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "Missing required live tool: $tool" >&2
    exit 1
  }
done
kubeconfig="${SPROOZI_KUBECONFIG:-}"
if [[ -z "$kubeconfig" || ! -f "$kubeconfig" ]]; then
  echo 'Set SPROOZI_KUBECONFIG to an explicit kubeconfig for the live Kind cluster' >&2
  exit 1
fi
if [[ -z "${SPROOZI_CNI_SELECTOR:-}" ]]; then
  echo 'Set SPROOZI_CNI_SELECTOR to the ready enforcing-CNI agent selector' >&2
  exit 1
fi
if ! kubectl --kubeconfig "$kubeconfig" version --request-timeout=15s >/dev/null 2>&1; then
  echo 'The explicit SPROOZI_KUBECONFIG cannot reach a Kubernetes API' >&2
  exit 1
fi

if [[ ! -f test/acceptance/live_test.go ]]; then
  echo 'Missing test/acceptance/live_test.go; no live acceptance may be claimed' >&2
  exit 1
fi
if ! gh auth status >/dev/null 2>&1; then
  echo 'gh is not authenticated; refusing to run live acceptance' >&2
  exit 1
fi

echo "Live acceptance requested for $repo using kubeconfig $kubeconfig"
# The suite bounds the run and each API operation itself. Go's default 10m
# process alarm would bypass deferred cleanup before the 20m run deadline.
SPROOZI_DEMO_REPO="$repo" KUBECONFIG="$kubeconfig" go test -tags=live ./test/acceptance -count=1 -timeout=0 -v
