# mcp-dock release packaging: design

Date: 2026-11-12

## Intent

Today the only consumer (the owner's homelab setup, `~/setup/ai/mcp-dock/`) builds mcp-dock from source. Its Containerfile git-clones a pinned 40-char commit, builds with a digest-pinned Go image, then adds the jaypost release binary (sha-checked twice) onto debian-slim and tags the result `localhost/mcp-dock:<sha12>-jaypost-<ver>`. Every mcp-dock upgrade means bumping the sha, a full Go rebuild, and re-running a local multi-stage build.

The owner wants the next upgrade to be a simpler setup. mcp-dock gets published as a real release that consumers pull instead of building. The image stays generic: any stdio MCP server can sit behind it, with first-class support for servers distributed as npm packages, pip packages, and single binaries.

## Goals

- Tagged releases of mcp-dock (semver, starting at `v0.1.0`).
- A generic container image on GHCR, `ghcr.io/bithyte/mcp-dock`, built and pushed by GitHub Actions when a `v*` tag is pushed. linux/amd64 and linux/arm64.
- The image can run, as upstreams, without any consumer-side build:
  - npm-distributed servers via `npx -y <pkg>@<exact version>` (node + npm in the image),
  - pip-distributed servers via `uvx <pkg>==<exact version>` (uv + a python in the image),
  - single binaries mounted read-only into the container, including dynamically linked glibc ones such as jaypost (glibc base).
- Packages npx/uvx download persist on the `/data` volume, so they're fetched once and survive restarts and image upgrades.
- Release binaries attached to each GitHub release, with a `SHA256SUMS` file, in the same shape jaypost uses (e.g. `mcp-dock-linux-x64.gz` plus `SHA256SUMS`). For people running mcp-dock without a container.
- Docs (README) cover: pulling the image, the three upstream kinds with a config example each, mounting binaries, the cache, and the escape hatch below.
- Docs recommend a TLS-terminating reverse proxy in front of any internet-facing deployment (owner's request). mcp-dock speaks plain HTTP. The README's container section says so prominently, next to the run examples rather than only in a later section. It also says the proxy must preserve the public Host header and allow streaming responses (SSE, no response buffering, long read timeouts, since `ask_user` can wait a long time), and it shows one minimal proxy example (Caddy or nginx, builder's pick). Run examples publish the port on `127.0.0.1` only, so the proxy is the only way in.

## Non-goals

- Bundling any specific upstream (jaypost included) in the published image.
- An npm package of mcp-dock itself. It's a Go binary; npm was considered and dropped.
- Migrating `~/setup/ai/mcp-dock/` to the published image. That's a separate follow-up in the setup repo, done after the first release exists. This design only has to make that migration easy (see "Success looks like").
- Runtimes beyond node and python (no deno, bun, java, ...). A user who needs one extends the image.
- Changing gateway behavior, config schema, or the OAuth flow, beyond what's strictly needed for the above.

## Anti-goals

- A published image that runs as root, needs capabilities, or requires a writable root filesystem. The current consumer runs it as uid 65532, read-only rootfs, `DropCapability=ALL`, `NoNewPrivileges`; the published image must keep working under exactly that.
- npx/uvx downloads landing somewhere ephemeral (e.g. `/tmp` tmpfs), so every restart re-downloads from the registry.
- Unpinned anything in the docs' examples (`npx -y pkg` without a version, `latest` image tags in examples meant for production).
- Release artifacts without checksums.
- Docs examples that publish the gateway port on all interfaces (`-p 8080:8080`, `0.0.0.0`) or otherwise suggest exposing plain-HTTP mcp-dock straight to the internet.

## Decisions

### Install at container start, not at build time (my recommendation, owner agreed)

Upstreams are installed when the container starts (npx/uvx) or mounted (binaries). No consumer build. The container already needs outbound network at runtime (GitHub OAuth, and jaypost in the owner's case), so build-time installs don't buy network isolation here.

Accepted tradeoff (owner said fine): npx/uvx pin the top-level package but resolve its transitive deps fresh on first install, so those aren't lockfile-pinned. The persistent cache keeps them from changing after the first install. Anyone who wants a full lock writes a small `FROM ghcr.io/bithyte/mcp-dock:<ver>` Containerfile that installs the upstream at build time. The README documents this as the escape hatch, with one example.

### Image contents

- glibc base (Debian slim, e.g. trixie-slim), so dynamically linked binaries like the jaypost release run when mounted. The jaypost x64 binary needs glibc, libstdc++, libgcc_s and libatomic; libatomic isn't in debian-slim, so the image must include it (the current consumer copies it in by hand).
- Node LTS with npm/npx.
- python3 and uv/uvx. A python in the image, so uvx doesn't download an interpreter at runtime. My call.
- CA certificates.
- `mcp-dock` built static (`CGO_ENABLED=0`, `-trimpath`, version stamped via `-X main.version=<tag>`).
- `USER 65532:65532`, `ENTRYPOINT ["mcp-dock"]`, `CMD ["serve"]`, `MCP_DOCK_CONFIG=/etc/mcp-dock/config.yaml`.
- `/data` is the expected data volume (`data_dir: /data`).
- The Containerfile lives in this repo and builds from the local checkout (not a git clone).

### Where caches go (my call)

The gateway passes only `PATH` and `HOME` from its own env to upstreams (plus each upstream's `env`/`env_from`, `internal/config/config.go` `Environ`). The image sets `HOME=/data/home`, so npm's `~/.npm` (including `_npx`) and uv's `~/.cache/uv` and tool dirs land on the data volume with no per-upstream config. All upstreams share that HOME. A per-upstream `env: {HOME: ...}` still overrides it.

The builder must check that npm, npx and uv actually work under uid 65532 with a read-only rootfs and HOME on `/data` (including creating `/data/home` if it doesn't exist; `/data` is a mounted volume, so the image can't pre-create it there).

The gateway's env boundary stays as it is (my call): `Environ` keeps passing only `PATH` and `HOME`, so image `ENV` other than those never reaches upstreams. Any npm or uv setting the image needs beyond HOME goes in a system-level config file baked into the image instead. That's the global npmrc for npm, and `/etc/uv/uv.toml` for uv, e.g. `python-downloads = "never"`, so uvx uses the image's python and never downloads an interpreter. Users override per upstream with `env:`. `/tmp` is expected to be a tmpfs (the README says to mount one, as the current quadlet does).

The current consumer sets `Environment=HOME=/tmp` in its quadlet. The migration has to drop that, and the README should say so.

### Tags and publishing

- A repeatable way to cut a release, all through GitHub Actions (owner's request). A manually triggered workflow (`workflow_dispatch`) takes a version input (`X.Y.Z`), validates it (semver, not already tagged, `go test ./...` green on the commit), creates and pushes the `vX.Y.Z` tag, and then the build/publish runs. Pushing a `vX.Y.Z` tag by hand also triggers the same build/publish, so both paths produce identical artifacts. Note: a tag pushed with the default `GITHUB_TOKEN` doesn't trigger other workflows, so either do the build/publish in the same workflow run (e.g. a reusable workflow called from both triggers) or use another mechanism. Builder's pick, but it must actually work end to end.
- The workflow creates the GitHub release with the binaries, `SHA256SUMS` and generated notes, and pushes the image.
- A short `RELEASING.md` (or a README section) says how to cut a release, and that the GHCR package must be public (one-time manual step).
- Image tags: `X.Y.Z`, `X.Y`, `latest`. My call. Docs' production examples pin `X.Y.Z` and recommend pinning the digest too.
- Release binaries for linux amd64 and arm64 at minimum. Gzipped, plus `SHA256SUMS`. Whether to also ship darwin is left to the builder.
- CI also runs `go test ./...` and builds (not pushes) the image on PRs and pushes to main, so a broken Containerfile shows up before a tag. My call.

## Success looks like

- `podman run --rm ghcr.io/bithyte/mcp-dock:0.1.0` prints the mcp-dock usage, runs as uid 65532.
- With a config listing three upstreams (one npm via npx, one pip via uvx, one mounted binary), run under `--read-only --cap-drop=ALL --security-opt no-new-privileges --user 65532:65532 -v <dir>:/data`, all three start and list tools. After a restart, npx/uvx don't download the package again (served from the cache). Some metadata revalidation against the registry is fine (my call; the container has network anyway). README examples use `npx --prefer-offline -y pkg@ver` to keep that to a minimum. The builder picks small real servers for this check; jaypost can be the binary one if it's convenient, otherwise any glibc-linked binary MCP server.
- The owner's follow-up migration is: delete their Containerfile and `build.sh`, set `Image=ghcr.io/bithyte/mcp-dock:0.1.0@sha256:...`, add `Volume=%h/apps/mcp-dock/bin/jaypost:/usr/local/bin/jaypost:ro` (or similar), drop `HOME=/tmp`, keep the rest of the quadlet hardening (pasta netns, keep-id, read-only, no caps). A later upgrade is bumping that one image line.

## First release (owner's decision)

The builder cuts `v0.1.0` through the release workflow once it's merged to main. Order:
1. Workflow and Containerfile changes merged to `main`. My assumption: the owner merges the PR.
2. Builder triggers the release workflow for `0.1.0`.
3. The first GHCR push creates a private package. The builder asks the owner to make it public (GitHub UI, needs `bithyte` admin) and waits.
4. Builder runs the "Success looks like" checks against the real `ghcr.io/bithyte/mcp-dock:0.1.0`, pulled anonymously. Before the merge, the same checks run against a locally built image.

If the first push fails because of org package settings, the builder stops and reports instead of working around it.

## Constraints

- Work on the current branch (`cozminu/package-build`).
- Go version per `go.mod` (1.27.2). Base images in the Containerfile pinned by digest, matching the owner's style; updating them is a normal PR.
- GHCR under `bithyte`, using the workflow's `GITHUB_TOKEN` with `packages: write`.
- No secrets in the image or workflow beyond `GITHUB_TOKEN`.

## Left to the builder

- Exact base image(s) and how node, python and uv get in (official node image as base, Debian packages, copying uv from its official image, ...), as long as the contents and constraints above hold.
- GoReleaser vs. a plain workflow script for the release binaries.
- Build provenance/attestations (nice to have, not required).
- Whether `/data/home` creation is handled by mcp-dock, a tiny entrypoint, or npm/uv creating it themselves (if they reliably do).
- Exact README layout.
