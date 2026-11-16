package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

const (
	aliceSub   = "https://ilp.interledger-test.dev/alice"
	mallorySub = "https://ilp.interledger-test.dev/mallory"
)

var (
	walletProvider = config.OIDCConfig{Name: "wallet", Label: "Open Payments wallet"}
	corpProvider   = config.OIDCConfig{Name: "corp", Label: "Corp SSO"}
)

// walletOnly is the common setup: one OIDC provider "wallet" with alice's
// wallet sub allowlisted.
func walletOnly() oidcTestOpts {
	return oidcTestOpts{
		Providers: []config.OIDCConfig{walletProvider},
		Allowed:   map[string][]string{"wallet": {aliceSub}},
	}
}

// oidcAuthorize runs /authorize where name is the only provider and returns
// the IdP authorize URL it redirects to. The request id is in the cookie.
func oidcAuthorize(t *testing.T, c *http.Client, ts *httptest.Server, fake *testutil.FakeOIDC, clientID string) string {
	t.Helper()
	resp, body := get(t, c, authorizeURL(ts, clientID, claudeRedirect, nil))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, fake.URL+"/authorize?") {
		t.Fatalf("authorize Location %q", loc)
	}
	return loc
}

// oidcStart presses the name button for reqID and returns the IdP authorize URL.
func oidcStart(t *testing.T, c *http.Client, ts *httptest.Server, fake *testutil.FakeOIDC, name, reqID string) string {
	t.Helper()
	resp, body := postForm(t, c, ts.URL+oidcStartPath(name), url.Values{"request": {reqID}})
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("oidc start: status %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, fake.URL+"/authorize?") {
		t.Fatalf("oidc start Location %q", loc)
	}
	return loc
}

// idpHop plays the IdP's authorize step and returns the callback URL it
// sends the browser to.
func idpHop(t *testing.T, ts *httptest.Server, name, authURL string) string {
	t.Helper()
	resp, body := get(t, newBrowser(t), authURL)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("fake idp: status %d: %s", resp.StatusCode, body)
	}
	cb := resp.Header.Get("Location")
	if !strings.HasPrefix(cb, ts.URL+oidcCallbackPath(name)+"?") {
		t.Fatalf("idp Location %q", cb)
	}
	return cb
}

// oidcCallbackURL runs /authorize (single OIDC provider) and the IdP hop.
func oidcCallbackURL(t *testing.T, c *http.Client, ts *httptest.Server, fake *testutil.FakeOIDC, name, clientID string) string {
	t.Helper()
	return idpHop(t, ts, name, oidcAuthorize(t, c, ts, fake, clientID))
}

func TestSigninShowsOIDCButtons(t *testing.T) {
	_, ts, _, _ := newTestServerOIDC(t, oidcTestOpts{
		GitHub:    true,
		Providers: []config.OIDCConfig{walletProvider, corpProvider},
		Allowed:   map[string][]string{"wallet": {aliceSub}},
	})
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, body := signinPage(t, c, ts, id)
	last := -1
	for _, want := range []string{
		`action="/oauth/github/start"`, "Continue with GitHub",
		`action="/oauth/oidc/wallet/start"`, "Continue with Open Payments wallet",
		`action="/oauth/oidc/corp/start"`, "Continue with Corp SSO",
	} {
		i := strings.Index(body, want)
		if i < 0 {
			t.Fatalf("sign-in page lacks %q: %s", want, body)
		}
		if i < last {
			t.Errorf("%q out of order: %s", want, body)
		}
		last = i
	}
	if n := strings.Count(body, `name="request" value="`+reqID+`"`); n != 3 {
		t.Errorf("%d forms carry the request field, want 3: %s", n, body)
	}
	if strings.Contains(body, `name="wallet"`) {
		t.Errorf("built-in wallet field shown: %s", body)
	}
}

func TestAuthorizeOIDCOnlySkipsPage(t *testing.T) {
	_, ts, fakes, _ := newTestServerOIDC(t, walletOnly())
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	loc, _ := url.Parse(oidcAuthorize(t, c, ts, fakes["wallet"], id))
	q := loc.Query()
	if q.Get("redirect_uri") != ts.URL+"/oauth/oidc/wallet/callback" {
		t.Errorf("redirect_uri %q", q.Get("redirect_uri"))
	}
	if q.Get("client_id") != "wallet-cid" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Errorf("query %v", q)
	}
	if reqID := jarAuthCookie(c, ts).Value; q.Get("state") == reqID {
		t.Error("IdP state is the auth request id")
	}
}

