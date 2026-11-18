# mcp-dock

OAuth-protected MCP gateway. Sits in front of streamable HTTP MCP servers, usually stdio servers (the first one is jaypost) each wrapped by an mcp-runner container, and exposes them at `<public_url>/mcp`. Users sign in with GitHub or with any OpenID Connect (OIDC) provider; only users on an allowlist get in. Every allowed user sees every configured upstream. Tools are exposed as `<upstream>__<tool>`, e.g. `jaypost__ask_user`. Tools only, no resources or prompts.

The gateway (the Dock) speaks plain HTTP. Put a TLS-terminating reverse proxy in front.

## Build

    go build ./cmd/mcp-dock
    go build ./cmd/mcp-runner

Optional version stamp: `go build -ldflags "-X main.version=1.0.0" ./cmd/mcp-dock` (same for `mcp-runner`).

## Container image

There are two images, both for linux/amd64 and linux/arm64, with tags `X.Y.Z`, `X.Y` and `latest`:

- `ghcr.io/bithyte/mcp-dock`: the gateway. Distroless static base with just the `mcp-dock` binary: no shell, no node, no python.
- `ghcr.io/bithyte/mcp-runner`: mcp-runner plus the toolchain stdio servers need (node 24, npm, python 3.13, uv, libatomic). One container per upstream; see [mcp-runner](#mcp-runner).

Pin an exact version in production, and the digest too if you can (`ghcr.io/bithyte/mcp-dock:0.2.0@sha256:<digest>`; the digests are in each release's notes). Both run as uid 65532 and work with a read-only rootfs and no capabilities.

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

The Dock image has no toolchain and runs no upstreams itself; it only talks to them over HTTP. It runs as uid 65532, so the data dir has to be writable by that uid. For docker or rootful podman:

    mkdir -p data && sudo chown 65532:65532 data

For rootless podman, add `--userns=keep-id:uid=65532,gid=65532` to the run command instead, so your host user owns the files. A named volume also works and needs no chown (the image ships `/data` owned by 65532): use `-v mcp-dock-data:/data` in place of `-v ./data:/data`.

    podman run -d --name mcp-dock \
      --read-only --cap-drop=ALL --security-opt no-new-privileges \
      --user 65532:65532 --tmpfs /tmp \
      -p 127.0.0.1:8080:8080 \
      --env-file ./mcp-dock.env \
      -v ./data:/data \
      -v ./config.yaml:/etc/mcp-dock/config.yaml:ro \
      ghcr.io/bithyte/mcp-dock:0.2.0

`mcp-dock.env` holds the Dock's own secrets (`GITHUB_CLIENT_SECRET=...`, and one client secret per OIDC provider, e.g. `WALLET_OIDC_SECRET=...`) and the runner bearer tokens your upstreams' `bearer_token_env` name. Upstream secrets (API keys and the like) go in each runner's env, not here. Set `data_dir: /data` in the config. The image's default command is `serve` and `MCP_DOCK_CONFIG` is `/etc/mcp-dock/config.yaml`. Running it with no config doesn't print usage; to check the binary, use `docker run --rm ghcr.io/bithyte/mcp-dock:0.2.0 version`. The same commands work with `docker` in place of `podman`.

The Dock has to reach each runner's URL. With containers, put them on a shared network, as in the [compose example](#compose-example).

### Upstreams

Each upstream in the Dock config is a URL, usually an mcp-runner container's `/mcp`:

    upstreams:
      - name: everything
        url: http://everything:8080/mcp
        bearer_token_env: EVERYTHING_RUNNER_TOKEN
      - name: time
        url: http://time:8080/mcp
        bearer_token_env: TIME_RUNNER_TOKEN

The hostnames are runner container names on a network the Dock shares with them. See [Config](#config) for the fields.

The Dock pings each upstream every 30s, with a 10s timeout. mcp-runner relays the ping to its child, so a child that handles requests one at a time and spends more than 10s on a call misses the ping, and the Dock resets that upstream's session.

## mcp-runner

mcp-runner serves one stdio MCP server command as streamable HTTP at `/mcp`, starting one child process per MCP session (the Dock keeps one session per upstream, shared by all users). `/healthz` returns `ok` without auth. Logs are JSON on stdout; each line the child writes to stderr becomes an `upstream stderr` record.

    mcp-runner [flags] [--] <command> [args...]

Flags go before the command. Each falls back to an env var, then to a default:

- `-listen addr` (`MCP_RUNNER_LISTEN`): listen address. Default `:8080`.
- `-max-sessions n` (`MCP_RUNNER_MAX_SESSIONS`): concurrent sessions, so concurrent children. Default 4. An initialize past the cap gets 503.
- `-idle-timeout dur` (`MCP_RUNNER_IDLE_TIMEOUT`): end a session, and its child, after `dur` with no request in flight. Default `10m`. The Dock's pings count as requests, so with a live Dock this only ends sessions the Dock abandoned.
- `-workdir dir` (`MCP_RUNNER_WORKDIR`): the child's working dir, created 0700 if missing. Default: the runner's current dir. The image sets `/data/cwd`.
- `-version`: print the version and exit.

`MCP_RUNNER_BEARER_TOKEN` (env only, no flag): if set, every `/mcp` request needs `Authorization: Bearer <token>`, else 401. If unset, the runner trusts anyone who can reach its port, so either set it or keep the runner on a network only the Dock can reach (preferably both). Pass it with `-e MCP_RUNNER_BEARER_TOKEN` (value from your shell) or an env file, never on the command line.

Any `/mcp` request with an `Origin` header gets 403, checked before the token. Browsers always send `Origin` and the Dock never does, so this keeps a web page from reaching a runner through DNS rebinding, which matters most with no token on `127.0.0.1`. It also means browser-based clients (MCP Inspector in direct mode, for one) can't talk to a runner directly; go through the Dock.

The child gets the runner's environment minus every `MCP_RUNNER_*` variable, so the token and runner settings never reach it. Put the upstream's own secrets in the runner container's env.

### Running a runner

The image's entrypoint is `mcp-runner` with no default command; the container command is the stdio server to run. The examples publish on `127.0.0.1` for a Dock running on the host; with a containerized Dock, use a shared network and don't publish at all. Pin every package to an exact version. Run runners with an init process (`--init` here, `init: true` in compose): the runner is otherwise PID 1, and when it kills an npx or uvx wrapper, the wrapper's own child is orphaned to PID 1, which only reaps its direct children, so it'd stay a zombie once it exits.

npx (`--prefer-offline` keeps registry revalidation to a minimum on restarts):

    podman run -d --name everything --init \
      --read-only --cap-drop=ALL --security-opt no-new-privileges \
      --user 65532:65532 --tmpfs /tmp \
      -p 127.0.0.1:8081:8080 \
      -e MCP_RUNNER_BEARER_TOKEN \
      -v everything-data:/data \
      ghcr.io/bithyte/mcp-runner:0.2.0 \
      npx --prefer-offline -y @modelcontextprotocol/server-everything@2.3.0 stdio

uvx:

    podman run -d --name time --init \
      --read-only --cap-drop=ALL --security-opt no-new-privileges \
      --user 65532:65532 --tmpfs /tmp \
      -p 127.0.0.1:8082:8080 \
      -e MCP_RUNNER_BEARER_TOKEN \
      -v time-data:/data \
      ghcr.io/bithyte/mcp-runner:0.2.0 \
      uvx mcp-server-time==2026.11.11

A mounted binary, with its secrets in an env file:

    podman run -d --name jaypost --init \
      --read-only --cap-drop=ALL --security-opt no-new-privileges \
      --user 65532:65532 --tmpfs /tmp \
      -p 127.0.0.1:8083:8080 \
      --env-file ./jaypost.env \
      -v jaypost-data:/data \
      -v /path/to/bin/jaypost:/usr/local/bin/jaypost:ro \
      ghcr.io/bithyte/mcp-runner:0.2.0 \
      jaypost mcp

`jaypost.env` holds `MCP_RUNNER_BEARER_TOKEN=...`, `JAYPOST_URL=...` and `JAYPOST_TOKEN=...`. The base is Debian (glibc), and libatomic is installed, so dynamically linked glibc binaries work. Musl-only binaries don't.

The command has to exist when the runner starts, or it exits with an error. The child itself only starts on the first initialize, so npx and uvx download on the Dock's first connect, and that first initialize can take a while.

### Package cache

The runner image sets `HOME=/data/home`, and the child inherits it, so npm's cache and uv's cache and tool dirs land on that runner's data volume. npm and uv create `/data/home` themselves on first start. Packages are downloaded once and survive restarts and image upgrades. Give each runner its own volume.

Don't set `HOME=/tmp` on a runner container. `/tmp` is a tmpfs, so every restart would re-download everything.

Only the top-level package is pinned. Its transitive dependencies aren't lockfile-pinned, so they're whatever resolved on first install; the cache freezes them from then on. Delete the runner's `/data/home` to force a fresh resolve.

uvx uses the image's python 3.13 and never downloads one (`/etc/uv/uv.toml` sets `python-downloads = "never"`). A package that needs a different python fails with an error rather than fetching one.

### Adding your own packages

If you don't want installs at startup, build on the runner image and install at build time. The global npm prefix is `/usr/local`, so the installed bin is on `PATH` and the container command is its bin name.

    FROM ghcr.io/bithyte/mcp-runner:0.2.0
    USER 0
    RUN HOME=/root npm install -g some-mcp-server@1.2.3
    USER 65532:65532
    CMD ["some-mcp-server"]

### Limitations

- The runner relays standard MCP methods transparently, but it uses the go-sdk server transport, so a client request with a non-standard method (or a standard method missing required params) is rejected with HTTP 400 and never reaches the child.
- Notifications the child sends on its own (not as part of a request's response) go out only on the session's standalone GET stream, and are dropped if none is open.
- No resumability: if the Dock-to-runner connection drops, the in-flight call fails.
- The runner only speaks the stateful protocol that starts with `initialize`. The Dock's client tries `server/discover` first on every connect, gets a 400 from the runner, and falls back to `initialize`, so each connect costs one extra round trip.

### Compose example

The Dock plus two runners. The runners are on an internal network with the Dock and publish no ports; only the Dock's port is published, on `127.0.0.1`. The runners are also on the default network so npx can reach the registry and jaypost can reach its service; drop that if your packages are baked into the image and the upstream needs no outbound access.

    services:
      mcp-dock:
        image: ghcr.io/bithyte/mcp-dock:0.2.0
        read_only: true
        cap_drop: [ALL]
        security_opt: [no-new-privileges]
        user: "65532:65532"
        tmpfs: [/tmp]
        stop_grace_period: 15s
        ports: ["127.0.0.1:8080:8080"]
        env_file: ./mcp-dock.env
        volumes:
          - dock-data:/data
          - ./config.yaml:/etc/mcp-dock/config.yaml:ro
        networks: [default, upstreams]

      everything:
        image: ghcr.io/bithyte/mcp-runner:0.2.0
        command: ["npx", "--prefer-offline", "-y", "@modelcontextprotocol/server-everything@2.3.0", "stdio"]
        init: true
        read_only: true
        cap_drop: [ALL]
        security_opt: [no-new-privileges]
        user: "65532:65532"
        tmpfs: [/tmp]
        stop_grace_period: 15s
        env_file: ./everything.env
        volumes: [everything-data:/data]
        networks: [default, upstreams]

      jaypost:
        image: ghcr.io/bithyte/mcp-runner:0.2.0
        command: ["jaypost", "mcp"]
        init: true
        read_only: true
        cap_drop: [ALL]
        security_opt: [no-new-privileges]
        user: "65532:65532"
        tmpfs: [/tmp]
        stop_grace_period: 15s
        env_file: ./jaypost.env
        # Lets the runner reach the jaypost service on the host; see below.
        extra_hosts: ["host.docker.internal:host-gateway"]
        volumes:
          - jaypost-data:/data
          - /path/to/bin/jaypost:/usr/local/bin/jaypost:ro
        networks: [default, upstreams]

    networks:
      upstreams:
        internal: true

    volumes:
      dock-data:
      everything-data:
      jaypost-data:

The env files:

- `mcp-dock.env`: `GITHUB_CLIENT_SECRET`, `EVERYTHING_RUNNER_TOKEN`, `JAYPOST_RUNNER_TOKEN`.
- `everything.env`: `MCP_RUNNER_BEARER_TOKEN`, the same value as `EVERYTHING_RUNNER_TOKEN`.
- `jaypost.env`: `MCP_RUNNER_BEARER_TOKEN` (same as `JAYPOST_RUNNER_TOKEN`), `JAYPOST_URL=http://host.docker.internal:8787`, `JAYPOST_TOKEN`.

The jaypost service is a placeholder until you've done the host-side setup. The service runs on the host and listens on `127.0.0.1:8787` by default, which no container can reach. `extra_hosts` maps `host.docker.internal` to the host's bridge gateway address, but the service also has to listen on that address (for docker's default bridge, usually `172.17.0.1`; check with `ip addr show docker0`). Bind it to that address only, not `0.0.0.0`: the gateway address is reachable from every container on the host, and `0.0.0.0` would expose the service on all interfaces. Anything that can reach that address with the API key can use jaypost, so firewall it if other containers on the host aren't trusted. Untested: how jaypost sets its listen address, and all of this setup; confirm it against jaypost's docs. With rootless podman, `host-gateway` support depends on the version and network backend, so that's untested too.

And the upstreams in `config.yaml`:

    upstreams:
      - name: everything
        url: http://everything:8080/mcp
        bearer_token_env: EVERYTHING_RUNNER_TOKEN
      - name: jaypost
        url: http://jaypost:8080/mcp
        bearer_token_env: JAYPOST_RUNNER_TOKEN
        deny_args: [attachment.path]

This compose file hasn't been run as written. The image settings in it are the ones the image tests use.

## Release binaries

Each GitHub release has `mcp-dock-linux-x64.gz`, `mcp-dock-linux-arm64.gz`, `mcp-runner-linux-x64.gz`, `mcp-runner-linux-arm64.gz` and one `SHA256SUMS` covering all four, for running without a container.

    sha256sum -c SHA256SUMS --ignore-missing
    gunzip mcp-dock-linux-x64.gz
    chmod +x mcp-dock-linux-x64

The `mcp-runner` binaries work the same way. Outside the image you supply node, python and uv yourself.

Run the check after downloading, before the `gunzip`.

## Usage

    mcp-dock serve [-config path]
    mcp-dock users reset [-config path] <login | github:<login> | oidc:<provider>:<value>>
    mcp-dock version

The config path defaults to `$MCP_DOCK_CONFIG`, else `./config.yaml`. Logs are JSON on stdout. `/healthz` returns `ok` without auth.

## GitHub OAuth App setup

1. GitHub, Settings, Developer settings, OAuth Apps, New OAuth App.
2. Homepage URL: your `public_url`.
3. Authorization callback URL: `<public_url>/oauth/github/callback`.
4. Put the client ID in the config (`providers.github.client_id`) and generate a client secret. The secret goes in an env var, never the config.

## OIDC provider setup

Any OpenID Connect provider works (Google, Okta, Keycloak, Authentik, single-tenant Entra, a company IdP, open-payments-oidc). You can configure several at once, alongside GitHub. Login is the authorization code flow with PKCE (S256) and a nonce. The ID token is verified (signature against the issuer's JWKS, `iss`, `aud`, `exp`, `nonce`, and `azp` if present). The IdP's access and refresh tokens are discarded after the code exchange.

1. Register mcp-dock at the IdP as a confidential client (one with a client secret). Authorization code flow only.
2. Redirect URI: `<public_url>/oauth/oidc/<name>/callback`, where `<name>` is the provider's `name` in the config.
3. Add the provider under `providers.oidc` (see [Config](#config)). The secret goes in an env var named by `client_secret_env`, never the config.
4. Add who may sign in under `allowed_users.oidc.<name>`.

Things to know:

- `issuer` must match the IdP's own issuer exactly, byte for byte, as in its `/.well-known/openid-configuration`. That includes a trailing slash if the IdP has one (Auth0 does). It's used as written. `http` is only accepted for `localhost`, `127.0.0.1` and `::1`.
- `user_claim` picks which ID token claim is the user's identity. Default `sub`, which is stable per issuer. `email` works too, but the ID token must also carry `email_verified: true` or the login is denied; request the `email` scope for it. The claim has to be in the ID token itself; the userinfo endpoint isn't read. If the claim is missing or isn't a non-empty string, login is denied.
- Use a `user_claim` that is immutable and unique per issuer. Claims like `preferred_username` or `upn` can be reassigned at some IdPs, and then a new holder of the name would log in as the old user.
- Multi-tenant Entra (the `common` and `organizations` endpoints) isn't supported: their issuer is templated, so it can't match the strict issuer check. Single-tenant Entra works.
- The IdP isn't contacted at startup. The first login through a provider fetches its discovery document and keys, and a success is cached for the life of the process. A failure isn't cached, the user sees "Couldn't reach <label>. Try again.", and the next attempt retries.

### open-payments-oidc

[open-payments-oidc](https://github.com/cozminu/open-payments-oidc) is a self-hosted OIDC provider where people sign in with an Open Payments wallet address. It replaces the wallet login mcp-dock used to have built in.

1. In open-payments-oidc's `clients.json`, add a confidential client for mcp-dock, with a `client_secret` and `redirect_uris` set to `<public_url>/oauth/oidc/wallet/callback` (use whatever `name` you gave the provider in place of `wallet`). Check its README for the exact format.
2. In mcp-dock, add the provider with `issuer` set to open-payments-oidc's root URL, and leave `user_claim` at `sub`. Its `sub` is the canonical wallet address URL.
3. Allowlist the `https://` form of the wallet address, as it appears in `sub`: `https://ilp.interledger-test.dev/alice`, not the `$host/path` shortcut. Allowlist entries for OIDC are matched as plain strings; there's no wallet address normalization.

```yaml
providers:
  oidc:
    - name: wallet
      label: Open Payments wallet
      issuer: https://oidc.example.com
      client_id: mcp-dock
      client_secret_env: WALLET_OIDC_SECRET
allowed_users:
  oidc:
    wallet:
      - https://ilp.interledger-test.dev/alice
```

## Config

See `config.example.yaml`. Unknown fields are rejected. A bad config fails startup.

- `public_url`: external https URL of the gateway, origin only (`https://host[:port]`, no path, query or fragment; the OAuth routes live at the root). Required. Used for OAuth metadata and callbacks; never taken from request headers. The MCP endpoint is `<public_url>/mcp`.
- `listen`: listen address. Default `:8080`.
- `data_dir`: required. Holds `gateway.db` (SQLite).
- `providers`: login providers. Configure at least one. The old top-level `github:` block and flat `allowed_users` list are rejected at startup, with a message saying where they moved.
  - `providers.github.client_id`: GitHub OAuth App client ID.
  - `providers.github.client_secret_env`: name of the env var holding the client secret.
  - `providers.oidc`: optional list of OIDC providers. Each entry:
    - `name`: required. Lowercase letters, digits, single `_` or `-`; unique among OIDC providers; not `github`. Used in user ids, `allowed_users.oidc` keys and the route paths. Renaming it changes its users' ids (see [Operational notes](#operational-notes)).
    - `label`: sign-in button text. Default: the name.
    - `issuer`: required. Absolute URL, no query, fragment or credentials; `https`, except `http` for `localhost`, `127.0.0.1` and `::1`. Kept exactly as written (no slash trimming). Two providers can't share an issuer (trailing slashes ignored when comparing).
    - `client_id`: required.
    - `client_secret_env`: required. Name of the env var holding the client secret; it must be non-empty at startup.
    - `scopes`: default `[openid]`. `openid` is always sent, and is added if you leave it out.
    - `user_claim`: ID token claim used as the user's identity. Default `sub`. With `email`, `email_verified` must be true.
  - The sign-in page shows one button per provider: GitHub first, then OIDC providers in config order. With exactly one provider, `/authorize` skips the page and goes straight to it.
  - `providers.open_payments` and `allowed_users.open_payments` are gone. Either one is rejected at startup with `config: built-in Open Payments login was removed; run open-payments-oidc and add it under providers.oidc`.
- `allowed_users`: per-provider allowlists. A list for a provider that isn't configured is a startup error. Checked at login, at token issue and refresh, and on every MCP request. There's no config hot reload, so removing an entry takes effect after a restart; the restart also drops that user's open sessions.
  - `allowed_users.github`: GitHub logins, case-insensitive.
  - `allowed_users.oidc.<name>`: entries for the OIDC provider with that name; naming a provider that isn't in `providers.oidc` is a startup error. Entries are trimmed and matched exactly against the claim value, with one exception: when the provider's `user_claim` is `email`, entries and the signed-in value are lowercased. An entry for one provider never admits a user from another, even with the same value.
- `upstreams`: list of streamable HTTP MCP servers, usually mcp-runner containers in front of stdio servers.
  - `name`: lowercase letters, digits, single `_` or `-`; no `__`; unique.
  - `url`: the server's MCP endpoint, e.g. `http://jaypost:8080/mcp`. Absolute `http` or `https` with a host; a path is fine, but no query, fragment or `user:pass@`, so credentials don't end up in the config file or in logged URLs. Kept as written. `http` is allowed since runners usually sit on an internal network.
  - `bearer_token_env`: optional. Name of the env var holding the bearer token sent as `Authorization: Bearer` on every request to this upstream. If set, the var must be non-empty at startup.
  - `deny_args`: dotted argument paths rejected before the call reaches the upstream (e.g. `attachment.path`). Each segment is an exact property name (no wildcards), matched case-insensitively. Arrays along the path are looked through, so `attachment.path` also blocks `{"attachment": [{"path": ...}]}`. Denied paths are also removed from the tool schemas clients see. A path that matches no parameter of any of the upstream's tools is logged as a warning, since it's probably a typo.
  - `command`, `args`, `env` and `env_from` (the old stdio fields) are rejected at startup with a message saying stdio upstreams moved to mcp-runner.

Users are separate per provider. GitHub login `alice` is user `github:alice`; claim value `https://example.com/alice` at the OIDC provider named `wallet` is user `oidc:wallet:https://example.com/alice` (the value can contain colons). The same person signing in at two providers is two users, each needing their own allowlist entry and having their own consent and tokens. There is no account linking. Existing GitHub users are migrated to `github:<login>` ids at startup, so their sessions and GitHub ID pins carry over.

Users who signed in with the old built-in Open Payments login have `open_payments:...` ids. Those tokens and consents no longer work; they sign in again through an OIDC provider and need a new allowlist entry under `allowed_users.oidc.<name>`. Nothing is migrated.

## Caller identity

Every `tools/call` the gateway sends to an upstream says who the signed-in user is. It's in `params._meta`:

```json
"io.github.bithyte.mcp-dock/user": {"provider": "github", "id": "octocat"}
```

`provider` is `github` or `oidc:<name>`. For GitHub, `id` is the login, lowercased. For OIDC, it's the claim value (lowercased if `user_claim` is `email`), e.g. `{"provider": "oidc:wallet", "id": "https://ilp.interledger-test.dev/alice"}`. `_meta` reaches stdio servers behind mcp-runner unchanged, so that's the field to read.

The same two values are also sent as HTTP headers, `X-MCP-Dock-User-Provider` and `X-MCP-Dock-User-Id`, for HTTP-native upstreams that can't read `_meta`. They go on every request the gateway makes on behalf of the call: the POST carrying the `tools/call`, and also a `notifications/cancelled` for it if the call is cancelled. mcp-runner ignores them. If a value isn't a valid HTTP header value, the headers are left out for that call and only `_meta` carries the identity. Requests not made on behalf of a call (initialize, tools/list, pings) carry none, since the session is shared by all users.

The gateway always sets these from the authenticated session. It never forwards a client's `_meta`, so a client can't claim to be someone else by sending the key itself. With no signed-in user there's no key and no headers.

An upstream can only trust these values because it trusts the gateway: keep it reachable only from the gateway (network isolation) or require a runner bearer token. Anyone who can reach the upstream directly can send any identity they like.

## Environment variables

- `GITHUB_CLIENT_SECRET` (or whatever `providers.github.client_secret_env` names): GitHub OAuth App secret.
- `WALLET_OIDC_SECRET` (or whatever an OIDC provider's `client_secret_env` names): that provider's client secret. One per provider.
- `JAYPOST_RUNNER_TOKEN` (or whatever an upstream's `bearer_token_env` names): bearer token for that upstream's runner.
- `MCP_DOCK_CONFIG`: default config path.

The runner's variables (`MCP_RUNNER_*`) are listed under [mcp-runner](#mcp-runner). Upstream secrets go in the runner's env, not the Dock's.

Both `serve` and `users reset` load and validate the full config, so `users reset` needs the same env vars set as `serve`.

## jaypost setup

jaypost's MCP process runs in an mcp-runner container (`jaypost mcp`, as in the mounted binary example); the jaypost service is separate and runs on the host.

1. Install the jaypost binary on the host and mount it into the runner, or use Node 24.15+.
2. Run `jaypost service install`.
3. Pair it with your Telegram/Discord bot.
4. Create an API key.
5. Put `JAYPOST_URL` and `JAYPOST_TOKEN` in the jaypost runner's env (`jaypost.env`), not the Dock's. `JAYPOST_URL` has to be reachable from inside the runner container: `127.0.0.1` there is the container itself, not the host, so the service's default `http://127.0.0.1:8787` won't work as is. See the note under the [compose example](#compose-example) for one way to set it up (untested).

These steps haven't been checked against jaypost's own docs. The owner should confirm them (the service install command and how the API key is created in particular).

## Local testing

1. Create a second GitHub OAuth App with callback `http://localhost:8080/oauth/github/callback`.
2. Config with `public_url: http://localhost:8080`, your login in `allowed_users.github`, a throwaway `data_dir`.
3. Start a runner for jaypost on the host: `JAYPOST_URL=http://127.0.0.1:8787 JAYPOST_TOKEN=... MCP_RUNNER_LISTEN=127.0.0.1:8081 ./mcp-runner jaypost mcp`, and point the upstream at it with `url: http://127.0.0.1:8081/mcp` (no `bearer_token_env` needed for a local test).
   Then `GITHUB_CLIENT_SECRET=... ./mcp-dock serve -config config.yaml`.
4. Run MCP Inspector in proxy mode (connection through the Inspector's proxy, not direct from the browser; `/mcp` sends no CORS headers, so a direct browser connection fails) and connect with streamable HTTP to `http://localhost:8080/mcp`. It registers via `/register` (dynamic client registration), sends you through the sign-in page (a button per configured provider) and the consent page, then lists `jaypost__*` tools.

## users reset

    mcp-dock users reset [-config path] <login | github:<login> | oidc:<provider>:<value>>

If a login is already bound to one GitHub numeric ID and someone with a different ID signs in as it, the gateway denies and logs a warning. `users reset` clears that user's stored GitHub ID, consents, and tokens so it can re-bind. It operates directly on the SQLite DB.

The argument is a GitHub login (`alice` or `github:alice`) or an OIDC user (`oidc:wallet:https://ilp.interledger-test.dev/alice`; everything after the second colon is the claim value). For an OIDC user it clears consents and tokens; there's no stored ID to clear. For a provider with `user_claim: email` the value is lowercased before reset, so `oidc:corp:Alice@Example.com` resets `alice@example.com`. The `$host/path` and `https://...` wallet shortcuts are gone, so `users reset '$example.com/alice'` is an error. It only resets that one user; users of the same name at other providers are unaffected.

## Known limits

- All jaypost questions go to the owner's single paired Telegram/Discord, whoever asked.
- `list_questions` is shared across users.
- Async request IDs are lost if the upstream restarts.
- `attachment.path` is blocked (`deny_args`) so remote users can't read host files.
- `project_name` is not stamped with the caller's identity.
- The OIDC login trusts the IdP's ID token and nothing else: no userinfo lookups, no group or role claims, no allowlist patterns (exact entries only), no account linking, no logout at the IdP.
- One shared process per upstream; no per-user access control, no config hot reload, no admin UI.

## Operational notes

- Refresh tokens rotate on every use. Replaying a used refresh token is rejected, but it does not revoke tokens already issued from it (known gap).
- Consent is remembered per (user, client, redirect URI), where the user is a GitHub login or an OIDC user.
- TLS must be terminated by a reverse proxy in front, and it must preserve the public Host header.
- Shutdown can take up to about 12s: up to 10s for open HTTP requests to drain, then closing the upstream sessions (about 2s each, in parallel). Set a container stop timeout of at least 15s.
- A child's stderr is logged verbatim by its runner (one `upstream stderr` record per line), so whatever an upstream prints, including anything sensitive, ends up in the runner's log.
- Existing GitHub logins are migrated automatically at startup; no manual database changes are needed.
- The IdP isn't contacted until the first login through that provider, so an IdP outage at startup doesn't stop the Dock, and GitHub logins and tokens already issued keep working.
- Changing an OIDC provider's `name` changes its users' ids (`oidc:<name>:<value>`). Their old tokens and consents stop working, they log in again, and the allowlist key has to be renamed to match.

## Manual verification (owner)

- [ ] Login through open-payments-oidc. Run it locally (`http://localhost:3000`), register mcp-dock as a confidential client with redirect URI `http://localhost:<port>/oauth/oidc/wallet/callback` and a `client_secret`, put the matching `providers.oidc` entry (`issuer: http://localhost:3000`, `client_secret_env` set) in the config, allowlist your wallet's `https://` form under `allowed_users.oidc.wallet`, and sign in from an MCP client. The consent page should say you're signed in as that `https://` address, and `jaypost` tool calls should carry `{"provider": "oidc:wallet", ...}`.
- [ ] Same check against open-payments-oidc deployed behind https.
- [ ] MCP Inspector completes OAuth via DCR against a real GitHub OAuth App, lists `jaypost__*` tools, and `jaypost__notify_user` delivers to Telegram/Discord.
- [ ] Once reachable over https, add as a Claude Desktop custom connector. Logs should show a hit on `/register`. If it sends a URL as `client_id` (Client ID Metadata Document), CIMD support is needed; that is a new task.
