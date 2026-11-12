# Releasing

## Cut a release

In GitHub, go to Actions, "release", Run workflow on `main`, and enter the version as `X.Y.Z` (no `v`). It checks the version, runs `go test ./...`, tags `vX.Y.Z`, then publishes.

You can also push a `vX.Y.Z` tag by hand; that triggers the same publish workflow.

## What gets published

- Image `ghcr.io/bithyte/mcp-dock` (linux/amd64 and linux/arm64) tagged `X.Y.Z`, `X.Y` and `latest`. The image is tested before it's pushed.
- A GitHub release `vX.Y.Z` with `mcp-dock-linux-x64.gz`, `mcp-dock-linux-arm64.gz` and `SHA256SUMS`, generated notes, and the image digest.

## One-time setup

- Make the GHCR package public: package settings, "Change visibility". New packages start private.
- If the org restricts the default `GITHUB_TOKEN`, the repo's Actions settings need to let it write (tag push, release, package).

## When publish fails

If it fails after the tag is pushed, re-run the failed jobs from the Actions UI. Dispatching the same version again is refused because the tag already exists.

## Dependencies

Base images in the `Containerfile` and the actions in the workflows are pinned by digest or SHA. Bumping them is a normal PR.