func TestOIDCLoginFlow(t *testing.T) {
	srv, ts, fakes, logs := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	cb := oidcCallbackURL(t, c, ts, fake, "wallet", id)
	resp, body := get(t, c, cb)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	checkHTMLHeaders(t, resp)
	if !strings.Contains(body, "Signed in with Open Payments wallet as <strong>"+aliceSub+"</strong>") {
		t.Fatalf("consent: %s", body)
	}
	resp, _ = postForm(t, c, ts.URL+consentPath, url.Values{"request": {consentRequestID(t, body)}, "action": {"approve"}})
	code := redirectQuery(t, resp, claudeRedirect).Get("code")
	if code == "" {
		t.Fatal("no code")
	}
	r := postToken(t, ts, codeForm(id, code, claudeRedirect))
	wantOK(t, r)
	info, err := srv.Verify(context.Background(), r.str("access_token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "oidc:wallet:"+aliceSub {
		t.Fatalf("user %q", info.UserID)
	}
	out := logs.String()
	for _, want := range []string{"oidc login started", "oidc login verified"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q: %s", want, out)
		}
	}
}

func TestOIDCCallbackOtherBrowser(t *testing.T) {
	_, ts, fakes, _ := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	cb := oidcCallbackURL(t, c, ts, fake, "wallet", id)

	resp, body := get(t, newBrowser(t), cb)
	checkHTMLError(t, resp, http.StatusBadRequest)
	if !strings.Contains(body, "Login link expired") {
		t.Fatalf("body: %s", body)
	}
	// A browser in its own flow (its own pending state) can't use it either,
	// and doing so spoils only its own attempt.
	other := newBrowser(t)
	oidcAuthorize(t, other, ts, fake, id)
	resp, _ = get(t, other, cb)
	checkHTMLError(t, resp, http.StatusBadRequest)

	resp, body = get(t, c, cb)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
		t.Fatalf("original browser: status %d: %s", resp.StatusCode, body)
	}
}

func TestOIDCCallbackReplay(t *testing.T) {
	_, ts, fakes, _ := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	cb := oidcCallbackURL(t, c, ts, fake, "wallet", id)
	reqID := jarAuthCookie(c, ts).Value
	resp, body := get(t, c, cb)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	resp, body = get(t, c, cb)
	checkHTMLError(t, resp, http.StatusBadRequest)
	if !strings.Contains(body, "Login link expired") {
		t.Fatalf("replay: %s", body)
	}
	// Starting again after login is a stale link.
	resp, _ = postForm(t, c, ts.URL+oidcStartPath("wallet"), url.Values{"request": {reqID}})
	checkHTMLError(t, resp, http.StatusBadRequest)
	// The replay didn't spoil the real consent.
	resp, _ = postForm(t, c, ts.URL+consentPath, url.Values{"request": {reqID}, "action": {"approve"}})
	if q := redirectQuery(t, resp, claudeRedirect); q.Get("code") == "" {
		t.Fatalf("query %v", q)
	}
}

func TestOIDCStateMismatch(t *testing.T) {
	srv, ts, fakes, _ := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	cb, _ := url.Parse(oidcCallbackURL(t, c, ts, fake, "wallet", id))
	reqID := jarAuthCookie(c, ts).Value
	q := cb.Query()
	q.Set("state", q.Get("state")+"x")
	cb.RawQuery = q.Encode()
	resp, body := get(t, c, cb.String())
	checkHTMLError(t, resp, http.StatusBadRequest)
	if !strings.Contains(body, "Login link expired") {
		t.Fatalf("body: %s", body)
	}
	authRequestGone(t, srv, reqID)
}

