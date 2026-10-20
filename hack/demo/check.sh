#!/usr/bin/env bash
# Check the demo prerequisites without printing configuration values.
#
#   bash hack/demo/check.sh [--credentials]
#
# --credentials checks only what setup cannot supply (tools, model and GitHub
# App credentials, gh sign-in), so `make demo-check` reports missing
# credentials before setup builds images or starts the registry. Without it,
# every setting, including the CNI manifest, repository and Codex image, is
# checked.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# demo_env_load checks that every line is a plain assignment to an allowed
# name before it sources the file; see hack/demo/env.sh. Non-empty values from
# the environment win over the file, as they do for hack/demo/run.sh.
source "$repo_root/hack/demo/env.sh"
demo_env_load
env_file="$demo_env_file"
credentials_only=0
[[ "${1:-}" != --credentials ]] || credentials_only=1

# Local configuration first: these fail in milliseconds. Collect every missing
# setting so one run reports them all.
missing=()
require() { [[ -n "${!1:-}" ]] || missing+=("  $1${2:+ ($2)}"); }
case "${MODEL_AUTH_MODE:-api_key}" in
  api_key) require OPENAI_API_KEY ;;
  chatgpt) require CHATGPT_SESSION_FILE "run make demo-chatgpt-login first" ;;
  *) echo "MODEL_AUTH_MODE must be api_key or chatgpt (found ${MODEL_AUTH_MODE})" >&2; exit 1 ;;
esac
require GITHUB_APP_ID
require GITHUB_APP_INSTALLATION_ID
require GITHUB_APP_PRIVATE_KEY_FILE
if (( ! credentials_only )); then
  require SPROOZI_CNI_MANIFEST "run make demo-setup, or set a pinned enforcing CNI manifest"
  require SPROOZI_CNI_SELECTOR "run make demo-setup, or set the CNI agent label selector"
  require SPROOZI_DEMO_REPOSITORY "set it to owner/name"
  require CODEX_IMAGE "run make demo-setup, or set a digest-pinned Codex image"
fi
if (( ${#missing[@]} )); then
  printf 'Set these %d demo settings in %s or the environment:\n' "${#missing[@]}" "$env_file" >&2
  printf '%s\n' "${missing[@]}" >&2
  exit 1
fi
[[ -f "$GITHUB_APP_PRIVATE_KEY_FILE" && -r "$GITHUB_APP_PRIVATE_KEY_FILE" ]] || { echo "GitHub App private key does not exist or is not readable: $GITHUB_APP_PRIVATE_KEY_FILE" >&2; exit 1; }
[[ "$GITHUB_APP_ID" =~ ^[1-9][0-9]*$ ]] || { echo "GITHUB_APP_ID must be a positive integer" >&2; exit 1; }
[[ "$GITHUB_APP_INSTALLATION_ID" =~ ^[1-9][0-9]*$ ]] || { echo "GITHUB_APP_INSTALLATION_ID must be a positive integer" >&2; exit 1; }

container_tool="${CONTAINER_TOOL:-docker}"
kind_bin="${KIND:-kind}"
kubectl_bin="${KUBECTL:-kubectl}"
for tool in "$container_tool" "$kind_bin" "$kubectl_bin" git gh go make openssl python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "Missing required tool: $tool" >&2; exit 1; }
done
"$container_tool" info >/dev/null 2>&1 || { echo "Container engine is unavailable; start it before running the demo" >&2; exit 1; }
"$kind_bin" version >/dev/null 2>&1 || { echo "Kind is unavailable or unusable" >&2; exit 1; }
"$kubectl_bin" version --client >/dev/null 2>&1 || { echo "kubectl client is unavailable" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "Authenticate gh so the demo can verify the real PR" >&2; exit 1; }
if [[ "${MODEL_AUTH_MODE:-api_key}" == chatgpt ]]; then
  (cd "$repo_root" && go run ./cmd/chatgpt-login --check --output "$CHATGPT_SESSION_FILE")
fi

if (( credentials_only )); then
  echo "Demo tools and credentials are available"
  exit 0
fi

[[ -f "$SPROOZI_CNI_MANIFEST" ]] || { echo "SPROOZI_CNI_MANIFEST does not exist: $SPROOZI_CNI_MANIFEST" >&2; exit 1; }
# An empty manifest would only fail after the cluster exists.
grep -Eqv '^[[:space:]]*(#.*)?$' "$SPROOZI_CNI_MANIFEST" || { echo "SPROOZI_CNI_MANIFEST has no content: $SPROOZI_CNI_MANIFEST" >&2; exit 1; }
[[ "$SPROOZI_DEMO_REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { echo "SPROOZI_DEMO_REPOSITORY must be owner/name" >&2; exit 1; }
[[ "$CODEX_IMAGE" =~ ^[A-Za-z0-9./_:-]+@sha256:[a-f0-9]{64}$ ]] || { echo "CODEX_IMAGE must be repository@sha256:<64 lowercase hex characters>" >&2; exit 1; }

echo "Demo prerequisites and private configuration are available"
