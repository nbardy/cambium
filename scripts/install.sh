#!/usr/bin/env sh
set -eu

: "${GOBIN:=$(go env GOPATH)/bin}"
mkdir -p "$GOBIN"
go build -trimpath -o "$GOBIN/cambium" ./cmd/cambium
go build -trimpath -o "$GOBIN/git-cambium" ./cmd/git-cambium
printf 'installed %s and %s\n' "$GOBIN/cambium" "$GOBIN/git-cambium"
