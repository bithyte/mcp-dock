# Multiple Login Providers (GitHub + Open Payments) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let users sign in to mcp-dock with GitHub or with an Open Payments wallet address, picked on a sign-in page, with each provider optional in config.

**Architecture:** A small `identity` package defines provider-prefixed user ids (`github:<login>`, `open_payments:<wallet url>`) that replace bare logins everywhere in the store and token path. Config moves to `providers.*` and per-provider `allowed_users.*`. A new `oplogin` package wraps open-payments-go (wallet fetch, subject grant, continue, cancel, finish-hash check, hardened HTTP). `oauth.Server` gets a sign-in page and per-provider start/callback routes; provider-specific pending state lives in one opaque `pending` column on `auth_requests`, claimed atomically by the callback.

**Tech Stack:** Go 1.27, `github.com/interledger/open-payments-go` v0.5.0 (new), existing `modelcontextprotocol/go-sdk`, `modernc.org/sqlite`, `go.yaml.in/yaml/v3`, stdlib `net/http`, `html/template`.

**Spec:** `docs/superpowers/specs/2026-11-12-oauth-providers-design.md` (builds on `docs/superpowers/specs/2026-11-10-mcp-dock-design.md`)

## Global Constraints

- User ids: `github:<login lowercased>` and `open_payments:<normalized wallet address>`. Wallet paths keep their case. Split on the first `:`.
- Config shape (hard break, old shape rejected):
  `providers.github.{client_id, client_secret_env}`, `providers.open_payments.{wallet_address, key_id, private_key_env}`, `allowed_users.{github, open_payments}`. At least one provider.
- Open Payments private key: Ed25519 PKCS#8, given as PEM text or base64 of the PEM, never a file path.
- Wallet addresses: `$` becomes `https://`; only `https`; no query, fragment or userinfo; scheme and host lowercase; path case kept.
- Allowlist checked before any network call to a wallet host, again on the wallet's `id`, again after the grant is finalized, and (as today) at token issue, refresh and every MCP request.
- Subject-only grant. Never request `access_token`. Never move money.
- Finish hash: `sha256(clientNonce + "\n" + finishNonce + "\n" + interact_ref + "\n" + authServer)`, accepted as unpadded base64url or padded standard base64, constant-time compare.
- Outbound wallet HTTP: 15s timeout, redirects not followed, response bodies capped at 1 MiB, errors carry only method, host and status.
- Never log the private key, continue tokens, interact refs, hashes, nonces or gateway codes/tokens.
- GitHub callback path stays `/oauth/github/callback`. New paths: `POST /oauth/github/start`, `POST /oauth/open-payments/start`, `GET /oauth/open-payments/callback`. All under `/oauth/` (auth cookie path).
- All HTML via `html/template` and `writeHTML` (no framing, no-store).
- No em-dashes in code, comments, docs or commit messages.
- Commit messages follow the repo's `type: lowercase summary` style.

## Review Focus

- Wallet provider returns an `id` that differs only in host case or default port from what the user typed: login should still work (both normalize the same). Test in Task 6.
- User submits the wallet form, comes back with the browser's back button and submits again: the newest attempt should finish fine, the stale callback should fail cleanly. Test in Task 6.
- Wallet host hangs: the user should get an error page within the timeout, not a hung request. Test in Task 4.
- Wallet callback opened in a different browser (no cookie): expired-link page, and the original browser's attempt is not consumed. Test in Task 6.
- Owner restarts with the old config file: startup fails with a message that says where `github` and `allowed_users` moved. Test in Task 3.

---

### Task 1: `identity` package

**Files:**
- Create: `internal/identity/identity.go`
- Test: `internal/identity/identity_test.go`

