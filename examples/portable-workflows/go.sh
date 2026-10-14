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
# Kubernetes fsGroup adds set-group-ID to volume roots. Private test repositories
# must not inherit that permission, which checkpoint selection rejects.
chmod u=rwx,go=,ug-s "$GOCACHE" "$GOTMPDIR"
export TMPDIR="$GOTMPDIR"
exec /usr/local/go/bin/go "$@"
