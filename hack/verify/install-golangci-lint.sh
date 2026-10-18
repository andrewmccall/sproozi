#!/usr/bin/env bash
set -euo pipefail

# Download the official golangci-lint release archive and verify it before
# installing the binary. Compiling from source takes over a minute on a CI
# runner, and the setup-go cache never holds golangci-lint's own dependencies.
# The checksums are copied from the release's golangci-lint-*-checksums.txt.
#
#   hack/verify/install-golangci-lint.sh DESTINATION_FILE
version="${GOLANGCI_LINT_VERSION:-v2.12.2}"
destination="${1:?usage: install-golangci-lint.sh DESTINATION_FILE}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

case "${version}-${os}-${arch}" in
  v2.12.2-linux-amd64) expected=8df580d2670fed8fa984aac0507099af8df275e665215f5c7a2ae3943893a553 ;;
  v2.12.2-linux-arm64) expected=44cd40a8c76c86755375adfeea52cfd3533cb43d7bd647771e0ae065e166df3a ;;
  v2.12.2-darwin-amd64) expected=f6f06d94b6241521c53d15450c5209b028270bf966f842afb11c030c79f5bc16 ;;
  v2.12.2-darwin-arm64) expected=a9c54498731b3128f79e090be6110f3e5fffccc617b08142ed244d4126c73f29 ;;
  *)
    echo "No pinned checksum for golangci-lint ${version} on ${os}-${arch}; add it to $0" >&2
    exit 1
    ;;
esac

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1"
  else
    shasum -a 256 "$1"
  fi
}

name="golangci-lint-${version#v}-${os}-${arch}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl --fail --silent --show-error --location --retry 3 --retry-all-errors \
  "https://github.com/golangci/golangci-lint/releases/download/${version}/${name}.tar.gz" \
  --output "$tmp/$name.tar.gz"
actual="$(sha256 "$tmp/$name.tar.gz")"
[[ "${actual%% *}" == "$expected" ]] || {
  echo "golangci-lint ${version} checksum mismatch for ${name}.tar.gz" >&2
  exit 1
}
tar -xzf "$tmp/$name.tar.gz" -C "$tmp" "$name/golangci-lint"
mkdir -p "$(dirname "$destination")"
install -m 0755 "$tmp/$name/golangci-lint" "$destination"
"$destination" version