**Interfaces:**
- Produces:
  - `type Provider string`; `const GitHub Provider = "github"`; `const OpenPayments Provider = "open_payments"`
  - `func GitHubUser(login string) string` returns `"github:" + strings.ToLower(login)`
  - `func WalletUser(normalizedWallet string) string` returns `"open_payments:" + normalizedWallet` (no further normalization)
  - `func Split(id string) (p Provider, name string, ok bool)`: ok is false for no `:`, empty name, or unknown provider
  - `func NormalizeWalletAddress(s string) (string, error)`
  - `func Describe(id string) string`: `"GitHub account alice"`, `"Open Payments wallet address https://..."`; unknown id returns the id
  - `func Label(p Provider) string`: `"GitHub"`, `"Open Payments"`
  - `func ParseCLIUser(arg string) (string, error)`: bare name or `github:<login>` gives `GitHubUser`; `$...` or `https://...` gives `WalletUser(NormalizeWalletAddress(arg))`; `open_payments:<url>` likewise; anything else (empty, `http://`, other prefix) is an error

- [ ] **Step 1: Write failing tests**

```go
func TestNormalizeWalletAddress(t *testing.T) {
	ok := map[string]string{
		"$ilp.interledger-test.dev/alice":           "https://ilp.interledger-test.dev/alice",
		"  https://ILP.Interledger-Test.dev/Alice ": "https://ilp.interledger-test.dev/Alice",
		"https://ilp.interledger-test.dev:443/a":     "https://ilp.interledger-test.dev/a",
		"https://w.example/a/":                       "https://w.example/a",
		"$w.example":                                 "https://w.example/.well-known/pay",
		"https://w.example/":                         "https://w.example/.well-known/pay",
		"https://127.0.0.1:8443/alice":               "https://127.0.0.1:8443/alice",
	}
	for in, want := range ok { /* got, err := NormalizeWalletAddress(in); err == nil && got == want */ }
	bad := []string{"", "alice", "http://w.example/a", "https://w.example/a?x=1", "https://w.example/a?",
		"https://w.example/a#f", "https://w.example/a#", "https://u:p@w.example/a", "https:///a", "ftp://w.example/a", "$"}
	for _, in := range bad { /* err != nil */ }
}

func TestSplitAndUsers(t *testing.T) {
	// GitHubUser("Alice") == "github:alice"
	// WalletUser("https://w.example/Al") == "open_payments:https://w.example/Al"
	// Split("open_payments:https://w.example/a") == (OpenPayments, "https://w.example/a", true)
	// Split("alice"), Split("github:"), Split("google:x") all ok == false
}

func TestParseCLIUser(t *testing.T) {
	// "Alice" -> "github:alice"; "github:Alice" -> "github:alice"
	// "$w.example/Al" -> "open_payments:https://w.example/Al"
	// "open_payments:$w.example/Al" -> same
	// "", "http://w.example/a", "google:x" -> error
}

func TestDescribe(t *testing.T) {
	// Describe("github:alice") == "GitHub account alice"
	// Describe("open_payments:https://w.example/a") == "Open Payments wallet address https://w.example/a"
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/identity/`
Expected: build failure, package has no non-test files.

