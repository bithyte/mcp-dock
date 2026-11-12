# mcp-dock Release Packaging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish mcp-dock as tagged releases: a generic multi-arch image on GHCR that runs npm, pip and mounted-binary stdio upstreams with no consumer build, plus checksummed release binaries, all cut from a GitHub workflow.

**Architecture:** A Containerfile in the repo builds mcp-dock statically (cross-compiled in a `$BUILDPLATFORM` Go stage) onto a Debian trixie-slim runtime that carries node/npm, python3, uv and libatomic. `HOME=/data/home` puts npm/uv caches on the data volume; npm/uv settings come from system config files baked into the image because the gateway passes only `PATH`/`HOME` to upstreams. Three workflows: `ci.yml` (test + build + image test on PRs/main), `publish.yml` (binaries, GitHub release, image push; triggered by a `v*` tag push or called), `release.yml` (manual dispatch: validate, tag, call publish).

**Tech Stack:** Go 1.27.2, Podman/Docker (buildx), GitHub Actions, GHCR, bash.

**Spec:** `docs/superpowers/specs/2026-11-12-release-packaging-design.md`

## Global Constraints

- Work on the current branch `cozminu/package-build`. Never create or switch branches.
- Image: `ghcr.io/bithyte/mcp-dock`, platforms `linux/amd64,linux/arm64`, tags `X.Y.Z`, `X.Y`, `latest`.
- Image runs as `USER 65532:65532`, `ENTRYPOINT ["mcp-dock"]`, `CMD ["serve"]`, `ENV MCP_DOCK_CONFIG=/etc/mcp-dock/config.yaml HOME=/data/home`. Must work under `--read-only --cap-drop=ALL --security-opt no-new-privileges --user 65532:65532 --tmpfs /tmp`.
- mcp-dock binary: `CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags "-s -w -X main.version=<X.Y.Z>"`. Version string has no `v` prefix (my call).
- Every base image in the Containerfile pinned as `name:tag@sha256:<digest of the multi-arch index>`, not a single-platform digest.
- Every third-party action pinned by full commit sha with a `# vN` comment (matches the owner's pin-everything style; my call).
- Release assets: `mcp-dock-linux-x64.gz`, `mcp-dock-linux-arm64.gz`, `SHA256SUMS` (jaypost's naming; linux only, my call).
- Gateway env boundary unchanged: `config.UpstreamConfig.Environ` still passes only `PATH` and `HOME`.
- No secrets beyond the workflow's `GITHUB_TOKEN`. Docs never publish the port on all interfaces; examples use `127.0.0.1:8080:8080`.
- No em-dashes anywhere (code, docs, commits). No Claude/Anthropic attribution in commits or PRs.

## Review Focus

- Fresh, empty `/data` volume: npm and uv must create `/data/home/...` themselves; first start must work with no pre-created dirs. Task 2's image test always starts from an empty dir.
- arm64 image: the runtime stage runs `apt-get` under QEMU and the Go stage cross-compiles; a wrong `GOARCH` gives an amd64 binary in the arm64 image. Task 4's publish job checks `podman run --platform linux/arm64 ... version` (via QEMU) before pushing, and Task 6 pulls both platforms.
- Tag created by `release.yml` with `GITHUB_TOKEN` does not trigger `publish.yml`'s tag trigger, so the dispatch path must call publish directly and must not double-publish. Task 4 wires it with `workflow_call`; Task 6 checks only one publish run happened.
- `release.yml` dispatched from a non-main ref, or with a version that's already tagged or not plain `X.Y.Z`: it must refuse before tagging. Task 4 step lists the checks, and Task 4's local test runs the validation script against bad inputs.
- uvx silently downloading a newer python when a package's `requires-python` excludes trixie's 3.13: `python-downloads = "never"` must make that a visible failure, not a download. Task 2 test asserts `uv python find` uses `/usr/bin/python3` and that no `~/.local/share/uv/python` dir appears.

---

### Task 1: `mcp-dock version` subcommand

Needed so the image and binaries can be checked for the stamped version (my call; small and outside the gateway's behavior).

**Files:**
- Modify: `cmd/mcp-dock/main.go` (`usage` const, `run` switch)
- Test: `cmd/mcp-dock/main_test.go`

**Interfaces:**
- Produces: `mcp-dock version` prints `version` (the ldflags-stamped var, default `dev`) plus a newline to stdout and exits 0. Takes no args or flags. Later tasks run `mcp-dock version` and compare to `X.Y.Z`.

- [ ] **Step 1: Write the failing test**

```go
func TestVersion(t *testing.T) {
	old := version
	version = "1.2.3"
	t.Cleanup(func() { version = old })
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, testGetenv, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "1.2.3\n" {
		t.Errorf("version printed %q, want %q", out.String(), "1.2.3\n")
	}
	err := run(context.Background(), []string{"version", "extra"}, testGetenv, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("version extra = %v, want usage error", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/mcp-dock -run TestVersion -v`
Expected: FAIL (`unknown command "version"`).

- [ ] **Step 3: Implement**

Add `case "version":` to `run`: usage error `"version takes no arguments"` if `len(args) > 1`, else `fmt.Fprintln(stdout, version)`. Add `  mcp-dock version` to the `usage` const.

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/mcp-dock/main.go cmd/mcp-dock/main_test.go
git commit -m "feat: add version subcommand"
```

---

### Task 2: Containerfile, image config files, image test script

**Files:**
- Create: `Containerfile`
- Create: `.containerignore` (and a `.dockerignore` symlink to it)
- Create: `image/npmrc`, `image/uv.toml`
- Create: `scripts/test-image.sh`

**Interfaces:**
- Consumes: `mcp-dock version` (Task 1).
- Produces:
  - `Containerfile` with build arg `VERSION` (default `dev`). Builds with `podman build --build-arg VERSION=X.Y.Z -f Containerfile .` and `docker buildx build --platform linux/amd64,linux/arm64 ...`.
  - `scripts/test-image.sh <image-ref>`, env `CT` (container CLI, default `podman`; CI uses `docker`) and optional `EXPECT_VERSION`. Prints one `ok`/`FAIL` line per check, exits 0 only if all pass. Tasks 4 and 6 call it.

- [ ] **Step 1: Write `scripts/test-image.sh`** (it's the test; write it first)

Checks, in order, each as an `ok NAME` / `FAIL NAME: detail` line:
1. `usage`: `$CT run --rm --network none IMAGE` output contains `mcp-dock serve`.
2. `uid`: `--entrypoint id IMAGE -u` prints `65532`.
3. `version`: `--entrypoint mcp-dock IMAGE version` equals `$EXPECT_VERSION` (skip if unset).
4. `runtimes`: `node --version`, `npx --version`, `python3 --version`, `uv --version`, `uvx --version` all exit 0.
5. `libatomic`: `ldconfig -p` inside the image lists `libatomic.so.1`.
6. `uv_system_python`: `uv python find` (with `HOME=/tmp`) prints `/usr/bin/python3` (or the path python3 resolves to).
7. `three_upstreams`: in a fresh `mktemp -d` data dir (chmod 0777 so uid 65532 can write under both podman rootless and docker), write `config.yaml`:
   - `public_url: http://localhost:8080`, `listen: ":8080"`, `data_dir: /data`, `github.client_id: test`, `client_secret_env: GITHUB_CLIENT_SECRET`, `allowed_users: [test]`.
   - upstream `npm`: `command: npx`, `args: ["--prefer-offline", "-y", "@modelcontextprotocol/server-everything@<exact>"]`, plus whatever arg makes it serve stdio.
   - upstream `pip`: `command: uvx`, `args: ["mcp-server-time==<exact>"]`.
   - upstream `bin`: `command: /opt/fake/fakeupstream` (built on the host by the script with `CGO_ENABLED=0 go build -o <tmp>/fakeupstream ./internal/testutil/fakeupstream`, mounted `:ro`).
   Resolve the exact versions once with `npm view @modelcontextprotocol/server-everything version` and `curl -s https://pypi.org/pypi/mcp-server-time/json | jq -r .info.version`, and hardcode them in the script with a comment and the date.
   Run detached: `--read-only --cap-drop=ALL --security-opt no-new-privileges --user 65532:65532 --tmpfs /tmp -e GITHUB_CLIENT_SECRET=x -v DATA:/data -v DATA/config.yaml:/etc/mcp-dock/config.yaml:ro -v FAKE:/opt/fake:ro -p 127.0.0.1::8080`. Under podman rootless, add `--userns=keep-id:uid=65532,gid=65532`. Poll `$CT logs` up to 300 s for all three lines matching `"msg":"upstream started","upstream":"NAME","tools":[1-9]`. On timeout, FAIL and print the logs.
8. `healthz`: `curl` the published port `/healthz` returns `ok`.
9. `cache_persists`: record `find DATA/home/.npm/_cacache/content-v2 -type f` and `ls DATA/home/.cache/uv/archive-v0`; `$CT stop -t 20`, `$CT start`, wait for the three lines again; both listings unchanged.
10. `no_python_download`: `DATA/home/.local/share/uv/python` doesn't exist.

Cleanup on EXIT: remove the container; delete the data dir via `$CT run --rm --user 0 --entrypoint rm -v DATA:/d IMAGE -rf /d/home /d/gateway.db /d/upstreams`, then `rm -rf` it on the host.

- [ ] **Step 2: Run against a nonexistent image to see it fail**

Run: `scripts/test-image.sh localhost/mcp-dock:nope`
Expected: FAIL lines, nonzero exit.

- [ ] **Step 3: Write `image/npmrc` and `image/uv.toml`**

`image/npmrc`: `update-notifier=false`, `fund=false`, `audit=false`, `loglevel=warn`. Installed as npm's global config (`$(npm config get globalconfig)` in the base image; usually `/usr/local/etc/npmrc`).
`image/uv.toml`: `python-downloads = "never"` and `python-preference = "only-system"`. Installed at `/etc/uv/uv.toml`.

- [ ] **Step 4: Write `Containerfile`**

- Stage `build`: `FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-trixie@sha256:<index digest>`. Copy `go.mod`/`go.sum`, `go mod download`, copy source, then `CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build` with the Global Constraints flags and `-X main.version=$VERSION`.
- Stage `uv`: `FROM ghcr.io/astral-sh/uv:<current version>@sha256:<index digest>` (only to copy `/uv` and `/uvx`).
- Runtime: `FROM docker.io/library/node:24-trixie-slim@sha256:<index digest>`. `apt-get install --no-install-recommends python3 libatomic1 ca-certificates`, clear apt lists. Copy `uv`, `uvx` to `/usr/local/bin/`, `mcp-dock` to `/usr/local/bin/`, the two config files to their paths. `LABEL org.opencontainers.image.source=https://github.com/bithyte/mcp-dock` (links the GHCR package to the repo), plus `org.opencontainers.image.licenses` only if the repo has a license. `ENV`/`USER`/`ENTRYPOINT`/`CMD` per Global Constraints.
- Get each index digest with `skopeo inspect --raw docker://<ref> | sha256sum` or `podman manifest inspect` / `docker buildx imagetools inspect <ref>` (the top-level `Digest`). Put the tag and digest in a comment with the date.
- `.containerignore`: `.git`, `data/`, `*.db`, `/mcp-dock`, `docs/`.

- [ ] **Step 5: Build and test locally**

Run: `podman build --build-arg VERSION=0.0.0-test -t localhost/mcp-dock:test -f Containerfile . && EXPECT_VERSION=0.0.0-test scripts/test-image.sh localhost/mcp-dock:test`
Expected: every line `ok`, exit 0. If `three_upstreams` fails because npm or uv can't create `/data/home`, stop and report rather than adding an entrypoint (the spec leaves the fix open; it needs a decision).

- [ ] **Step 6: Commit**

```bash
git add Containerfile .containerignore .dockerignore image/ scripts/test-image.sh
git commit -m "feat: generic container image with node, python and uv for stdio upstreams"
```

---

### Task 3: Release binaries script

**Files:**
- Create: `scripts/build-release-binaries.sh`

**Interfaces:**
- Produces: `scripts/build-release-binaries.sh <X.Y.Z> <outdir>` writes `mcp-dock-linux-x64.gz`, `mcp-dock-linux-arm64.gz`, `SHA256SUMS` (`sha256sum` format over the two `.gz` files, sorted) into `<outdir>`. Refuses a version not matching `^[0-9]+\.[0-9]+\.[0-9]+$` or an existing non-empty `<outdir>`. Task 4 calls it.

- [ ] **Step 1: Run it before it exists**

Run: `scripts/build-release-binaries.sh 0.0.0 /tmp/rel-test`
Expected: `No such file`.

- [ ] **Step 2: Write the script**

`set -euo pipefail`. For `amd64->x64`, `arm64->arm64`: build with the Global Constraints flags, `GOOS=linux GOARCH=<arch>`, then `gzip -9n`. Then `(cd outdir && sha256sum *.gz | sort -k2 > SHA256SUMS)`.

- [ ] **Step 3: Verify**

Run: `rm -rf /tmp/rel-test && scripts/build-release-binaries.sh 0.0.0 /tmp/rel-test && (cd /tmp/rel-test && sha256sum -c SHA256SUMS) && gunzip -c /tmp/rel-test/mcp-dock-linux-x64.gz > /tmp/rel-test/m && chmod +x /tmp/rel-test/m && /tmp/rel-test/m version && file <(gunzip -c /tmp/rel-test/mcp-dock-linux-arm64.gz)`
Expected: both `OK`, prints `0.0.0`, `file` says ARM aarch64, statically linked. Also `scripts/build-release-binaries.sh v0.0.0 /tmp/x` exits nonzero.

- [ ] **Step 4: Commit**

```bash
git add scripts/build-release-binaries.sh
git commit -m "feat: script to build checksummed release binaries"
```

---

### Task 4: GitHub workflows

**Files:**
- Create: `.github/workflows/ci.yml`, `.github/workflows/publish.yml`, `.github/workflows/release.yml`
- Create: `scripts/check-release-version.sh`

**Interfaces:**
- Consumes: `scripts/test-image.sh` (Task 2), `scripts/build-release-binaries.sh` (Task 3).
- Produces: `scripts/check-release-version.sh <X.Y.Z>` exits 0 only if the version matches `^[0-9]+\.[0-9]+\.[0-9]+$` and `git ls-remote --tags origin refs/tags/vX.Y.Z` is empty. `publish.yml` is callable with input `version` (string, `X.Y.Z`).

- [ ] **Step 1: Write `scripts/check-release-version.sh` and check it on bad inputs**

Run: `for v in v0.1.0 0.1 0.1.0-rc.1 ''; do scripts/check-release-version.sh "$v" && echo "WRONGLY OK $v"; done; scripts/check-release-version.sh 0.1.0 && echo good`
Expected: no `WRONGLY OK` lines, prints `good` (no `v0.1.0` tag exists yet).

- [ ] **Step 2: `ci.yml`**

On `pull_request` and `push` to `main`. Permissions `contents: read`. Job: checkout, setup-go from `go.mod`, `go vet ./...`, `go test ./...`, `docker buildx build --load --build-arg VERSION=0.0.0-ci -t mcp-dock:ci .`, `CT=docker EXPECT_VERSION=0.0.0-ci scripts/test-image.sh mcp-dock:ci` (amd64 only; my call: arm64 gets checked at publish).

- [ ] **Step 3: `publish.yml`**

Triggers: `push: tags: ['v*']` and `workflow_call: inputs: version (required string)`. Version = input, else `${GITHUB_REF_NAME#v}`; re-check it with `^[0-9]+\.[0-9]+\.[0-9]+$`. Permissions `contents: write`, `packages: write`. Check out `refs/tags/v<version>`. Steps:
1. `go test ./...`.
2. `scripts/build-release-binaries.sh <version> dist`.
3. setup-qemu, setup-buildx, login to `ghcr.io` with `GITHUB_TOKEN`.
4. Build both platforms, load the amd64 one locally and run `CT=docker EXPECT_VERSION=<version> scripts/test-image.sh`; run `docker run --rm --platform linux/arm64 --entrypoint mcp-dock <arm64 image> version` and require `<version>`.
5. Push with tags `ghcr.io/bithyte/mcp-dock:<X.Y.Z>`, `:<X.Y>`, `:latest`.
6. `gh release create v<version> dist/* --generate-notes --verify-tag`, and append the image digest (`docker buildx imagetools inspect ... --format '{{json .Manifest.Digest}}'`) to the notes.

Concurrency group `publish-${{ version }}`, no cancel.

- [ ] **Step 4: `release.yml`**

`workflow_dispatch` with input `version` (`X.Y.Z`, no `v`). Permissions `contents: write`, `packages: write`. Job `tag`: fail unless `github.ref == 'refs/heads/main'`; checkout with full history; `scripts/check-release-version.sh`; `go test ./...`; `git tag -a v<version> -m v<version>` on the checked-out commit and push it with the default token (it won't fire `publish.yml`'s tag trigger, which is what we want). Job `publish`: `needs: tag`, `uses: ./.github/workflows/publish.yml`, `with: version`, `secrets: inherit`.

- [ ] **Step 5: Lint**

Run: `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 && shellcheck scripts/*.sh` (use whatever actionlint release is current; install shellcheck if missing)
Expected: no output, exit 0.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows scripts/check-release-version.sh
git commit -m "ci: test, publish and release workflows"
```

---

### Task 5: Docs

**Files:**
- Modify: `README.md`
- Create: `RELEASING.md`

- [ ] **Step 1: README container section**, near the top after "Build":
- A short paragraph, prominent: mcp-dock speaks plain HTTP. Any internet-facing deployment needs a TLS-terminating reverse proxy in front. The proxy must preserve the Host header, must not buffer responses (SSE), and needs long read timeouts (`ask_user` can wait a long time). Then a minimal Caddy example (`reverse_proxy 127.0.0.1:8080 { flush_interval -1 }` plus a `transport http { read_timeout ... }`).
- `podman run` / `docker run` example: image pinned `0.1.0` with a `@sha256:` placeholder note, `-p 127.0.0.1:8080:8080`, the hardening flags from Global Constraints, `-v ./data:/data`, config mount, `--env-file`.
- Upstream kinds, one config snippet each: npx (`--prefer-offline -y pkg@exact`), uvx (`pkg==exact`), mounted binary (`-v /path/bin:/usr/local/bin/name:ro`, glibc base and libatomic noted).
- Cache: `HOME=/data/home`, shared by all upstreams, override per upstream with `env: {HOME: ...}`; don't set `HOME=/tmp` on the container or caches are lost. Transitive deps aren't lockfile-pinned; the cache freezes them after first install.
- uvx uses the image's python 3.13 and never downloads one; a package needing another python fails loudly.
- Escape hatch: a 4-line `FROM ghcr.io/bithyte/mcp-dock:0.1.0` example that installs an npm package at build time (`USER 0`, `npm install -g pkg@ver`, `USER 65532:65532`).
- `mcp-dock version` added to Usage.
- Release binaries: download, `sha256sum -c SHA256SUMS --ignore-missing`.

- [ ] **Step 2: `RELEASING.md`**
Cutting a release: Actions, "release", Run workflow on `main`, enter `X.Y.Z`. Or push a `vX.Y.Z` tag by hand. What gets published. One-time: make the GHCR package public (package settings, "Change visibility"). Bumping pinned base images and actions is a normal PR.

- [ ] **Step 3: Check**

Run: `grep -n '—' README.md RELEASING.md; grep -nE '\-p (0\.0\.0\.0:)?8080:8080' README.md`
Expected: no output.

- [ ] **Step 4: Commit**

```bash
git add README.md RELEASING.md
git commit -m "docs: container image, upstream kinds, reverse proxy, releasing"
```

---

### Task 6: First release v0.1.0 (owner-gated steps)

- [ ] **Step 1:** Ask the owner before pushing the branch and opening the PR (outward-facing). Then `git push -u origin cozminu/package-build` and `gh pr create --base main` (PR body per the owner's writing rules, no attribution).
- [ ] **Step 2:** Watch the PR's `ci.yml` run: `gh pr checks --watch`. Expected: green. Fix and push if not.
- [ ] **Step 3:** Owner merges the PR. Wait for it.
- [ ] **Step 4:** `gh workflow run release.yml --ref main -f version=0.1.0`, then `gh run watch`. Expected: `tag` and `publish` jobs green; `gh run list --workflow publish.yml` shows no separate tag-triggered run.
- [ ] **Step 5:** If the push failed on org package permissions, stop and report. Otherwise ask the owner to make `ghcr.io/bithyte/mcp-dock` public, and wait.
- [ ] **Step 6:** Verify anonymously: `podman logout ghcr.io; podman pull ghcr.io/bithyte/mcp-dock:0.1.0 && EXPECT_VERSION=0.1.0 scripts/test-image.sh ghcr.io/bithyte/mcp-dock:0.1.0`; `podman run --rm --platform linux/arm64 --entrypoint mcp-dock ghcr.io/bithyte/mcp-dock:0.1.0 version` prints `0.1.0` (needs qemu-user-static; skip with a note if missing); tags `0.1` and `latest` resolve to the same digest; `gh release download v0.1.0 -D /tmp/r && (cd /tmp/r && sha256sum -c SHA256SUMS)`.
- [ ] **Step 7:** Report the image digest to the owner for their follow-up migration in `~/setup/ai/mcp-dock`.