func TestOIDCResubmit(t *testing.T) {
	_, ts, fakes, _ := newTestServerOIDC(t, oidcTestOpts{
		GitHub:    true,
		Providers: []config.OIDCConfig{walletProvider},
		Allowed:   map[string][]string{"wallet": {aliceSub}},
	})
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	id := registerClient(t, ts, claudeRedirect)

	t.Run("second attempt completes", func(t *testing.T) {
		c := newBrowser(t)
		reqID, _ := signinPage(t, c, ts, id)
		first := idpHop(t, ts, "wallet", oidcStart(t, c, ts, fake, "wallet", reqID))
		second := idpHop(t, ts, "wallet", oidcStart(t, c, ts, fake, "wallet", reqID))
		if first == second {
			t.Fatal("both attempts got the same callback")
		}
		resp, body := get(t, c, second)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("stale attempt fails cleanly", func(t *testing.T) {
		c := newBrowser(t)
		reqID, _ := signinPage(t, c, ts, id)
		first := idpHop(t, ts, "wallet", oidcStart(t, c, ts, fake, "wallet", reqID))
		oidcStart(t, c, ts, fake, "wallet", reqID)
		resp, _ := get(t, c, first)
		checkHTMLError(t, resp, http.StatusBadRequest)
		if jarAuthCookie(c, ts) != nil {
			t.Error("cookie not cleared")
		}
	})
}

func TestOIDCCallbackWrongProvider(t *testing.T) {
	srv, ts, fakes, logs := newTestServerOIDC(t, oidcTestOpts{
		Providers: []config.OIDCConfig{walletProvider, corpProvider},
		Allowed:   map[string][]string{"wallet": {aliceSub}, "corp": {aliceSub}},
	})
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	cb, _ := url.Parse(idpHop(t, ts, "wallet", oidcStart(t, c, ts, fake, "wallet", reqID)))
	cb.Path = oidcCallbackPath("corp")
	resp, body := get(t, c, cb.String())
	checkHTMLError(t, resp, http.StatusInternalServerError)
	if strings.Contains(body, "approve") {
		t.Fatalf("body: %s", body)
	}
	authRequestGone(t, srv, reqID)
	if strings.Contains(logs.String(), "oidc login verified") {
		t.Fatalf("login verified: %s", logs.String())
	}
}

func TestOIDCIdPError(t *testing.T) {
	srv, ts, fakes, _ := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	id := registerClient(t, ts, claudeRedirect)
	for idpErr, want := range map[string]string{
		"access_denied":           "access_denied",
		"temporarily_unavailable": "server_error",
	} {
		t.Run(idpErr, func(t *testing.T) {
			fake.SetError(idpErr)
			t.Cleanup(func() { fake.SetError("") })
			c := newBrowser(t)
			cb := oidcCallbackURL(t, c, ts, fake, "wallet", id)
			reqID := jarAuthCookie(c, ts).Value
			resp, _ := get(t, c, cb)
			q := redirectQuery(t, resp, claudeRedirect)
			if q.Get("error") != want || q.Get("state") != testState || q.Get("code") != "" {
				t.Fatalf("query %v", q)
			}
			authRequestGone(t, srv, reqID)
		})
	}
}

func TestOIDCNotAllowed(t *testing.T) {
	srv, ts, fakes, _ := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	fake.SetSub(mallorySub)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	cb := oidcCallbackURL(t, c, ts, fake, "wallet", id)
	reqID := jarAuthCookie(c, ts).Value
	resp, body := get(t, c, cb)
	checkHTMLError(t, resp, http.StatusForbidden)
	if !strings.Contains(body, "Open Payments wallet user "+mallorySub) {
		t.Fatalf("body: %s", body)
	}
	authRequestGone(t, srv, reqID)
}

// sub "alice" at corp isn't sub "alice" at wallet.
func TestOIDCSameValueOtherProvider(t *testing.T) {
	_, ts, fakes, _ := newTestServerOIDC(t, oidcTestOpts{
		Providers: []config.OIDCConfig{walletProvider, corpProvider},
		Allowed:   map[string][]string{"wallet": {"alice"}},
	})
	fakes["wallet"].SetSub("alice")
	fakes["corp"].SetSub("alice")
	id := registerClient(t, ts, claudeRedirect)

	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	resp, body := get(t, c, idpHop(t, ts, "corp", oidcStart(t, c, ts, fakes["corp"], "corp", reqID)))
	checkHTMLError(t, resp, http.StatusForbidden)
	if !strings.Contains(body, "Corp SSO user alice") {
		t.Fatalf("body: %s", body)
	}

	c = newBrowser(t)
	reqID, _ = signinPage(t, c, ts, id)
	resp, body = get(t, c, idpHop(t, ts, "wallet", oidcStart(t, c, ts, fakes["wallet"], "wallet", reqID)))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
		t.Fatalf("wallet alice: status %d: %s", resp.StatusCode, body)
	}
}

func TestOIDCInvalidToken(t *testing.T) {
	srv, ts, fakes, logs := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	fake.SetClaims(map[string]any{"aud": "other"})
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	cb := oidcCallbackURL(t, c, ts, fake, "wallet", id)
	reqID := jarAuthCookie(c, ts).Value
	resp, body := get(t, c, cb)
	checkHTMLError(t, resp, http.StatusBadGateway)
	if !strings.Contains(body, "Open Payments wallet login failed") ||
		!strings.Contains(body, "Couldn&#39;t complete the Open Payments wallet login. Try again.") {
		t.Fatalf("body: %s", body)
	}
	if strings.Contains(body, "other") || strings.Contains(logs.String(), `"other"`) {
		t.Errorf("verify error text leaked: %s / %s", body, logs.String())
	}
	authRequestGone(t, srv, reqID)
}

func TestOIDCClaimMissing(t *testing.T) {
	p := walletProvider
	p.UserClaim = "wallet_address"
	srv, ts, fakes, _ := newTestServerOIDC(t, oidcTestOpts{
		Providers: []config.OIDCConfig{p},
		Allowed:   map[string][]string{"wallet": {aliceSub}},
	})
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	cb := oidcCallbackURL(t, c, ts, fakes["wallet"], "wallet", id)
	reqID := jarAuthCookie(c, ts).Value
	resp, body := get(t, c, cb)
	checkHTMLError(t, resp, http.StatusForbidden)
	if !strings.Contains(body, "Access denied") ||
		!strings.Contains(body, "Open Payments wallet didn&#39;t provide a verified account id.") {
		t.Fatalf("body: %s", body)
	}
	authRequestGone(t, srv, reqID)
}

func TestOIDCIdPDownAtStart(t *testing.T) {
	srv, ts, fakes, _ := newTestServerOIDC(t, oidcTestOpts{
		GitHub:    true,
		Providers: []config.OIDCConfig{walletProvider},
		Allowed:   map[string][]string{"wallet": {aliceSub}},
	})
	fake := fakes["wallet"]
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)

	fake.SetDown(true)
	resp, body := postForm(t, c, ts.URL+oidcStartPath("wallet"), url.Values{"request": {reqID}})
	checkHTMLError(t, resp, http.StatusBadGateway)
	if !strings.Contains(body, "Couldn&#39;t reach Open Payments wallet") {
		t.Fatalf("body: %s", body)
	}
	if _, err := srv.store.GetAuthRequest(context.Background(), reqID); err != nil {
		t.Fatalf("auth request dropped: %v", err)
	}

	fake.SetDown(false)
	oidcStart(t, c, ts, fake, "wallet", reqID)
}