- [ ] **Step 3: Implement.** Use `url.Parse`; reject `ForceQuery`, a `?` or `#` anywhere in the trimmed input, `User != nil`, empty host, `Opaque`. Strip `:443`. Empty path or `/` becomes `/.well-known/pay` (Open Payments payment-pointer rule). Strip one trailing `/` otherwise. Return `scheme://host + EscapedPath()`.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./internal/identity/`

- [ ] **Step 5: Commit**

```bash
git add internal/identity
git commit -m "feat: add identity package for provider-prefixed user ids"
```

---

### Task 2: GitHub users become `github:<login>` everywhere

Still on the old config shape. Only the stored identity changes.

**Files:**
- Modify: `internal/store/store.go` (migration in `Open`; drop `norm`)
- Modify: `internal/store/oauth.go` (all `norm(...)` calls removed; parameter names `login` become `userID`)
- Modify: `internal/config/config.go` (`IsAllowed`)
- Modify: `internal/oauth/authorize.go`, `internal/oauth/templates.go`
- Modify: `cmd/mcp-dock/main.go` (`resetUser`)
- Test: `internal/store/store_test.go`, `internal/oauth/authorize_test.go`, `internal/oauth/token_test.go`, `cmd/mcp-dock/main_test.go`, `internal/e2e/e2e_test.go`

**Interfaces:**
- Consumes: Task 1 `identity.GitHubUser`, `identity.Split`, `identity.Describe`, `identity.Label`, `identity.ParseCLIUser`
- Produces:
  - Store methods take already-normalized user ids and store them verbatim: `SetAuthRequestUser(ctx, id, userID string, githubID int64)`, `HasConsent(ctx, userID, clientID, redirectURI)`, `SaveConsent(...)`, `GitHubID(ctx, userID)`, `SetGitHubID(ctx, userID, id)`, `ResetUser(ctx, userID)`. `AuthRequest.Login`, `Code.Login`, `Token.Login` hold user ids.
  - `func (c *Config) IsAllowed(userID string) bool` (in this task: GitHub provider only, `EqualFold` against `AllowedUsers`; Task 3 replaces the body)
  - `consentData{ClientName, Redirect, Provider, Name, RequestID string}`
  - `func (s *Server) notAllowed(w http.ResponseWriter, userID string)` renders `identity.Describe(userID) + " isn't allowed on this gateway."`
  - `func (s *Server) afterLogin(w http.ResponseWriter, r *http.Request, ar *store.AuthRequest, userID string, githubID int64)`: the shared tail of the GitHub callback from `SetAuthRequestUser` on (consent lookup, consent page or `issueCode`). Task 6 reuses it.

- [ ] **Step 1: Write failing tests**
  - `TestMigrateBareLogins` (store): create a DB with `Open`, insert via `s.db` rows with login `Alice`/`alice` into `users`, `consents`, `tokens`, `codes`, and an `auth_requests` row with `login=''`; close; `Open` again. Assert logins are `github:alice`, the empty login stays empty, and a third `Open` changes nothing (idempotent).
  - Store tests that pass bare logins now pass `identity.GitHubUser(...)`; add `TestStoreKeepsWalletCase`: `SaveConsent(ctx, "open_payments:https://w.example/Al", ...)` then `HasConsent` with `.../al` is false.
  - `TestCallbackStoresPrefixedUser` (oauth): after GitHub login of `Alice`, the issued code's token verifies to `TokenInfo.UserID == "github:alice"`.
  - `TestConsentNamesProvider`: consent page body contains `Signed in with GitHub as <strong>Alice</strong>` (display name keeps GitHub's casing, as today).
  - `TestCallbackNotAllowed` body contains `GitHub account`.
  - `TestUsersReset` (cmd): seed `SetGitHubID(ctx, "github:alice", 42)`; `users reset Alice` clears it and stdout names `github:alice`; `users reset '$w.example/Al'` succeeds and stdout names `open_payments:https://w.example/Al`; `users reset http://x` is a usage error.
  - e2e: audit `user` assertions become `github:alice` / `github:friend`.

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./...`
Expected: new tests fail (bare logins stored, page text old).

- [ ] **Step 3: Implement**
  - Migration in `Open` after the schema: for each of `auth_requests, codes, tokens, consents, users`: `UPDATE <t> SET login = 'github:' || lower(login) WHERE login <> '' AND instr(login, ':') = 0`, in one transaction.
  - GitHub callback: `uid := identity.GitHubUser(user.Login)` used for `IsAllowed`, `GitHubID`, `SetGitHubID`, then `s.afterLogin(w, r, ar, uid, user.ID)`. Consent page `Name` is `user.Login`; for the consent-remembered path nothing renders. Log field `user` holds `uid`.
  - `handleConsent` and `token.go` already pass `ar.Login`/`c.Login`/`t.Login` to `IsAllowed`; no change beyond the new semantics.
  - Consent template line: `<p>Signed in with {{.Provider}} as <strong>{{.Name}}</strong>.</p>`.
  - `resetUser` uses `identity.ParseCLIUser`; output `reset <uid>: cleared consents and tokens` plus `and stored GitHub ID` for GitHub users. Update the usage text to `<login | wallet address>`.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./...`

- [ ] **Step 5: Commit**

```bash
git add -A internal cmd
git commit -m "refactor: store users as provider-prefixed ids and migrate existing github logins"
```

---

### Task 3: Config: `providers.*` and per-provider `allowed_users`

**Files:**
- Modify: `internal/config/config.go`, `config.example.yaml`
- Modify: `internal/app/app.go` (GitHub client only when configured), `internal/oauth/server.go`, `internal/oauth/authorize.go` (nil GitHub client), `cmd/mcp-dock/main.go` (startup log)
- Create: `internal/testutil/keys.go`
- Test: `internal/config/config_test.go`; fix configs in `internal/oauth/helpers_test.go`, `internal/e2e/e2e_test.go`, `cmd/mcp-dock/main_test.go`

