#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <package> <test-regexp>" >&2
  exit 2
fi

package="$1"
pattern="$2"
listed="$(go test "$package" -list "$pattern")"
count="$(printf '%s\n' "$listed" | awk '/^Test[[:alnum:]_]+$/ { count++ } END { print count + 0 }')"
if [[ "$count" -eq 0 ]]; then
  echo "required test selection matched zero tests: package=$package pattern=$pattern" >&2
  exit 1
fi

output="$(mktemp)"
trap 'rm -f "$output"' EXIT
if ! go test "$package" -run "$pattern" -count=1 -json | tee "$output"; then
  exit 1
fi
if grep -q '"Action":"skip"' "$output"; then
  echo "required test selection skipped at least one test: package=$package pattern=$pattern" >&2
  exit 1
fi
