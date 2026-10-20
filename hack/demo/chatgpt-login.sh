#!/usr/bin/env bash
# This helper requires only human browser consent; it never prints tokens.
#
#   bash hack/demo/chatgpt-login.sh [--reauthorize]
#
# Reuses the saved ChatGPT credential when one exists, with a warning;
# --reauthorize always opens a fresh browser sign-in and replaces it.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$repo_root/hack/demo/env.sh"
demo_env_load
session_file="${CHATGPT_SESSION_FILE:-$repo_root/.local/chatgpt-session.json}"
cd "$repo_root"
reuse=0
login_args=(--output "$session_file" --host "$repo_root/.local/chatgpt-host.json")
if [[ "${1:-}" == --reauthorize ]]; then
  shift
elif [[ -f "$session_file" ]]; then
  login_args+=(--reuse)
  reuse=1
fi
if (( $# )); then echo 'Usage: chatgpt-login.sh [--reauthorize]' >&2; exit 1; fi
# Each ChatGPT credential has one refresh owner (ADR 0026): a gateway that loaded this
# file rotates its tokens, so a second gateway must not load it too.
if (( reuse )); then
  echo "Warning: reusing the saved ChatGPT credential in $session_file. It must not be in use by another gateway, such as an existing demo cluster; if it is, sign in again with bash hack/demo/chatgpt-login.sh --reauthorize (or make demo-chatgpt-login CHATGPT_REAUTHORIZE=1) and load that credential instead" >&2
fi
go run ./cmd/chatgpt-login "${login_args[@]}"
demo_env_set CHATGPT_SESSION_FILE "$session_file"
demo_env_set MODEL_AUTH_MODE chatgpt
echo 'Demo gateway configured for ChatGPT plan usage; the existing API key was preserved'