**Interfaces:**
- Consumes: Task 1 `identity.NormalizeWalletAddress`, `identity.Split`
- Produces:

```go
type Config struct {
	PublicURL    string           `yaml:"public_url"`
	Listen       string           `yaml:"listen"`
	DataDir      string           `yaml:"data_dir"`
	Providers    ProvidersConfig  `yaml:"providers"`
	AllowedUsers AllowedUsers     `yaml:"allowed_users"`
	Upstreams    []UpstreamConfig `yaml:"upstreams"`
}
type ProvidersConfig struct {
	GitHub       *GitHubConfig       `yaml:"github"`
	OpenPayments *OpenPaymentsConfig `yaml:"open_payments"`
}
type OpenPaymentsConfig struct {
	WalletAddress string `yaml:"wallet_address"` // normalized at load
	KeyID         string `yaml:"key_id"`
	PrivateKeyEnv string `yaml:"private_key_env"`
	PrivateKey    string `yaml:"-"` // env value, checked to be PEM or base64 PEM of an Ed25519 PKCS#8 key
}
type AllowedUsers struct {
	GitHub       []string `yaml:"github"`
	OpenPayments []string `yaml:"open_payments"` // normalized at load
}
func (c *Config) IsAllowed(userID string) bool // github: EqualFold; open_payments: exact match on normalized entries
```
  - `func Ed25519PEM(t testing.TB) (pemText string, pub ed25519.PublicKey)` in `testutil`: fresh key, `x509.MarshalPKCS8PrivateKey`, `pem.EncodeToMemory` with type `PRIVATE KEY`.
  - `oauth.Options.GitHub` may be nil.

- [ ] **Step 1: Write failing tests** (config)
  - `validYAML` and `config.example.yaml` move to the new shape (example uses `wallet_address: https://ilp.interledger-test.dev/mcp`, `private_key_env: OP_PRIVATE_KEY`, and a `$ilp.interledger-test.dev/alice` allowlist entry). `goodEnv` gains `OP_PRIVATE_KEY` from `testutil.Ed25519PEM`.
  - `TestLoadValid`: both providers load; `c.AllowedUsers.OpenPayments[0] == "https://ilp.interledger-test.dev/alice"`; GitHub-only and Open-Payments-only configs load.
  - `TestLoadRejects` new rows (error must mention the field): no providers (`providers`); `open_payments.wallet_address` http or empty (`wallet_address`); empty `key_id` (`key_id`); unset key env (`private_key_env`); key env holding a file path to a real PEM file (`private_key_env`); key env holding an RSA PKCS#8 PEM (`private_key_env`); base64 garbage (`private_key_env`); `allowed_users.open_payments` entry `http://w.example/a` (`allowed_users.open_payments`); `allowed_users.github` set while `providers.github` absent (`allowed_users.github`).
  - `TestLoadAcceptsBase64Key`: base64 of the PEM loads.
  - `TestOldConfigShape`: YAML with top-level `github:` fails with an error containing `providers.github`; YAML with `allowed_users: [alice]` (a list) fails with an error containing `allowed_users.github`.
  - `TestIsAllowed`: `github:ALICE` allowed for entry `Alice`; `open_payments:https://ilp.interledger-test.dev/alice` allowed; `.../Alice` not; `github:https://ilp.interledger-test.dev/alice` not; `open_payments:alice` not; `alice` (no prefix) not.

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/config/`

- [ ] **Step 3: Implement**
  - Old-shape detection: before the strict decode, decode into `map[string]any`; top-level `github` key, or `allowed_users` being a list, returns the targeted error (`github moved to providers.github`, `allowed_users is now per provider, e.g. allowed_users.github: [...]`).
  - Key check in config with `crypto/x509` + `encoding/pem` (no SDK import): trimmed value starting with `-----BEGIN` is PEM; otherwise `base64.StdEncoding` decode must give PEM; the block must parse as PKCS#8 holding `ed25519.PrivateKey`. Store the original env value in `PrivateKey`. Error text never includes the value.
  - `app.Build`: build `*github.Client` only when `cfg.Providers.GitHub != nil`. `oauth.Server.Routes` registers the GitHub callback only when `gh != nil`. Until Task 6, `handleAuthorize` with `gh == nil` returns `serverError("no login provider", ...)`.
  - Startup log: replace `allowed_users` with `allowed_github` and `allowed_open_payments` counts.
  - Fix the three test config builders to the new types.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./...`

