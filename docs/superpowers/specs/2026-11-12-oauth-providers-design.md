# Multiple login providers, with Open Payments as the second: design

Date: 2026-11-12

Builds on `docs/superpowers/specs/2026-11-10-mcp-dock-design.md`. Everything there still holds unless this doc changes it. The earlier doc listed "login providers other than GitHub" as a non-goal; this lifts it.

## Intent

Today the only way to sign in to mcp-dock is GitHub. The owner wants a second way in, for people who don't have a GitHub account and for when GitHub is down. The second provider is Open Payments: the user proves they control an Open Payments wallet address, and that address is their identity.

The code should stop assuming GitHub is the only provider, so a third provider later is cheap.

References the owner pointed at:

- https://openpayments.dev/guides/verify-wallet-address-ownership/
- https://github.com/interledger/open-payments-go
- https://github.com/cozminu/open-payments-login-node (owner's own Node implementation of the same login; the owner has tested it against a live wallet provider and it works)

## Goals

- Users can sign in with GitHub or with an Open Payments wallet address.
- A GitHub user and a wallet user are always separate users, even if they're the same person. No account linking. Each needs its own allowlist entry, and each has its own consents and tokens.
- Each provider is optional in config, but at least one must be configured.
- A sign-in page lets the user pick a provider (layout chosen below).
- Everything after login (consent, codes, tokens, refresh, allowlist checks on every MCP request, `users reset`) works the same for both providers.

## Non-goals

- Account linking or merging across providers.
- Any payment. The Open Payments grant is subject-only; no access token for payments, quotes or incoming payments is ever requested.
- Keeping the old config shape working. This is a hard break: the old top-level `github:` block and flat `allowed_users` list are replaced. The README gets updated.
- Network calls at startup (e.g. checking the gateway's key is published in its wallet's JWKS).
- Per-provider permissions. Every allowed user still sees every upstream.

## Anti-goals

