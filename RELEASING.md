# Releasing

## Cut a release

In GitHub, go to Actions, "release", Run workflow on `main`, and enter the version as `X.Y.Z` (no `v`). It checks the version, runs `go test ./...`, tags `vX.Y.Z`, then publishes.

You can also push a `vX.Y.Z` tag by hand; that triggers the same publish workflow. Nothing checks that path against main or CI: the tag publishes whatever commit it points at and moves `latest`. Only tag commits on main that passed CI.

## What gets published

- Images `ghcr.io/bithyte/mcp-dock` and `ghcr.io/bithyte/mcp-runner` (both linux/amd64 and linux/arm64), each tagged `X.Y.Z`, `X.Y` and `latest`. Both are tested before either is pushed. `latest` and `X.Y` move on every release, including a backport tag.
- A GitHub release `vX.Y.Z` with `mcp-dock-linux-x64.gz`, `mcp-dock-linux-arm64.gz`, `mcp-runner-linux-x64.gz`, `mcp-runner-linux-arm64.gz` and one `SHA256SUMS` covering all four, generated notes, and both image digests.

## One-time setup

- Make the GHCR packages public: package settings, "Change visibility". New packages start private. Do it for `mcp-dock` and for the new `mcp-runner` package. `mcp-runner` doesn't exist until the first release pushes it, so flip it to public right after that release.
- If the org restricts the default `GITHUB_TOKEN`, the repo's Actions settings need to let it write (tag push, release, package).

## When publish fails

If it fails after the tag is pushed, re-run the failed jobs from the Actions UI. That only helps for flaky failures: a re-run uses the files at the tagged commit, so a bug in `publish.yml` or the scripts fails again. Dispatching the same version again is refused because the tag already exists.

The Dock image is pushed before the runner image, so a failed runner push can leave `mcp-dock:X.Y.Z` (and its `X.Y` and `latest` tags) already published with no matching `mcp-runner`. Check both GHCR packages. Re-running is safe: each push overwrites the same tags, and the GitHub release is created last, so a failed run never leaves a release behind. A re-run may rebuild the Dock image with a different digest; the release notes record the digests that were pushed last.

To recover from a real bug: fix it on main, delete the tag (`git push --delete origin vX.Y.Z`), delete any partially created GitHub release and any GHCR tags that got pushed, then dispatch the release again.

## Dependencies

Base images in the `Containerfile` and `Containerfile.runner` and the actions in the workflows are pinned by digest or SHA. Bumping them is a normal PR.
