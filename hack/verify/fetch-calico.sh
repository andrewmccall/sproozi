#!/usr/bin/env bash
set -euo pipefail

# Download the Calico manifest used by Kind tests and verify it before use. CI,
# `make demo` and every documented fetch call this script, so the expected
# checksum lives only here. An existing file is verified, not trusted.
#
#   hack/verify/fetch-calico.sh DESTINATION_FILE
version=v3.30.4
expected=1e0f605a1f0a337a198a6ed8512c4ade67a2d8ab239f0bb67062a663fed84e6d
destination="${1:?usage: fetch-calico.sh DESTINATION_FILE}"

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1"
  else
    shasum -a 256 "$1"
  fi
}

verify() {
  local actual
  actual="$(sha256 "$1")"
  [[ "${actual%% *}" == "$expected" ]] || {
    echo "Calico $version checksum mismatch for $1" >&2
    return 1
  }
}

if [[ ! -f "$destination" ]]; then
  mkdir -p "$(dirname "$destination")"
  download="$(mktemp "${destination}.XXXXXX")"
  trap 'rm -f "$download"' EXIT
  curl --fail --location --silent --show-error --retry 3 --retry-all-errors \
    "https://raw.githubusercontent.com/projectcalico/calico/${version}/manifests/calico.yaml" \
    --output "$download"
  verify "$download"
  mv "$download" "$destination"
  trap - EXIT
fi
verify "$destination"