- The gateway makes HTTP requests to arbitrary hosts a stranger typed into the sign-in page.
- A wallet user gets in without the wallet provider confirming they control the address (hash not checked, or `sub_ids` not checked).
- A wallet entry on the allowlist matches a GitHub login, or the other way round.
- Something that moves money or gets permission to move money.
- Open Payments secrets (the gateway's private key, grant continue tokens) show up in logs, the config file, or process listings.
- Existing GitHub users get silently locked out or lose their GitHub ID pin.

## Config (owner approved)

```yaml
providers:
  github:
    client_id: Ov23li...
    client_secret_env: GITHUB_CLIENT_SECRET
  open_payments:
    wallet_address: https://ilp.interledger-test.dev/mcp   # the gateway's own wallet address
    key_id: 1a2b...                                        # kid of the gateway's key in that wallet's JWKS
    private_key_env: OP_PRIVATE_KEY                        # env var holding the private key

allowed_users:
  github:
    - cozmin-github-login
  open_payments:
    - $ilp.interledger-test.dev/cozmin    # $ or https:// form, normalized at load
```

The owner already has the gateway's wallet address and key.

Validation (my calls):

- At least one of `providers.github`, `providers.open_payments`.
- `providers.github` keeps today's rules (client ID set, secret env var set and non-empty).
- `providers.open_payments`: `wallet_address` is an absolute `https://` URL (a `$` form is normalized first); `key_id` non-empty; `private_key_env` names a set env var whose value loads as an Ed25519 PKCS#8 private key (PEM text, or base64 of the PEM). Fail startup if it doesn't load. Don't let the value be interpreted as a file path (the SDK's `LoadKey` tries a path first; check the value looks like PEM or base64 before passing it, or parse it ourselves).
- `allowed_users.open_payments` entries are normalized (see "Wallet address normalization") and must be valid `https://` URLs. Duplicates after normalization are fine but pointless; builder's call whether to reject.
- An allowlist section for a provider that isn't configured is a startup error (almost certainly a mistake).
- Unknown fields are still rejected, so an old config fails with a parse error. A friendlier message pointing at the new shape would be nice; builder's call.

## User identity (owner approved)

Internally a user is a provider-prefixed string:

- `github:<login>`, login lowercased as today.
- `open_payments:<wallet address id>`, exactly as normalized (scheme and host lowercase, path case kept).

This string is what goes in `auth_requests`, `codes`, `tokens`, `consents`, `users`, `TokenInfo.UserID`, and the gateway's per-call log `user` field. The allowlist check becomes provider-aware: split on the first `:`, look in that provider's list. The current `norm()` lowercasing of the whole login must not be applied to wallet ids; wallet paths are compared exactly (my call; I don't know that every wallet provider treats paths case-insensitively).

Existing database (my call): migrate in place at startup so current GitHub users stay signed in. Rows whose login is non-empty and has no `:` get `github:` prepended, in every table that stores a login. Empty logins (pending auth requests) stay empty, or the "no user set yet" replay guard breaks. Idempotent. The `users` table keeps pinning GitHub IDs; it only ever holds `github:` rows for now. The auth request's `github_id` column stays (GitHub-only); builder decides whether to generalize it.

## Sign-in page (owner picked layout A)

Shown by `/authorize` after it validates and saves the auth request, instead of redirecting to GitHub. Same plain style as the existing consent page (`internal/oauth/templates.go`).

```
Sign in to mcp-dock
<Client name> wants to use the tools on this gateway. Sign in first.

[ Continue with GitHub ]
            or
Open Payments wallet address
[ $ilp.interledger-test.dev/alice    ]
[ Continue with wallet ]
```

- Only GitHub configured: skip the page, redirect to GitHub exactly like today (my call).
- Only Open Payments configured: show the page with just the wallet field (my call).
- The client name is attacker-controlled (set at dynamic registration). Render it through `html/template` like the consent page does, and the page gets the same headers (`writeHTML`: no framing, no-store, etc.).
- Both buttons submit a POST that carries the auth request ID (same pattern and CSRF reasoning as the consent form: SameSite=Lax cookie plus the 256-bit request ID). Exact paths are the builder's call but must sit under `/oauth/` so the auth cookie is sent.
- Bad wallet input (doesn't normalize, not https) or a failure fetching the wallet address: re-render the page with a short inline error, auth request stays alive so the user can retry (my call).
- Wallet address not on the allowlist: the same "Access denied" page GitHub misses get, auth request abandoned. Message names the provider and the address.

## GitHub flow

Unchanged apart from where it starts (the sign-in page button, or `/authorize` directly when it's the only provider). Callback stays at `/oauth/github/callback`, so the owner's existing GitHub OAuth App setting stays valid. The stored user becomes `github:<login>`.

## Open Payments flow (owner approved; details marked as my calls)

1. **Normalize the typed address.** Trim. Leading `$` becomes `https://`. Must parse as an absolute URL with scheme `https` (reject `http`; the Node repo accepts it, I don't). Lowercase scheme and host, keep the path. No query, fragment or userinfo (my call). Trailing slash, empty path (`$host`, which Open Payments maps to `/.well-known/pay`) and default port handling are the builder's call; the allowlist and the identity go through the same normalizer, so a wrong guess only causes a denial the admin can fix in config.
2. **Allowlist check before any network call.** If the normalized address isn't in `allowed_users.open_payments`, deny. This is what keeps the gateway from fetching stranger-supplied URLs: only addresses the admin listed are ever fetched.
3. **Fetch the wallet address** (unsigned GET). Take its `id` and `authServer`. Require `id` to also be on the allowlist after normalization (it should equal what was typed; if the wallet provider canonicalizes differently, the admin lists the canonical form). Require `authServer` to be `https`.
4. **Request a grant** at `authServer`, signed as the gateway's wallet (`providers.open_payments`). Body, matching the owner's Node repo:
   ```json
   {
     "subject": { "sub_ids": [{ "id": "<wallet id>", "format": "uri" }] },
     "interact": {
       "start": ["redirect"],
       "finish": { "method": "redirect", "uri": "<public_url>/oauth/open-payments/callback", "nonce": "<client nonce>" }
     }
   }
   ```
   No `access_token` section. The SDK adds `client` (the gateway's wallet address) itself. Client nonce: at least 24 random bytes, base64url. The response must be a pending interactive grant with `interact.redirect`, `interact.finish` (the AS nonce), `continue.uri` and `continue.access_token.value`; anything else is a failure.
5. **Save the pending state on the auth request** in SQLite (not in memory, unlike the Node repo): provider, wallet id, auth server URL, client nonce, AS finish nonce, continue URI, continue token. These live only as long as the auth request (10 min TTL, cleaned up by the existing expiry loop). Storing the continue token in plain form is acceptable to me: short-lived, scoped to one subject grant, and the DB already holds client registrations in plain form. Builder may hash or encrypt if it's easy, but the token has to be usable, so hashing doesn't work.
6. **Redirect** the browser to `interact.redirect`. The user signs in and approves at their wallet provider.
7. **Callback** at `GET /oauth/open-payments/callback`. The finish redirect doesn't carry our state, so the auth request is found from the auth cookie alone (my call; putting the request ID in the finish URI would leak the cookie secret to the wallet provider). Then, in order:
   - auth request exists, unexpired, is an Open Payments request with pending state, and has no user set yet (replay guard, same as the GitHub callback). Claim the pending state atomically (as `issueCode` already does for codes) so two concurrent callbacks can't both continue the grant;
   - `result` query param present means the user declined or it failed: abandon and send an error back to the client, like GitHub's `error` param. `grant_rejected` maps to `access_denied`, anything else to `server_error`;
   - `interact_ref` and `hash` both present;
   - hash check: `sha256(clientNonce + "\n" + finishNonce + "\n" + interact_ref + "\n" + authServerURL)`. Accept unpadded base64url or padded standard base64, as the Node repo does (the owner tested that against a live provider). Constant-time compare. `authServerURL` is exactly the `authServer` string from the wallet address response;
   - continue the grant (`interact_ref`, continue token);
   - the result must be a finalized grant whose `subject.sub_ids` contains an entry with `format: "uri"` and `id` equal to the raw `id` from the wallet address response (as the Node repo does). The normalized form of that `id` is what becomes the identity;
   - re-check the allowlist for `open_payments:<wallet id>` (config can't change at runtime, but this keeps the "checked at login" rule literal);
   - cancel the grant using the continue URI and token from the continue response (my call). Best effort: log a failure, don't fail the login.
   Any check failing after the hash step: abandon the auth request and show the error page. Don't reuse the pending state for a second try.
8. Set the auth request's user to `open_payments:<wallet id>` and continue into the existing consent / code path. The consent page reads "Signed in with Open Payments as `<wallet id>`" (and "Signed in with GitHub as `<login>`" for GitHub).

`Continue.Wait` is ignored, as in the Node repo (the redirect-finish flow means the user has already approved when the callback fires).

Outbound HTTP to wallet providers: same discipline as the GitHub client (15s timeout, capped response size, errors that never include tokens or bodies). Concretely (my calls):

- Inject our own `*http.Client` into both SDK clients (`WithHTTPClientUnauthed`, `WithHTTPClientAuthed`) with the 15s timeout and redirects disabled. The SDK default follows redirects, which would let an allowlisted wallet host bounce the gateway to any other host.
- The SDK's `OpenPaymentsClientError` embeds up to 4KB of response body and the URL. Wrap SDK errors before logging and keep only the status code and host.
- The SDK only takes the private key as a string and runs `LoadKey` on it, which tries a file path first. Check at config load that the value starts with `-----BEGIN` or is valid base64 of a PEM block before handing it to the SDK.

## `users reset` (my call)

`mcp-dock users reset <who>`:

- bare name, or `github:<login>`: a GitHub user (so today's usage keeps working);
- `$...` or `https://...`: a wallet user, normalized the same way as the allowlist.

Same effect as today: clear consents and tokens (and the GitHub ID pin for GitHub users).

## Stack (my calls)

- `github.com/interledger/open-payments-go` v0.5.0 for wallet address fetch, request signing (Ed25519 HTTP message signatures), and grant request/continue/cancel. The owner linked it. It's pre-1.0 and pulls in `github.com/oapi-codegen/runtime`, which I'm fine with. Pin the version.
- Finish-hash verification is our own code (the SDK has none).
- The GitHub client in `internal/github` stays as is.

## Left to the builder

- The provider abstraction: interface shape, where it lives, how `oauth.Server` holds the configured providers, and how provider-specific pending state is stored (new columns on `auth_requests`, a side table, or an encoded blob). Goal: a third provider shouldn't need changes in token, consent or MCP code.
- Exact paths for the sign-in page POST targets (under `/oauth/`).
- Exact wording of the sign-in page, inline errors and denial messages.
- A fake wallet + auth server in `internal/testutil` (like `fakegithub.go`) and e2e coverage for the Open Payments login, including bad hash, declined `result`, `sub_ids` mismatch, allowlist miss before fetch (assert no request was made), and replayed callback.
- README updates: new config shape, Open Payments setup (gateway wallet, key, `key_id`), allowlist format, `users reset` forms.
- Startup log line: count allowed users per provider.