- [ ] **Step 5: Commit**

```bash
git add -A internal cmd config.example.yaml
git commit -m "feat: move login config under providers with per-provider allowlists"
```

---

### Task 4: `oplogin` package and fake wallet

**Files:**
- Create: `internal/oplogin/oplogin.go`, `internal/oplogin/hash.go`, `internal/oplogin/http.go`
- Create: `internal/testutil/fakeop.go`
- Test: `internal/oplogin/oplogin_test.go`, `internal/oplogin/hash_test.go`
- Modify: `go.mod`, `go.sum` (`go get github.com/interledger/open-payments-go@v0.5.0`)

**Interfaces:**
- Consumes: SDK `openpayments.NewAuthenticatedClient(wallet, key, keyID, openpayments.WithHTTPClientAuthed(hc))`; its `WalletAddress.Get`, `Grant.Request`, `Grant.Continue`, `Grant.Cancel`; `as.GrantRequest.FromGrantRequestWithSubject`. One authenticated client covers the unsigned wallet GET too (it uses the injected client).
- Produces:

```go
type Config struct {
	WalletAddress, KeyID, PrivateKey string // from config.OpenPaymentsConfig
	HTTP   *http.Client  // nil: default (15s timeout). Redirects are disabled and bodies capped either way.
	Logger *slog.Logger
}
type Wallet struct{ ID, AuthServer string } // raw values from the wallet document
type Pending struct {
	WalletID, AuthServer, ClientNonce, FinishNonce, ContinueURI, ContinueToken, RedirectURL string
}
var ErrBadHash, ErrSubjectMismatch, ErrNotPending error
func New(cfg Config) (*Client, error)
func (c *Client) Wallet(ctx context.Context, normalizedURL string) (Wallet, error) // ID and AuthServer required; AuthServer must be https
func (c *Client) Start(ctx context.Context, w Wallet, finishURI string) (Pending, error)
func (c *Client) Finish(ctx context.Context, p Pending, interactRef, hash string) error
func VerifyHash(clientNonce, finishNonce, interactRef, authServer, hash string) bool
```
  - `FakeOpenPayments` in testutil, a `httptest.NewTLSServer`:
    - `NewFakeOpenPayments(t, gatewayWallet string, gatewayPub ed25519.PublicKey) *FakeOpenPayments`
    - `Wallet(name string) string` (URL `<srv>/<name>`; its document has `id` = that URL, `authServer` = `<srv>/auth`)
    - knobs: `SetDecline(bool)`, `SetBadHash(bool)`, `SetSubject(id string)` (override `sub_ids` id), `SetWalletID(name, id string)`, `SetDelay(time.Duration)`
    - `Requests() []string` (`"METHOD /path"` log), `Cancels() int`
    - `POST /auth` checks: `client` equals `gatewayWallet`; no `access_token` in body; `subject.sub_ids[0]` is `{id: wallet id, format: uri}`; `interact.finish.method == "redirect"`; request carries `Signature` and `Signature-Input` and the signature verifies with `gatewayPub` (`httpsignatureutils.ValidateSignature` after stripping the `sig1=:`...`:` wrapper). Responds with `interact.redirect = <srv>/interact/<id>`, `interact.finish = <as nonce>`, `continue.uri = <srv>/continue/<id>`, `continue.access_token.value`.
    - `GET /interact/<id>`: 302 to the finish URI with `interact_ref` and `hash` (unpadded base64url), or `result=grant_rejected` when declined.
    - `POST /continue/<id>`: checks `Authorization: GNAP <token>` and `interact_ref`; returns `subject.sub_ids` and a fresh `continue`.
    - `DELETE /continue/<id>`: 204, counts a cancel.

