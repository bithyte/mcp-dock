#!/usr/bin/env bash
# Build the linux release binaries and checksums.
# Usage: scripts/build-release-binaries.sh <X.Y.Z> <outdir>
# Writes mcp-dock-linux-x64.gz, mcp-dock-linux-arm64.gz and SHA256SUMS into <outdir>.
set -euo pipefail

VERSION=${1:?usage: build-release-binaries.sh <X.Y.Z> <outdir>}
OUTDIR=${2:?usage: build-release-binaries.sh <X.Y.Z> <outdir>}

if ! [[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "error: version '$VERSION' must match X.Y.Z (no v prefix)" >&2
	exit 1
fi

if [ -e "$OUTDIR" ] && [ -n "$(ls -A "$OUTDIR")" ]; then
	echo "error: outdir '$OUTDIR' exists and is not empty" >&2
	exit 1
fi
mkdir -p "$OUTDIR"

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUTDIR=$(cd "$OUTDIR" && pwd)

# GOARCH:asset-suffix
for pair in amd64:x64 arm64:arm64; do
	goarch=${pair%%:*}
	suffix=${pair##*:}
	(
		cd "$REPO_ROOT"
		CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -mod=readonly -trimpath \
			-ldflags "-s -w -X main.version=$VERSION" \
			-o "$OUTDIR/mcp-dock-linux-$suffix" ./cmd/mcp-dock
	)
	gzip -9n "$OUTDIR/mcp-dock-linux-$suffix"
done

(cd "$OUTDIR" && sha256sum ./*.gz | sed 's# \./# #' | sort -k2 >SHA256SUMS)
