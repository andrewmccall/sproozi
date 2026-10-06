#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
destination="${1:?usage: install-gh.sh <destination-directory>}"
version="$(awk -F= '/^ARG GH_VERSION=/ { print $2; exit }' "$root/hack/demo/codex.Dockerfile")"
case "$version-$(uname -s)-$(uname -m)" in
  2.97.0-Linux-x86_64)
    platform=linux_amd64; extension=tar.gz
    checksum=a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112 ;;
  2.97.0-Linux-aarch64|2.97.0-Linux-arm64)
    platform=linux_arm64; extension=tar.gz
    checksum=73ea440ecad9c9e284429997ee6f93577bc6f7bc6fba357ef62c53ad8fb641a5 ;;
  2.97.0-Darwin-x86_64)
    platform=macOS_amd64; extension=zip
    checksum=63298c998cc2a924c9e254c6af6a1caad6ece281122687a91f079bc0a462700e ;;
  2.97.0-Darwin-arm64)
    platform=macOS_arm64; extension=zip
    checksum=a58b8fd77b417a38f47a0b54d1370c59b0fcdb324ccc9ca002b0998f7c4c999e ;;
  *) echo "No pinned gh checksum for version $version on $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac

if [[ -x "$destination/gh" ]] && "$destination/gh" --version | head -1 | grep -q "^gh version $version "; then
  exit 0
fi
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
archive="gh_${version}_${platform}.$extension"
curl --fail --location --silent --show-error \
  "https://github.com/cli/cli/releases/download/v${version}/$archive" -o "$temporary/$archive"
python3 - "$temporary/$archive" "$checksum" <<'PY'
import hashlib
import pathlib
import sys

if hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest() != sys.argv[2]:
    raise SystemExit("Pinned gh archive checksum mismatch")
PY
if [[ "$extension" == zip ]]; then
  unzip -q "$temporary/$archive" -d "$temporary"
else
  tar -xzf "$temporary/$archive" -C "$temporary"
fi
mkdir -p "$destination"
install -m 0755 "$temporary/gh_${version}_${platform}/bin/gh" "$destination/gh"
echo "Installed pinned gh $version in $destination"