func TestOIDCNoSecretsLogged(t *testing.T) {
	srv, ts, fakes, logs := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	authURL := oidcAuthorize(t, c, ts, fake, id)
	reqID := jarAuthCookie(c, ts).Value

	// Read the stored pending state and put it back unchanged.
	ctx := context.Background()
	raw, err := srv.store.ClaimAuthRequestPending(ctx, reqID, srv.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetAuthRequestPending(ctx, reqID, raw, srv.now()); err != nil {
		t.Fatal(err)
	}
	var pl pendingLogin
	if err := json.Unmarshal([]byte(raw), &pl); err != nil || pl.OIDC == nil {
		t.Fatalf("pending %v", err)
	}
	if pl.Provider != "oidc:wallet" {
		t.Fatalf("pending provider %q", pl.Provider)
	}

	cbURL := idpHop(t, ts, "wallet", authURL)
	cb, _ := url.Parse(cbURL)
	resp, body := get(t, c, cbURL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	resp, _ = postForm(t, c, ts.URL+consentPath, url.Values{"request": {reqID}, "action": {"approve"}})
	code := redirectQuery(t, resp, claudeRedirect).Get("code")

	secrets := map[string]string{
		"client secret": "wallet-secret",
		"idp code":      cb.Query().Get("code"),
		"state":         pl.OIDC.State,
		"nonce":         pl.OIDC.Nonce,
		"verifier":      pl.OIDC.Verifier,
		"request id":    reqID,
		"code":          code,
		"pending blob":  raw,
	}
	if la := fake.LastAuthorize(); la.Get("state") != pl.OIDC.State || la.Get("nonce") != pl.OIDC.Nonce {
		t.Fatal("captured pending state doesn't match the authorize request")
	}
	out := logs.String()
	if !strings.Contains(out, aliceSub) {
		t.Errorf("user not logged: %s", out)
	}
	for what, v := range secrets {
		if v == "" {
			t.Fatalf("%s not captured", what)
		}
		if strings.Contains(out, v) {
			t.Fatalf("log contains %s %q: %s", what, v, out)
		}
	}
	if strings.Contains(out, "eyJ") {
		t.Fatalf("log contains a JWT: %s", out)
	}
}
