#!/usr/bin/env bash
# Shared local configuration helpers. Never print configuration values.

demo_env_load() {
  demo_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  demo_env_file="${SPROOZI_DEMO_ENV_FILE:-$demo_repo_root/.local/demo.env}"
  if [[ -L "$demo_env_file" ]]; then
    echo 'Refusing to write demo configuration through a symlink' >&2
    return 1
  fi
  if [[ -f "$demo_env_file" ]]; then
    set -a
    # shellcheck disable=SC1090
    source "$demo_env_file"
    set +a
  fi
}

demo_env_set() {
  local key="$1" value="$2" temporary
  umask 077
  mkdir -p "$(dirname "$demo_env_file")"
  temporary="$(mktemp "${demo_env_file}.XXXXXX")"
  if [[ -f "$demo_env_file" ]]; then
    awk -v key="$key" 'index($0, key "=") != 1 { print }' "$demo_env_file" >"$temporary"
  fi
  printf '%s=%q\n' "$key" "$value" >>"$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$demo_env_file"
  printf -v "$key" '%s' "$value"
  export "$key"
}
