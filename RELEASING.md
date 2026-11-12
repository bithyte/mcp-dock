# Releasing

## Cut a release

In GitHub, go to Actions, "release", Run workflow on `main`, and enter the version as `X.Y.Z` (no `v`). It checks the version, runs `go test ./...`, tags `vX.Y.Z`, then publishes.

You can also push a `vX.Y.Z` tag by hand; that triggers the same publish workflow. Nothing checks that path against main or CI: the tag publishes whatever commit it points at and moves `latest`. Only tag commits on main that passed CI.

## What gets published

- Image `ghcr.io/bithyte/mcp-dock` (linux/amd64 and linux/arm64) tagged `X.Y.Z`, `X.Y` and `latest`. The image is tested before it's pushed. `latest` and `X.Y` move on every release, including a backport tag.
- A GitHub release `vX.Y.Z` with `mcp-dock-linux-x64.gz`, `mcp-dock-linux-arm64.gz` and `SHA256SUMS`, generated notes, and the image digest.

## One-time setup

- Make the GHCR package public: package settings, "Change visibility". New packages start private.
- If the org restricts the default `GITHUB_TOKEN`, the repo's Actions settings need to let it write (tag push, release, package).

## When publish fails

If it fails after the tag is pushed, re-run the failed jobs from the Actions UI. That only helps for flaky failures: a re-run uses the files at the tagged commit, so a bug in `publish.yml` or the scripts fails again. Dispatching the same version again is refused because the tag already exists.

To recover from a real bug: fix it on main, delete the tag (`git push --delete origin vX.Y.Z`), delete any partially created GitHub release and any GHCR tags that got pushed, then dispatch the release again.

## Dependencies

Base images in the `Containerfile` and the actions in the workflows are pinned by digest or SHA. Bumping them is a normal PR.