- [ ] **Step 1: Write failing tests**
  - `TestVerifyHash`: build the digest with stdlib in the test; both `RawURLEncoding` and `StdEncoding` forms pass; changing any of the four inputs or one hash char fails; empty hash fails.
  - `TestLoginHappyPath`: `Wallet` then `Start` then follow `RedirectURL` with the fake's client (no redirect follow) to read `interact_ref`/`hash` from `Location`, then `Finish` returns nil and `Cancels() == 1`.
  - `TestStartRequestShape`: covered by the fake's checks (the fake fails the request with 400 on any mismatch, so the happy path proves it); add `TestStartNeverRequestsAccessToken` that reads the captured body and asserts no `access_token` key.
  - `TestFinishBadHash` (`errors.Is(err, ErrBadHash)`, and no `/continue` request was made), `TestFinishSubjectMismatch` (`ErrSubjectMismatch`), `TestFinishCancelFailureIgnored` (fake returns 500 on DELETE; `Finish` still nil, warning logged).
  - `TestWalletRejectsHTTPAuthServer`, `TestWalletMissingID`.
  - `TestNoRedirectFollow`: fake wallet path answers 302 to another host; `Wallet` errors and the other host saw no request.
  - `TestBodyCap`: wallet document larger than 1 MiB errors.
  - `TestTimeout`: `SetDelay(2s)`, `Config.HTTP` with `Timeout: 200ms`; `Wallet` returns an error in under 1s.
  - `TestErrorsSanitized`: a 400 from the AS with a body containing `secret-body` gives an error whose text has the host and `400` and not `secret-body`; continue token never in error text.

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/oplogin/`

- [ ] **Step 3: Implement**
  - `http.go`: `hardened(base *http.Client) *http.Client` copies base (or a 15s-timeout default), sets `CheckRedirect` to return `http.ErrUseLastResponse`, wraps `Transport` so `resp.Body` errors past 1 MiB.
  - `sanitize(err error) error`: `*openpayments.OpenPaymentsClientError` becomes `open payments <method> <host>: status <n>`; `*url.Error` keeps only the cause; anything else becomes a fixed message plus `%w` only for context errors.
  - Client nonce: 24 bytes `crypto/rand`, base64url. Grant body built with `FromGrantRequestWithSubject` (subject `sub_ids: [{id: w.ID, format: uri}]`, interact start `redirect`, finish redirect with nonce and URI). Response must have `Interact.Redirect`, `Interact.Finish`, `Continue.Uri`, `Continue.AccessToken.Value`; otherwise `ErrNotPending`.
  - `Finish`: `VerifyHash` first; `Grant.Continue`; require `Subject` with a `uri` entry equal to `p.WalletID`; then `Grant.Cancel` with the continue response's `Continue` (log `Warn` on failure, return nil). `Continue.Wait` ignored.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./internal/oplogin/ && go vet ./...`

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/oplogin internal/testutil/fakeop.go
git commit -m "feat: add open payments wallet login client"
```

---

### Task 5: Store: pending provider state on auth requests

**Files:**
- Modify: `internal/store/store.go` (column migration), `internal/store/oauth.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces:
  - `auth_requests.pending TEXT NOT NULL DEFAULT ''`; added with `ALTER TABLE` when `pragma table_info(auth_requests)` lacks it.
  - `func (s *Store) SetAuthRequestPending(ctx context.Context, id, pending string) error`: only where `login = ''`; overwrites any earlier pending; `ErrNotFound` when no row matched.
  - `func (s *Store) ClaimAuthRequestPending(ctx context.Context, id string, now time.Time) (string, error)`: `UPDATE auth_requests SET pending='' WHERE id=? AND pending<>'' AND login='' AND expires_at>? RETURNING pending`; `ErrNotFound` otherwise.
  - The blob is opaque to the store (oauth stores JSON of `oplogin.Pending`).

