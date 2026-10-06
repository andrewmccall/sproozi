#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
env_file="${SPROOZI_DEMO_ENV_FILE:-$repo_root/.local/demo.env}"
if [[ -f "$env_file" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$env_file"
  set +a
fi

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

: "${SPROOZI_CNI_MANIFEST:?Set SPROOZI_CNI_MANIFEST to a pinned enforcing CNI manifest}"
[[ -f "$SPROOZI_CNI_MANIFEST" ]] || { echo "SPROOZI_CNI_MANIFEST does not exist: $SPROOZI_CNI_MANIFEST" >&2; exit 1; }
: "${SPROOZI_CNI_SELECTOR:?Set SPROOZI_CNI_SELECTOR to the CNI agent label selector}"
case "${MODEL_AUTH_MODE:-api_key}" in
  api_key) : "${OPENAI_API_KEY:?Set OPENAI_API_KEY in $env_file or the environment}" ;;
  chatgpt)
    : "${CHATGPT_SESSION_FILE:?Run make demo-chatgpt-login first}"
    (cd "$repo_root" && go run ./cmd/chatgpt-login --check --output "$CHATGPT_SESSION_FILE")
    ;;
  *) echo 'MODEL_AUTH_MODE must be api_key or chatgpt' >&2; exit 1 ;;
esac
: "${GITHUB_APP_ID:?Set GITHUB_APP_ID in $env_file or the environment}"
: "${GITHUB_APP_INSTALLATION_ID:?Set GITHUB_APP_INSTALLATION_ID in $env_file or the environment}"
: "${GITHUB_APP_PRIVATE_KEY_FILE:?Set GITHUB_APP_PRIVATE_KEY_FILE in $env_file or the environment}"
[[ -f "$GITHUB_APP_PRIVATE_KEY_FILE" ]] || { echo "GitHub App private key does not exist: $GITHUB_APP_PRIVATE_KEY_FILE" >&2; exit 1; }
: "${SPROOZI_DEMO_REPOSITORY:?Set SPROOZI_DEMO_REPOSITORY to owner/name}"
: "${CODEX_IMAGE:?Set CODEX_IMAGE to a digest-pinned Codex image}"

[[ "$GITHUB_APP_ID" =~ ^[1-9][0-9]*$ ]] || { echo "GITHUB_APP_ID must be a positive integer" >&2; exit 1; }
[[ "$GITHUB_APP_INSTALLATION_ID" =~ ^[1-9][0-9]*$ ]] || { echo "GITHUB_APP_INSTALLATION_ID must be a positive integer" >&2; exit 1; }
[[ "$SPROOZI_DEMO_REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { echo "SPROOZI_DEMO_REPOSITORY must be owner/name" >&2; exit 1; }
[[ "$CODEX_IMAGE" =~ ^[A-Za-z0-9./_:-]+@sha256:[a-f0-9]{64}$ ]] || { echo "CODEX_IMAGE must be repository@sha256:<64 lowercase hex characters>" >&2; exit 1; }

echo "Demo prerequisites and private configuration are available"
