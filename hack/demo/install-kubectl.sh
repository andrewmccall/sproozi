#!/usr/bin/env bash
# Download a checksum-verified kubectl into DESTINATION for the Kind demo, whose
# pinned Kubernetes 1.33.1 supports kubectl 1.32 to 1.34.
#
#   bash hack/demo/install-kubectl.sh VERSION DESTINATION
set -euo pipefail
[[ $# -eq 2 ]] || { echo "usage: $0 VERSION DESTINATION" >&2; exit 2; }
version="$1" destination="$2"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch=amd64
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; esac
url="https://dl.k8s.io/release/$version/bin/$os/$arch/kubectl"
mkdir -p "$(dirname "$destination")"
temporary="$(mktemp "$destination.XXXXXX")"
trap 'rm -f "$temporary"' EXIT
curl --fail --location --silent --show-error "$url" -o "$temporary"
expected="$(curl --fail --location --silent --show-error "$url.sha256")"
printf '%s  %s\n' "$expected" "$temporary" | shasum -a 256 -c - >/dev/null
chmod +x "$temporary"
mv "$temporary" "$destination"
trap - EXIT
"$destination" version --client
