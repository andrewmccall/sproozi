#!/bin/sh
# Public dependencies are image contents. Ephemeral build state is not portable.
set -eu
export GOTOOLCHAIN=local
export GOMODCACHE=/opt/sproozi-go-modules
export GOPROXY=off
export GOSUMDB=off
export CGO_ENABLED=0
export GOCACHE=/workspace/sproozi-go-build/cache
export GOTMPDIR=/workspace/sproozi-go-build/tmp
mkdir -p "$GOCACHE" "$GOTMPDIR"
exec /usr/local/go/bin/go "$@"
