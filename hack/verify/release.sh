#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# Release verification is an aggregator, not a best-effort smoke test. Every
# gate is required and a missing live/Kind prerequisite is a failure.
python3 hack/verify/release.py
make verify-fast
make verify-api
make verify-protocol
make verify-kind
make verify-live SPROOZI_DEMO_REPO="${SPROOZI_DEMO_REPO:-}"
make verify-docs
make verify-public-tree
make verify-assertions
test "${RELEASE_BUILD:-0}" = 1 || {
  echo 'Set RELEASE_BUILD=1 to execute both Docker release builds; static checks alone are not a release gate' >&2
  exit 1
}
container_tool="${CONTAINER_TOOL:-docker}"
command -v "$container_tool" >/dev/null 2>&1 || {
  echo "Missing required tool: $container_tool" >&2
  exit 1
}
"$container_tool" build --pull=false --progress=plain -t "${IMG:-controller:release}" .
echo 'Release verification passed; no publication or push was performed. Verify the selected Codex image separately.'