- [ ] **Step 1: Write failing tests**
  - `TestPendingClaimOnce`: set, claim returns the value, second claim `ErrNotFound`.
  - `TestPendingClaimConcurrent`: 10 goroutines claim; exactly one gets the value.
  - `TestPendingOverwrite`: set `a`, set `b`, claim returns `b`.
  - `TestPendingRejectedAfterLogin`: `SetAuthRequestUser` then set and claim both `ErrNotFound`.
  - `TestPendingExpired`: claim with `now` past `ExpiresAt` is `ErrNotFound`.
  - `TestPendingColumnMigration`: create an `auth_requests` table without `pending` via raw SQL in a fresh file, `Open` it, set/claim works.

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/store/`

- [ ] **Step 3: Implement** as specified above.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./internal/store/`

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat: store and atomically claim pending login state on auth requests"
```

---

### Task 6: Sign-in page and Open Payments routes

**Files:**
- Modify: `internal/oauth/server.go` (Options, routes), `internal/oauth/authorize.go`, `internal/oauth/templates.go`, `internal/app/app.go`
- Create: `internal/oauth/signin.go` (sign-in page + both start handlers + OP callback)
- Test: `internal/oauth/signin_test.go`, `internal/oauth/helpers_test.go`

**Interfaces:**
- Consumes: Task 2 `afterLogin`, `notAllowed`; Task 3 config; Task 4 `oplogin.Client`, `Pending`, errors; Task 5 `SetAuthRequestPending`, `ClaimAuthRequestPending`; `testutil.FakeOpenPayments`
- Produces:
  - `oauth.Options.OpenPayments *oplogin.Client` (nil when not configured). `app.Options.OpenPaymentsHTTP *http.Client` test override passed to `oplogin.Config.HTTP`; `app.Build` constructs `oplogin.New` from `cfg.Providers.OpenPayments` and fails loudly (panic is not OK; return the error from a new `app.Build` signature `(*App, error)` and update both callers).
  - `signinData{ClientName, RequestID, Wallet, Error string; GitHub, OpenPayments bool}`
  - Test helper `newTestServerOP(t, allowGitHub, allowWallets []string, withGitHub bool) (*Server, *httptest.Server, *testutil.FakeGitHub, *testutil.FakeOpenPayments)`

Behavior (copy for the page in the spec, layout A):
- `/authorize`: after save + cookie, `op == nil` redirects to GitHub (today's behavior, existing tests keep passing); otherwise render the sign-in page. GitHub button shown only when `gh != nil`.
- `POST /oauth/github/start` (form `request`): `boundRequest`; `ar.Login != ""` is `badLink`; redirect to `gh.AuthorizeURL(ar.ID, ...)`.
- `POST /oauth/open-payments/start` (form `request`, `wallet`): `boundRequest`; `ar.Login != ""` is `badLink`; `NormalizeWalletAddress` error re-renders with `Error: "Enter a wallet address like $wallet.example/alice."` (400); not on allowlist: log warn with the address, `abandon`, `notAllowed` (no outbound request); `op.Wallet` error re-renders `"Couldn't reach that wallet address. Check it and try again."` (502); normalized `w.ID` not allowed: `abandon`, `notAllowed`; `op.Start` error re-renders the same 502 message; `SetAuthRequestPending(json)`; `redirect` to `p.RedirectURL`. Re-renders keep the typed `Wallet` value.
- `GET /oauth/open-payments/callback`: request id comes from the auth cookie only (`boundRequest(w, r, cookieValue)`); `ClaimAuthRequestPending` (`ErrNotFound` is `badLink`); from here every failure abandons the request. `result` present: `redirectError` with `access_denied` for `grant_rejected`, else `server_error`. Missing `interact_ref` or `hash`: `badLink`. `op.Finish`: `ErrBadHash`/`ErrSubjectMismatch` show `errorPage(400, "Wallet login failed", "Couldn't confirm you own that wallet address. Start again from your app.")`, other errors the same title with 502 and `"Couldn't complete the wallet login. Try again."`. Then `uid := identity.WalletUser(normalized p.WalletID)`, `IsAllowed(uid)` recheck, `afterLogin(w, r, ar, uid, 0)`. Consent `Name` is the normalized wallet address.

- [ ] **Step 1: Write failing tests**
  - `TestAuthorizeShowsSigninPage`: both providers; 200, `checkHTMLHeaders`, body has both buttons, `name="request"`, and the escaped client name.
  - `TestAuthorizeGitHubOnlySkipsPage`: existing `TestAuthorizeRedirectsToGitHub` still passes unchanged.
  - `TestSigninOpenPaymentsOnly`: no GitHub button, wallet field present; `POST /oauth/github/start` is 404/405.
  - `TestGitHubStart`: button POST leads through fake GitHub to consent.
  - `TestWalletLoginFlow`: wallet allowed; start, follow fake interact redirect, callback, consent shows `Signed in with Open Payments as` + wallet URL; approve; token verifies to `open_payments:<wallet>`; `Cancels() == 1`.
  - `TestWalletNotAllowedMakesNoRequest`: unlisted address is 403 `Open Payments wallet address`, and `fake.Requests()` is empty.
  - `TestWalletBadInput`: `http://...` re-renders 400 with the error and keeps the typed value; the auth request still works for a following valid submit.
  - `TestWalletIDCanonicalizesSame` (Review Focus): `SetWalletID("alice", <the wallet URL with its host uppercased>)`; login succeeds and the user id uses the lowercase host.
  - `TestWalletIDDiffers`: `SetWalletID` to an unlisted URL gives 403 and no `/auth` request.
  - `TestWalletDeclined`: `SetDecline(true)`; callback redirects to the client with `error=access_denied`; auth request gone.
  - `TestWalletBadHash`: `SetBadHash(true)`; 400 page; no code issued; a second callback hit is the expired-link page.
  - `TestWalletCallbackOtherBrowser` (Review Focus): replay the callback URL from a fresh browser: `badLink`; the original browser then completes successfully.
  - `TestWalletResubmit` (Review Focus): submit the wallet form twice in the same browser, then complete only the second attempt: success. Separately: submit twice, hit the first attempt's callback: 400 wallet-login-failed page (its nonces were overwritten), the request is abandoned, and a fresh `/authorize` works.
  - `TestWalletCallbackReplay`: after a successful login reaches consent, hitting the callback again is `badLink`.
  - `TestWalletNoSecretsLogged`: logs contain none of the continue token, interact ref, hash or client nonce (capture them from the fake).

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/oauth/`

- [ ] **Step 3: Implement** per the behavior list. Sign-in template in `templates.go`, styled with `pageStyle`, structure from the spec's layout A, hidden `request` input on both forms.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./... && go vet ./...`

