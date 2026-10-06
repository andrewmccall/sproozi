#!/usr/bin/env bash
set -euo pipefail

# Download the exact release binary used by CI and verify it before putting it
# on PATH. The checksums are copied from the matching official release assets.
version="${KIND_VERSION:-v0.29.0}"
destination="${1:?usage: install-kind.sh DESTINATION_DIR}"
arch="$(uname -m)"
case "$arch" in
  x86_64) asset_arch=amd64; expected="${KIND_LINUX_AMD64_SHA256:-c72eda46430f065fb45c5f70e7c957cc9209402ef309294821978677c8fb3284}" ;;
  aarch64|arm64) asset_arch=arm64; expected="${KIND_LINUX_ARM64_SHA256:-03d45095dbd9cc1689f179a3e5e5da24b77c2d1b257d7645abf1b4174bebcf2a}" ;;
  *) echo "Unsupported Linux architecture: $arch" >&2; exit 1 ;;
esac

mkdir -p "$destination"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
asset="kind-linux-${asset_arch}"
url="https://github.com/kubernetes-sigs/kind/releases/download/${version}/${asset}"
curl --fail --silent --show-error --location --retry 3 --retry-all-errors "$url" --output "$tmp/$asset"
printf '%s  %s\n' "$expected" "$tmp/$asset" | sha256sum --check --status
install -m 0755 "$tmp/$asset" "$destination/kind"
"$destination/kind" version
