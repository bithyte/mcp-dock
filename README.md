# mcp-dock

OAuth-protected MCP gateway. Sits in front of local stdio MCP servers (the first one is jaypost) and exposes them over streamable HTTP at `<public_url>/mcp`. Users sign in with GitHub or with an Open Payments wallet address; only users on an allowlist get in. Every allowed user sees every configured upstream. Tools are exposed as `<upstream>__<tool>`, e.g. `jaypost__ask_user`. Tools only, no resources or prompts.

The gateway speaks plain HTTP. Put a TLS-terminating reverse proxy in front.

## Build

    go build ./cmd/mcp-dock

Optional version stamp: `go build -ldflags "-X main.version=1.0.0" ./cmd/mcp-dock`.

## Usage

    mcp-dock serve [-config path]
    mcp-dock users reset [-config path] <login | wallet address>

The config path defaults to `$MCP_DOCK_CONFIG`, else `./config.yaml`. Logs are JSON on stdout. `/healthz` returns `ok` without auth.

## GitHub OAuth App setup

1. GitHub, Settings, Developer settings, OAuth Apps, New OAuth App.
2. Homepage URL: your `public_url`.
3. Authorization callback URL: `<public_url>/oauth/github/callback`.
4. Put the client ID in the config (`providers.github.client_id`) and generate a client secret. The secret goes in an env var, never the config.

## Open Payments setup

Optional second login method. The user types an Open Payments wallet address; their wallet provider confirms they control it; that address is their identity. The gateway needs a wallet address of its own to ask the provider for that confirmation.

