#!/usr/bin/env bash
set -euo pipefail

# Re-resolve every pinned client to one npm dist-tag or version (default:
# latest) and rewrite its package.json and package-lock.json. npm only reads
# registry metadata here; no package code runs. Prints `<client>=<version>`.
#
#   hack/clients/update.sh [latest|<dist-tag>]
#   hack/clients/update.sh --print     # print the current pins only
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
selector="${1:-latest}"
for entry in codex:@openai/codex claude-code:@anthropic-ai/claude-code opencode:opencode-ai; do
  dir="${entry%%:*}" package="${entry#*:}"
  if [[ "$selector" != --print ]]; then
    npm install --prefix "$root/$dir" --package-lock-only --ignore-scripts \
      --save-exact --no-audit --no-fund "$package@$selector" >/dev/null
  fi
  version="$(node -p 'require(process.argv[1]).dependencies[process.argv[2]]' \
    "$root/$dir/package.json" "$package")"
  printf '%s=%s\n' "$dir" "$version"
done
