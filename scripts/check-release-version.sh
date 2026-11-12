#!/usr/bin/env bash
# Check that a release version is well formed and not yet tagged on origin.
# Usage: scripts/check-release-version.sh <X.Y.Z>
# Fails closed: if the remote can't be queried, exits nonzero.
set -euo pipefail

VERSION=${1-}

if ! [[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "error: version '$VERSION' must match X.Y.Z (no v prefix)" >&2
	exit 1
fi

if ! existing=$(git ls-remote --tags origin "refs/tags/v$VERSION"); then
	echo "error: could not query origin for tags" >&2
	exit 1
fi

if [ -n "$existing" ]; then
	echo "error: tag v$VERSION already exists on origin" >&2
	exit 1
fi
