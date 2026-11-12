# MCP Dock: design

Date: 2026-11-10

## Intent

A gateway that runs on the owner's homelab and is reachable from the internet. To MCP clients (Claude Desktop, claude.ai connectors, and similar tools) it is one remote MCP server over streamable HTTP. Behind it, it launches several local stdio MCP servers and exposes their tools as if they were its own.

The owner wants to use their local stdio MCP servers from Claude Desktop and similar remote clients, and let a few other people use them too, while keeping everyone else out.

First upstream integration: jaypost (https://bithyte.github.io/jaypost/, repo https://github.com/bithyte/jaypost), a local approval/notification service whose MCP server asks the owner questions through a Telegram or Discord bot.

## Goals

- Streamable HTTP MCP server endpoint usable from Claude Desktop custom connectors and other clients that speak the current MCP auth spec.
- Multiple users, each signing in with their own GitHub account. Only GitHub usernames on an admin-controlled allowlist get in.
- Multiple stdio upstream servers, configured only by the admin.
- Every authorized user sees every configured upstream's tools. No per-user permissions.
- Tools are exposed as `<server_name>__<tool_name>` (two underscores), e.g. `jaypost__ask_user`.
- Jaypost works end to end as the first upstream.

## Non-goals (v1)

- Reverse proxy, TLS termination, DNS, containerization, Podman/Quadlet units. Those are done later in the separate homelab project. The gateway listens on plain HTTP and assumes something in front terminates TLS.
- Running or managing the jaypost service itself (the bot-side service on 127.0.0.1:8787). The gateway only launches the jaypost MCP stdio process; the jaypost service is set up separately.
- Login providers other than GitHub (no Google, no Authentik/Keycloak).
- Per-user access to specific upstreams, roles, or quotas.
- Per-user upstream processes. One shared process per upstream.
- Per-user routing of jaypost questions. All users' jaypost questions go to the owner's single paired Telegram/Discord. The owner accepted this.
- Admin UI or API. The admin edits a config file.
- Hot reload of config.
- Forwarding upstream resources and prompts (see "Left to the builder" for the call made here).

## Anti-goals

- Anyone not on the allowlist can call any tool, list tools, or learn what upstreams exist.
- A remote user can read files off the gateway host through an upstream (jaypost's `attachment.path` is the known case).
- Secrets (GitHub client secret, jaypost token, issued tokens) show up in logs, the config file, or process listings.
- Removing someone from the allowlist doesn't actually cut them off until their token expires.

## Constraints and decisions

### Stack (owner's choice)

- Go. Single binary.
- Official MCP Go SDK: `github.com/modelcontextprotocol/go-sdk` (streamable HTTP server handler, stdio client via command transport). My call, the owner didn't object.
- Go 1.27 is installed on the dev box.

### Deployment shape

- Public URL is configured (e.g. `https://mcp.example.com`; owner's domain is `cozminu.com`, exact subdomain not decided). It's used for OAuth metadata, issuer, and redirect URLs. Don't derive it from request headers.
- Listen address configurable (e.g. `:8080`), plain HTTP.
- Later, gateway and jaypost will be containerized together on Podman. Keep that easy: config path and data dir via flags/env, logs to stdout, no assumptions about home directory layout.

### Authentication (owner chose GitHub only)

The gateway is its own OAuth 2.1 authorization server toward MCP clients and delegates the actual user login to GitHub. It stores no passwords and has no signup.

Flow:

1. Client hits the MCP endpoint without a token, gets `401` with `WWW-Authenticate` pointing at the protected resource metadata.
2. Client discovers the gateway's authorization server metadata, registers itself via dynamic client registration, starts an authorization code flow with PKCE (S256).
3. Gateway's `/authorize` redirects the browser to GitHub's OAuth authorize page (gateway's own GitHub OAuth App).
4. GitHub redirects back to the gateway's callback. Gateway exchanges the code, fetches the GitHub user, and checks the login against the allowlist. Not allowed: show a plain error page, issue nothing.
5. Allowed: gateway issues its own authorization code to the client's redirect URI; the client exchanges it at `/token` for a gateway access token and refresh token.
6. Client calls the MCP endpoint with `Authorization: Bearer <gateway token>`.

Must implement, per the current MCP authorization spec:

- OAuth 2.0 Protected Resource Metadata (RFC 9728) at `/.well-known/oauth-protected-resource`.
- Authorization Server Metadata (RFC 8414) at `/.well-known/oauth-authorization-server`.
- Dynamic Client Registration (RFC 7591), since Claude Desktop and most MCP clients rely on it.
- PKCE required (S256 only).
- Resource indicators: tokens are bound to the gateway's MCP resource URL; reject tokens issued for anything else.
- Claude's callback URL (`https://claude.ai/api/mcp/auth_callback`) and localhost redirect URIs (for desktop/CLI clients and MCP Inspector) must work.

GitHub tokens are only used during login to learn who the user is. They are not given to clients or upstreams and don't need to be stored.

Token policy (my call, owner said lgtm):

- Access tokens: about 1 hour. Refresh tokens: about 30 days, rotated on use.
- Allowlist is checked on every MCP request, not only at login. Removing a username from the config and restarting cuts that user off immediately.
- Tokens stored hashed, not in plaintext.

Consent screen (my call, raised by the design review). Open DCR plus a static GitHub app means an outsider could register a client with their own redirect URI and send an allowlisted user an `/authorize` link; GitHub skips its prompt for an already-approved app, so the code would go to the attacker. To prevent that, after GitHub login the gateway shows its own consent page: client name, the full redirect URI host, and Approve/Deny. Approval is remembered per (GitHub user, client) so it's a one-time click. Redirect URIs are otherwise not restricted, so other MCP clients can work.

Allowlist matches GitHub login names case-insensitively. The numeric GitHub user ID is recorded at first login. If a known login later maps to a different ID (account renamed, login grabbed by someone else), deny and log it (my call, follows the first anti-goal). Admin recovery: a CLI subcommand that clears the stored ID for a login, e.g. `mcp-dock users reset <login>`.

Client registration (my call): Dynamic Client Registration is required. Newer MCP spec revisions also allow Client ID Metadata Documents (CIMD). During implementation, check what current Claude Desktop actually uses. If it needs CIMD, implement it with fetch guards (https only, no private/loopback/link-local addresses, size and time limits, caching); otherwise leave CIMD for later.

### State

Small local persistent store (SQLite or similar, builder's choice) in a configurable data dir for: registered OAuth clients, refresh tokens, pending authorization requests, GitHub user ID mapping. A gateway restart must not log everyone out. Access tokens can be opaque random tokens looked up in the store, or signed tokens with a key kept in the data dir; builder's choice, but revocation-on-allowlist-removal must hold either way (the per-request allowlist check covers this).

### Config

One YAML file, edited by the admin. Secrets come from env vars, never the file. Rough shape (field names are the builder's call):

```yaml
public_url: https://mcp.example.com
listen: ":8080"
data_dir: /var/lib/mcp-dock

github:
  client_id: Iv1.xxxxx
  client_secret_env: GITHUB_CLIENT_SECRET

allowed_users:
  - alice-github-login
  - friend-login

upstreams:
  - name: jaypost
    command: jaypost
    args: ["mcp"]
    env:
      JAYPOST_URL: http://127.0.0.1:8787
    env_from:            # names of gateway env vars passed through
      - JAYPOST_TOKEN
    deny_args:
      - attachment.path
```

Upstream `name` must be safe as a tool prefix (lowercase letters, digits, single `_` or `-`; no `__`). Validate at startup and fail loudly on bad config.

Upstream processes get a minimal environment: only what the config gives them (plus `PATH` and `HOME`), not the gateway's whole environment, so the GitHub secret doesn't leak into upstreams.

### Upstream handling

- One shared stdio process per upstream, started when the gateway starts. All downstream sessions share it.
- Concurrent tool calls from different users go to the same upstream process concurrently (MCP supports multiple in-flight requests). Jaypost's `ask_user` blocks up to its timeout (default 300s, max 86400s), so one user's long call must not block others.
- Tool list: gateway lists each upstream's tools at startup and on `notifications/tools/list_changed` from the upstream, renames them to `<server>__<tool>`, and serves the merged list. On call, it strips the prefix and routes to the right upstream.
- Upstream crash: restart with exponential backoff. While it's down its tools are removed from the list, and the gateway sends `notifications/tools/list_changed` to connected clients. In-flight calls to a crashed upstream fail with a clear error.
- Progress notifications from upstream (jaypost sends these to keep long `ask_user` calls alive) are forwarded to the downstream caller that made the request, mapping progress tokens correctly.
- Cancellation from the downstream client is forwarded to the upstream request.
- Upstream stderr goes to the gateway log, tagged with the upstream name.

### Argument restrictions (the jaypost file-read problem)

Jaypost's `ask_user`, `ask_user_async`, and `notify_user` accept `attachment.path`, which makes the jaypost MCP process read a file from local disk and send it to the chat. Through the gateway that would let any remote user exfiltrate files from the homelab box to the owner's chat.

Decision (my call): a generic per-upstream `deny_args` list of dotted argument paths. For each listed path the gateway:

- removes it from the advertised input schemas, and
- rejects any tool call that includes it, with an error saying the argument isn't allowed through the gateway.

`attachment.text` stays allowed. Also run each upstream with its working directory set to an empty dir under the data dir, as a second layer.

### Logging

Structured logs to stdout. Log who (GitHub login) called which tool, when, duration, and success/failure. Don't log tool arguments or results by default. Never log tokens or secrets.

### Jaypost specifics (from its docs, 2026-11-10)

- Node 24.15+ for `npx`, or the standalone binary (`~/.local/bin/jaypost`).
- MCP launch: `jaypost mcp` (or `npx -y jaypost mcp`).
- Talks to the jaypost service at `JAYPOST_URL` with `JAYPOST_TOKEN`; without `JAYPOST_URL` it uses `JAYPOST_HOST`/`JAYPOST_PORT`, then `127.0.0.1:8787`, and reads `~/.config/jaypost/inbox.json`. Setting `JAYPOST_URL` + `JAYPOST_TOKEN` explicitly is preferred so the gateway doesn't depend on the inbox file.
- Tools: `ask_user`, `ask_user_async`, `wait_for_answer`, `cancel_question`, `list_questions`, `notify_user`.
- Background question state lives in the jaypost MCP process memory. If the gateway restarts that process, `ask_user_async` request IDs from before the restart are gone. Acceptable for v1; mention it in docs.
- `list_questions` and `wait_for_answer` are shared across all users since there's one process. User A can see or wait on user B's questions. Accepted along with "everything goes to the owner".
- Caller identity in jaypost messages: the owner only sees `[project_name]`, which the caller's agent chooses, so a friend's question can look like it came from the owner's own agent. Stamping the caller's GitHub login onto `project_name` was considered and deferred by the owner. v1 passes `project_name` through unchanged. Keep the call path such that a per-upstream argument rewrite can be added later.

## Success looks like

- The owner adds `https://<subdomain>.cozminu.com/mcp` (once the proxy exists) as a custom connector in Claude Desktop, signs in with GitHub, and sees `jaypost__ask_user` and friends.
- Calling `jaypost__ask_user` from Claude Desktop pings the owner's Telegram/Discord, and the answer comes back to Claude.
- A second allowlisted GitHub user can do the same from their own Claude Desktop.
- A GitHub user not on the allowlist can't get a token, and a removed user is cut off after restart.
- Adding a second stdio upstream is a config change only.

Before the reverse proxy exists, this should be testable locally: run with `public_url: http://localhost:8080`, a GitHub OAuth App whose callback is `http://localhost:8080/...`, and MCP Inspector or Claude Code as client.

## Left to the builder

- Package layout, HTTP router, storage engine, token format.
- Exact config field names and URL paths (keep the well-known paths per spec; MCP endpoint at `/mcp` suggested).
- Whether to forward upstream resources and prompts. My call: tools only in v1; jaypost only has tools. Design so adding them later isn't a rewrite.
- Backoff parameters, timeouts, graceful shutdown details.
- How downstream sessions map to the SDK's server/session model (one SDK server instance serving the merged tool list is fine).
- Test strategy. A small fake stdio MCP server for tests is expected; tests shouldn't need real GitHub or jaypost.
- A health endpoint (unauthenticated, reveals nothing about upstreams or users) for the future container setup.
