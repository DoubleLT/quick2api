#!/usr/bin/env bash
# Build quick2api for the supported targets.
# Usage: scripts/build.sh [version]
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-dev}"
LDFLAGS="-s -w -X main.version=${VERSION}"

mkdir -p dist

# The upstream chain needs the Quick desktop credential store + agent binary,
# which today means Windows. Other targets are built for the session_file /
# outpost_url code paths (attach to an outpost that already runs).
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/quick2api-windows-amd64.exe ./cmd/quick2api
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/quick2api-linux-amd64       ./cmd/quick2api
GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o dist/quick2api-darwin-arm64      ./cmd/quick2api

ls -la dist/