1. Pick the gateway's wallet address (any wallet you control). It goes in `providers.open_payments.wallet_address`.
2. In that wallet's developer keys, generate an Ed25519 key pair. Note the key ID; it goes in `key_id`. Download the private key.
3. Put the private key in an env var, named by `private_key_env` (`OP_PRIVATE_KEY` in the example). The value is the PEM text (`-----BEGIN PRIVATE KEY-----`, PKCS#8) or the base64 of that PEM. It is never a file path; a path is rejected at startup. For base64: `base64 -w0 private.key`.
4. Add the wallet addresses that may sign in under `allowed_users.open_payments`.

The grant the gateway requests is subject-only: it asks the wallet provider who the user is, never for an access token, and cancels the grant right after login. No money moves.

Not tested against a real wallet provider by this repo's tests; they use an in-process fake. Try it against your own wallet before relying on it.

## Config

See `config.example.yaml`. Unknown fields are rejected. A bad config fails startup.

- `public_url`: external https URL of the gateway, origin only (`https://host[:port]`, no path, query or fragment; the OAuth routes live at the root). Required. Used for OAuth metadata and callbacks; never taken from request headers. The MCP endpoint is `<public_url>/mcp`.
- `listen`: listen address. Default `:8080`.
- `data_dir`: required. Holds `gateway.db` (SQLite) and `upstreams/<name>/cwd` (empty working dir for each upstream).
- `providers`: login providers. Configure at least one. The old top-level `github:` block and flat `allowed_users` list are rejected at startup, with a message saying where they moved.
  - `providers.github.client_id`: GitHub OAuth App client ID.
  - `providers.github.client_secret_env`: name of the env var holding the client secret.
  - `providers.open_payments.wallet_address`: the gateway's own wallet address (`$host/path` or `https://host/path`).
  - `providers.open_payments.key_id`: key ID of the gateway's Ed25519 key in that wallet.
  - `providers.open_payments.private_key_env`: name of the env var holding the private key (PEM text or base64 of the PEM, not a path).
- `allowed_users`: per-provider allowlists. A list for a provider that isn't configured is a startup error. Checked at login, at token issue and refresh, and on every MCP request. There's no config hot reload, so removing an entry takes effect after a restart; the restart also drops that user's open sessions.
  - `allowed_users.github`: GitHub logins, case-insensitive.
  - `allowed_users.open_payments`: wallet addresses. `$host/path` and `https://host/path` are the same entry; both are normalized to `https://host/path` at load. Only `https` is accepted, with no query, fragment or credentials. Scheme and host are case-insensitive (lowercased); the path is case-sensitive and kept as written. A trailing slash is ignored, and a bare `$host` means `https://host/.well-known/pay`.
- `upstreams`: list of stdio servers.
  - `name`: lowercase letters, digits, single `_` or `-`; no `__`; unique.
  - `command`, `args`: how to launch it. `command` must be an absolute path or a bare name found on `PATH`. Relative paths like `./bin/x` are rejected, because the upstream runs in an empty working dir.
  - `env`: literal env vars for the upstream (non-secret).
  - `env_from`: names of gateway env vars to pass through (secrets). Each must be set at startup.
  - `deny_args`: dotted argument paths rejected before the call reaches the upstream (e.g. `attachment.path`). Each segment is an exact property name (no wildcards), matched case-insensitively. Arrays along the path are looked through, so `attachment.path` also blocks `{"attachment": [{"path": ...}]}`. Denied paths are also removed from the tool schemas clients see. A path that matches no parameter of any of the upstream's tools is logged as a warning, since it's probably a typo.

Users are separate per provider. GitHub login `cozmin` is user `github:cozmin`; wallet `$example.com/cozmin` is user `open_payments:https://example.com/cozmin`. The same person signing in both ways is two users, each needing their own allowlist entry and having their own consent and tokens. There is no account linking. Existing GitHub users are migrated to `github:<login>` ids at startup, so their sessions and GitHub ID pins carry over.

Wallet login checks, in order: the typed address must be on the allowlist before the gateway makes any request to it (so it never fetches a URL a stranger typed); the `id` in the wallet's own document must be on the allowlist too, and have the same scheme, host and port as what was typed; and the address is checked once more after the wallet provider finalizes the grant.

Upstream processes get only `PATH`, `HOME`, their `env`, and their `env_from` vars, not the gateway's environment.

## Environment variables

- `GITHUB_CLIENT_SECRET` (or whatever `providers.github.client_secret_env` names): GitHub OAuth App secret.
- `OP_PRIVATE_KEY` (or whatever `providers.open_payments.private_key_env` names): the gateway's Ed25519 private key, PEM text or base64 of the PEM.
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
2. Config with `public_url: http://localhost:8080`, your login in `allowed_users.github`, a throwaway `data_dir`.
3. `GITHUB_CLIENT_SECRET=... JAYPOST_TOKEN=... ./mcp-dock serve -config config.yaml`
4. Run MCP Inspector in proxy mode (connection through the Inspector's proxy, not direct from the browser; `/mcp` sends no CORS headers, so a direct browser connection fails) and connect with streamable HTTP to `http://localhost:8080/mcp`. It registers via `/register` (dynamic client registration), sends you through the sign-in page (GitHub, or your wallet address if Open Payments is configured) and the consent page, then lists `jaypost__*` tools.

## users reset

    mcp-dock users reset [-config path] <login | wallet address>

If a login is already bound to one GitHub numeric ID and someone with a different ID signs in as it, the gateway denies and logs a warning. `users reset` clears that user's stored GitHub ID, consents, and tokens so it can re-bind. It operates directly on the SQLite DB.

The argument is a GitHub login (`cozmin` or `github:cozmin`) or a wallet address (`$example.com/cozmin`, an `https://` URL, or `open_payments:...`). It only resets that one user; the GitHub and wallet users of the same name are unaffected.

## Known limits

- All jaypost questions go to the owner's single paired Telegram/Discord, whoever asked.
- `list_questions` is shared across users.
- Async request IDs are lost if the upstream restarts.
- `attachment.path` is blocked (`deny_args`) so remote users can't read host files.
- `project_name` is not stamped with the caller's identity.
- One shared process per upstream; no per-user access control, no config hot reload, no admin UI.

## Operational notes

- Refresh tokens rotate on every use. Replaying a used refresh token is rejected, but it does not revoke tokens already issued from it (known gap).
- Consent is remembered per (user, client, redirect URI), where the user is a GitHub login or a wallet address.
- TLS must be terminated by a reverse proxy in front, and it must preserve the public Host header.
- Shutdown can take up to about 12s: up to 10s for open HTTP requests to drain, then closing the upstreams (about 2s each, in parallel). Set a container stop timeout of at least 15s.
- Upstream stderr is logged verbatim (one `upstream stderr` record per line), so whatever an upstream prints, including anything sensitive, ends up in the gateway log.
- No schema migrations yet. When upgrading across dev schema changes, delete `<data_dir>/gateway.db` (this invalidates all clients and tokens).

## Manual verification (owner)

- [ ] A wallet address on `allowed_users.open_payments` signs in through a real wallet provider, with the gateway's real key. Checking the wallet provider publishes the key under `key_id` is part of this; nothing at startup does.
- [ ] MCP Inspector completes OAuth via DCR against a real GitHub OAuth App, lists `jaypost__*` tools, and `jaypost__notify_user` delivers to Telegram/Discord.
- [ ] Once reachable over https, add as a Claude Desktop custom connector. Logs should show a hit on `/register`. If it sends a URL as `client_id` (Client ID Metadata Document), CIMD support is needed; that is a new task.
