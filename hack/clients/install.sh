#!/bin/sh
# Install pinned agent clients inside a client image build:
#
#   COPY hack/clients/ /opt/sproozi-clients/
#   RUN sh /opt/sproozi-clients/install.sh codex claude-code opencode
#
# Each client installs from its committed package-lock.json, so npm verifies
# every package against its recorded integrity hash. No package lifecycle
# script runs. Claude Code and OpenCode ship a postinstall that only moves the
# platform binary from an optional dependency over a placeholder; this script
# does that step itself. See hack/clients/README.md.
set -eu
root="$(cd "$(dirname "$0")" && pwd)"
case "$(node -p process.arch)" in
  x64) arch=x64 ;;
  arm64) arch=arm64 ;;
  *) echo "Unsupported client architecture: $(node -p process.arch)" >&2; exit 1 ;;
esac

for client in "$@"; do
  dir="$root/$client"
  npm ci --prefix "$dir" --ignore-scripts --no-audit --no-fund
  modules="$dir/node_modules"
  case "$client" in
    codex)
      # bin/codex.js selects the platform binary at run time; no postinstall.
      name=codex ;;
    claude-code)
      # Equivalent of install.cjs for glibc Linux.
      name=claude
      ln -f "$modules/@anthropic-ai/claude-code-linux-$arch/claude" \
        "$modules/@anthropic-ai/claude-code/bin/claude.exe" ;;
    opencode)
      # Equivalent of postinstall.mjs for glibc Linux. The AVX2 build is used
      # on x64 so the image does not depend on the build host's CPU.
      name=opencode
      ln -f "$modules/opencode-linux-$arch/bin/opencode" \
        "$modules/opencode-ai/bin/opencode.exe" ;;
    *) echo "Unknown client: $client" >&2; exit 1 ;;
  esac
  target="$(readlink -f "$modules/.bin/$name")"
  test -f "$target"
  chmod 0755 "$target"
  ln -sf "$modules/.bin/$name" "/usr/local/bin/$name"
done
npm cache clean --force
