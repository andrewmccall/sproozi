#!/usr/bin/env bash
# This helper requires only human browser consent; it never prints tokens.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$repo_root/hack/demo/env.sh"
demo_env_load
session_file="${CHATGPT_SESSION_FILE:-$repo_root/.local/chatgpt-session.json}"
cd "$repo_root"
login_args=(--output "$session_file" --host "$repo_root/.local/chatgpt-host.json")
if [[ "${1:-}" == --reauthorize ]]; then
  shift
elif [[ -f "$session_file" ]]; then
  login_args+=(--reuse)
fi
if (( $# )); then echo 'Usage: chatgpt-login.sh [--reauthorize]' >&2; exit 1; fi
go run ./cmd/chatgpt-login "${login_args[@]}"
demo_env_set CHATGPT_SESSION_FILE "$session_file"
demo_env_set MODEL_AUTH_MODE chatgpt
echo 'Demo gateway configured for ChatGPT plan usage; the existing API key was preserved'