- [ ] **Step 5: Commit**

```bash
git add -A internal
git commit -m "feat: add sign-in page and open payments wallet login"
```

---

### Task 7: End to end, README

**Files:**
- Modify: `internal/e2e/e2e_test.go`, `README.md`

**Interfaces:**
- Consumes: everything above; `app.Options.OpenPaymentsHTTP` set to `fakeOP.Client()`.

- [ ] **Step 1: Write failing tests**
  - `newEnv` configures both providers (gateway wallet from the fake, key from `testutil.Ed25519PEM`), allowlists `alice`, `friend` and the fake's `alice` wallet.
  - `e.login` gains a provider switch: GitHub path goes through the sign-in page's GitHub button; add `e.walletLogin(t, name string) string`.
  - `TestEndToEndWallet`: wallet login, connect, `fake__echo` works, audit record `user` is `open_payments:<wallet url>`.
  - `TestEndToEndSameNameDifferentUsers`: GitHub `alice` and wallet `alice` both log in; audit shows two distinct users; `users reset`-equivalent `st.ResetUser(ctx, walletUID)` kills only the wallet session's tokens (next MCP call with the wallet token is 401, GitHub token still works).

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/e2e/`

- [ ] **Step 3: Update e2e helpers and README**
  - README: intro mentions both providers; "GitHub OAuth App setup" unchanged except config keys; new "Open Payments setup" (gateway wallet address, generating an Ed25519 key in the wallet's developer keys, `key_id`, `OP_PRIVATE_KEY` holds PEM or base64 PEM, not a path; the grant is subject-only, no money moves); config section rewritten for `providers.*` and `allowed_users.*` incl. `$` normalization and case rules; env vars list adds `OP_PRIVATE_KEY`; `users reset <login | wallet address>`.

- [ ] **Step 4: Run, expect PASS**

Run: `go test -race ./... && go vet ./... && go build ./cmd/mcp-dock`

- [ ] **Step 5: Commit**

```bash
git add internal/e2e README.md
git commit -m "test: end to end wallet login; docs: open payments setup and new config"
```
