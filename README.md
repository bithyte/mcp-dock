# mcp-dock

OAuth-protected MCP gateway. Sits in front of local stdio MCP servers (the first one is jaypost) and exposes them over streamable HTTP at `<public_url>/mcp`. Users sign in with GitHub; only logins on an allowlist get in. Every allowed user sees every configured upstream. Tools are exposed as `<upstream>__<tool>`, e.g. `jaypost__ask_user`. Tools only, no resources or prompts.

The gateway speaks plain HTTP. Put a TLS-terminating reverse proxy in front.

## Build

    go build ./cmd/mcp-dock

Optional version stamp: `go build -ldflags "-X main.version=1.0.0" ./cmd/mcp-dock`.

## Container image

Images are published to `ghcr.io/bithyte/mcp-dock` with tags `X.Y.Z`, `X.Y` and `latest`, for linux/amd64 and linux/arm64. Pin an exact version in production, and the digest too if you can (`ghcr.io/bithyte/mcp-dock:0.1.0@sha256:<digest>`; the digest is in each release's notes).

mcp-dock speaks plain HTTP. Any internet-facing deployment needs a TLS-terminating reverse proxy in front of it. The proxy must preserve the Host header, must not buffer responses (the MCP endpoint streams over SSE), and needs long read timeouts, since `ask_user` can wait a long time. Publish the container port on `127.0.0.1` only, so the proxy is the only way in. A minimal Caddy example (Caddy passes the Host header through by default):

    mcp.example.com {
        reverse_proxy 127.0.0.1:8080 {
            flush_interval -1
            transport http {
                read_timeout 1h
            }
        }
    }

### Running it

The image runs as uid 65532, so the data dir has to be writable by that uid. For docker or rootful podman:

    mkdir -p data && sudo chown 65532:65532 data

For rootless podman, add `--userns=keep-id:uid=65532,gid=65532` to the run command instead, so your host user owns the files. A named volume also works and needs no chown (the image ships `/data` owned by 65532): use `-v mcp-dock-data:/data` in place of `-v ./data:/data`.

    podman run -d --name mcp-dock \
      --read-only --cap-drop=ALL --security-opt no-new-privileges \
      --user 65532:65532 --tmpfs /tmp \
      -p 127.0.0.1:8080:8080 \
      --env-file ./mcp-dock.env \
      -v ./data:/data \
      -v ./config.yaml:/etc/mcp-dock/config.yaml:ro \
      ghcr.io/bithyte/mcp-dock:0.1.0

`mcp-dock.env` holds the secrets (`GITHUB_CLIENT_SECRET=...`, plus whatever your upstreams' `env_from` names). Set `data_dir: /data` in the config. The image's default command is `serve` and `MCP_DOCK_CONFIG` is `/etc/mcp-dock/config.yaml`. Running it with no config doesn't print usage; to check the binary, use `docker run --rm --entrypoint mcp-dock ghcr.io/bithyte/mcp-dock:0.1.0 version`. The same commands work with `docker` in place of `podman`.

### Upstreams

The image has node 24, npm, python 3.13, uv and libatomic. Pin every upstream to an exact version.

npx (`--prefer-offline` keeps registry revalidation to a minimum on restarts):

    - name: everything
      command: npx
      args: ["--prefer-offline", "-y", "@modelcontextprotocol/server-everything@2.3.0", "stdio"]

uvx:

    - name: time
      command: uvx
      args: ["mcp-server-time==2026.11.11"]

A mounted binary, added to the run command with `-v /path/to/bin/jaypost:/usr/local/bin/jaypost:ro`:

    - name: jaypost
      command: jaypost
      args: ["mcp"]

The base is Debian (glibc), and libatomic is installed, so dynamically linked glibc binaries work. Musl-only binaries don't.

### Package cache

The image sets `HOME=/data/home`, and the gateway passes `HOME` to every upstream, so npm's cache and uv's cache and tool dirs land on the data volume. npm and uv create `/data/home` themselves on first start. Packages are downloaded once and survive restarts and image upgrades. All upstreams share this HOME; override it for one with `env: {HOME: /data/home-foo}`.

Don't set `HOME=/tmp` on the container. `/tmp` is a tmpfs, so every restart would re-download everything.

Only the top-level package is pinned. Its transitive dependencies aren't lockfile-pinned, so they're whatever resolved on first install; the cache freezes them from then on. Delete `data/home` to force a fresh resolve.

uvx uses the image's python 3.13 and never downloads one (`/etc/uv/uv.toml` sets `python-downloads = "never"`). A package that needs a different python fails with an error rather than fetching one.

### Adding your own packages

If you don't want installs at startup, build on the image and install at build time. The global npm prefix is `/usr/local`, so the installed bin is on `PATH` and `command:` is its bin name.

    FROM ghcr.io/bithyte/mcp-dock:0.1.0
    USER 0
    RUN HOME=/root npm install -g some-mcp-server@1.2.3
    USER 65532:65532

## Release binaries

Each GitHub release has `mcp-dock-linux-x64.gz`, `mcp-dock-linux-arm64.gz` and `SHA256SUMS`, for running without a container.

    sha256sum -c SHA256SUMS --ignore-missing
    gunzip mcp-dock-linux-x64.gz
    chmod +x mcp-dock-linux-x64

Run the check after downloading, before the `gunzip`.

## Usage

    mcp-dock serve [-config path]
    mcp-dock users reset [-config path] <login>
    mcp-dock version

The config path defaults to `$MCP_DOCK_CONFIG`, else `./config.yaml`. Logs are JSON on stdout. `/healthz` returns `ok` without auth.

## GitHub OAuth App setup

1. GitHub, Settings, Developer settings, OAuth Apps, New OAuth App.
2. Homepage URL: your `public_url`.
3. Authorization callback URL: `<public_url>/oauth/github/callback`.
4. Put the client ID in the config (`github.client_id`) and generate a client secret. The secret goes in an env var, never the config.

## Config

See `config.example.yaml`. Unknown fields are rejected. A bad config fails startup.

- `public_url`: external https URL of the gateway, origin only (`https://host[:port]`, no path, query or fragment; the OAuth routes live at the root). Required. Used for OAuth metadata and callbacks; never taken from request headers. The MCP endpoint is `<public_url>/mcp`.
- `listen`: listen address. Default `:8080`.
- `data_dir`: required. Holds `gateway.db` (SQLite) and `upstreams/<name>/cwd` (empty working dir for each upstream).
- `github.client_id`: GitHub OAuth App client ID.
- `github.client_secret_env`: name of the env var holding the client secret.
- `allowed_users`: GitHub logins, case-insensitive. Checked at login, at token issue and refresh, and on every MCP request. There's no config hot reload, so removing a login takes effect after a restart; the restart also drops that user's open sessions.
- `upstreams`: list of stdio servers.
  - `name`: lowercase letters, digits, single `_` or `-`; no `__`; unique.
  - `command`, `args`: how to launch it. `command` must be an absolute path or a bare name found on `PATH`. Relative paths like `./bin/x` are rejected, because the upstream runs in an empty working dir.
  - `env`: literal env vars for the upstream (non-secret).
  - `env_from`: names of gateway env vars to pass through (secrets). Each must be set at startup.
  - `deny_args`: dotted argument paths rejected before the call reaches the upstream (e.g. `attachment.path`). Each segment is an exact property name (no wildcards), matched case-insensitively. Arrays along the path are looked through, so `attachment.path` also blocks `{"attachment": [{"path": ...}]}`. Denied paths are also removed from the tool schemas clients see. A path that matches no parameter of any of the upstream's tools is logged as a warning, since it's probably a typo.

Upstream processes get only `PATH`, `HOME`, their `env`, and their `env_from` vars, not the gateway's environment.

## Environment variables

- `GITHUB_CLIENT_SECRET` (or whatever `github.client_secret_env` names): GitHub OAuth App secret.
- `JAYPOST_TOKEN` (when using the jaypost upstream from the example config): jaypost API key.
- `MCP_DOCK_CONFIG`: default config path.

Both `serve` and `users reset` load and validate the full config, so `users reset` needs the same env vars set as `serve`.

## jaypost setup

The gateway only launches the jaypost MCP process; the jaypost service is separate.

1. Install the jaypost binary, or use Node 24.15+.
2. Run `jaypost service install`.
3. Pair it with your Telegram/Discord bot.
4. Create an API key.
5. Set `JAYPOST_URL=http://127.0.0.1:8787` (in the upstream `env`, as in the example) and export `JAYPOST_TOKEN` for the gateway.

These steps haven't been checked against jaypost's own docs. The owner should confirm them (the service install command and how the API key is created in particular).

## Local testing

1. Create a second GitHub OAuth App with callback `http://localhost:8080/oauth/github/callback`.
2. Config with `public_url: http://localhost:8080`, your login in `allowed_users`, a throwaway `data_dir`.
3. `GITHUB_CLIENT_SECRET=... JAYPOST_TOKEN=... ./mcp-dock serve -config config.yaml`
4. Run MCP Inspector in proxy mode (connection through the Inspector's proxy, not direct from the browser; `/mcp` sends no CORS headers, so a direct browser connection fails) and connect with streamable HTTP to `http://localhost:8080/mcp`. It registers via `/register` (dynamic client registration), sends you through GitHub login and the consent page, then lists `jaypost__*` tools.

## users reset

    mcp-dock users reset [-config path] <login>

If a login is already bound to one GitHub numeric ID and someone with a different ID signs in as it, the gateway denies and logs a warning. `users reset` clears that login's stored GitHub ID, consents, and tokens so it can re-bind. It operates directly on the SQLite DB.

## Known limits

- All jaypost questions go to the owner's single paired Telegram/Discord, whoever asked.
- `list_questions` is shared across users.
- Async request IDs are lost if the upstream restarts.
- `attachment.path` is blocked (`deny_args`) so remote users can't read host files.
- `project_name` is not stamped with the caller's identity.
- One shared process per upstream; no per-user access control, no config hot reload, no admin UI.

## Operational notes

- Refresh tokens rotate on every use. Replaying a used refresh token is rejected, but it does not revoke tokens already issued from it (known gap).
- Consent is remembered per (GitHub user, client, redirect URI).
- TLS must be terminated by a reverse proxy in front, and it must preserve the public Host header.
- Shutdown can take up to about 12s: up to 10s for open HTTP requests to drain, then closing the upstreams (about 2s each, in parallel). Set a container stop timeout of at least 15s.
- Upstream stderr is logged verbatim (one `upstream stderr` record per line), so whatever an upstream prints, including anything sensitive, ends up in the gateway log.
- No schema migrations yet. When upgrading across dev schema changes, delete `<data_dir>/gateway.db` (this invalidates all clients and tokens).

## Manual verification (owner)

- [ ] MCP Inspector completes OAuth via DCR against a real GitHub OAuth App, lists `jaypost__*` tools, and `jaypost__notify_user` delivers to Telegram/Discord.
- [ ] Once reachable over https, add as a Claude Desktop custom connector. Logs should show a hit on `/register`. If it sends a URL as `client_id` (Client ID Metadata Document), CIMD support is needed; that is a new task.
