package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

const wantGHStart = "/oauth/github/start"

// signinPage runs /authorize, expects the sign-in page and returns its
// request id and body.
func signinPage(t *testing.T, c *http.Client, ts *httptest.Server, clientID string) (string, string) {
	t.Helper()
	resp, body := get(t, c, authorizeURL(ts, clientID, claudeRedirect, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize: status %d Location %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	return consentRequestID(t, body), body
}

func authRequestGone(t *testing.T, srv *Server, id string) {
	t.Helper()
	if _, err := srv.store.GetAuthRequest(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("auth request still there: %v", err)
	}
}

// githubAndWallet is GitHub plus the "wallet" OIDC provider, so /authorize
// shows the sign-in page.
func githubAndWallet() oidcTestOpts {
	o := walletOnly()
	o.GitHub = true
	return o
}

func TestAuthorizeShowsSigninPage(t *testing.T) {
	_, ts, _, _ := newTestServerOIDC(t, githubAndWallet())
	id := registerNamedClient(t, ts, `<script>alert(1)</script>`, claudeRedirect)
	c := newBrowser(t)
	resp, body := get(t, c, authorizeURL(ts, id, claudeRedirect, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	checkHTMLHeaders(t, resp)
	for _, want := range []string{
		"Sign in to mcp-dock",
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"wants to use the tools on this gateway. Sign in first.",
		`action="/oauth/github/start"`, "Continue with GitHub",
		`action="/oauth/oidc/wallet/start"`, "Continue with Open Payments wallet",
		`name="request"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sign-in page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("client name not escaped")
	}
	if strings.Count(body, `name="request"`) != 2 {
		t.Errorf("want a request field on both forms: %s", body)
	}
	if strings.Contains(body, `name="wallet"`) || strings.Contains(body, "open-payments") {
		t.Errorf("built-in wallet form shown: %s", body)
	}
	if consentRequestID(t, body) != jarAuthCookie(c, ts).Value {
		t.Error("request field doesn't match cookie")
	}
}

// With only GitHub configured, /authorize goes straight to GitHub
// (TestAuthorizeRedirectsToGitHub covers the details). The old built-in
// Open Payments routes are gone.
func TestAuthorizeGitHubOnlySkipsPage(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, nil))
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), fake.URL+"/login/oauth/authorize?") {
		t.Fatalf("status %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	reqID := jarAuthCookie(c, ts).Value
	resp, _ = postForm(t, c, ts.URL+"/oauth/open-payments/start", url.Values{"request": {reqID}, "wallet": {"$wallet.example/alice"}})
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("old wallet start: status %d", resp.StatusCode)
	}
	resp, _ = get(t, c, ts.URL+"/oauth/open-payments/callback?interact_ref=x&hash=y")
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("old wallet callback: status %d", resp.StatusCode)
	}
}

// Without GitHub configured the page has no GitHub button and the GitHub
// routes don't exist.
func TestSigninWithoutGitHub(t *testing.T) {
	_, ts, _, _ := newTestServerOIDC(t, oidcTestOpts{
		Providers: []config.OIDCConfig{walletProvider, corpProvider},
		Allowed:   map[string][]string{"wallet": {aliceSub}},
	})
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, body := signinPage(t, c, ts, id)
	if strings.Contains(body, "Continue with GitHub") || strings.Contains(body, wantGHStart) {
		t.Errorf("GitHub button shown: %s", body)
	}
	resp, _ := postForm(t, c, ts.URL+wantGHStart, url.Values{"request": {reqID}})
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("github start: status %d", resp.StatusCode)
	}
	resp, _ = get(t, c, ts.URL+callbackPath+"?code=x&state="+url.QueryEscape(reqID))
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("github callback: status %d", resp.StatusCode)
	}
}

func TestGitHubStart(t *testing.T) {
	gh := testutil.NewFakeGitHub(t, testGitHubClientID, testGitHubClientSecret)
	gh.SetUser("alice", 1)
	opts := githubAndWallet()
	opts.GitHubFake = gh
	_, ts, _, _ := newTestServerOIDC(t, opts)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)

	// Without the cookie (another browser) the button does nothing.
	resp, _ := postForm(t, newBrowser(t), ts.URL+wantGHStart, url.Values{"request": {reqID}})
	checkHTMLError(t, resp, http.StatusBadRequest)

	resp, body := postForm(t, c, ts.URL+wantGHStart, url.Values{"request": {reqID}})
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("github start: status %d: %s", resp.StatusCode, body)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), gh.URL+"/login/oauth/authorize?") || loc.Query().Get("state") != reqID ||
		loc.Query().Get("redirect_uri") != ts.URL+callbackPath {
		t.Fatalf("Location %q", loc)
	}
	resp, _ = get(t, c, loc.String())
	resp, body = get(t, c, resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Signed in with GitHub as <strong>alice</strong>") {
		t.Fatalf("consent: status %d: %s", resp.StatusCode, body)
	}

	// Logged in already: the button is a stale link.
	resp, _ = postForm(t, c, ts.URL+wantGHStart, url.Values{"request": {reqID}})
	checkHTMLError(t, resp, http.StatusBadRequest)
}

// afterLogin on a request that already has a user is a stale link, and the
// first user stays.
func TestAfterLoginOnce(t *testing.T) {
	srv, ts, fakes, _ := newTestServerOIDC(t, walletOnly())
	fakes["wallet"].SetSub(aliceSub)
	c := newBrowser(t)
	_, body := get(t, c, oidcCallbackURL(t, c, ts, fakes["wallet"], "wallet", registerClient(t, ts, claudeRedirect)))
	ctx := context.Background()
	ar, err := srv.store.GetAuthRequest(ctx, consentRequestID(t, body))
	if err != nil {
		t.Fatal(err)
	}
	stale := *ar
	stale.Login = ""
	w := httptest.NewRecorder()
	srv.afterLogin(w, httptest.NewRequest(http.MethodGet, "/oauth/github/callback", nil), &stale, "github:alice", "alice", 1)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Login link expired") {
		t.Fatalf("second login: status %d: %s", w.Code, w.Body)
	}
	got, err := srv.store.GetAuthRequest(ctx, ar.ID)
	if err != nil || got.Login != "oidc:wallet:"+aliceSub || got.GitHubID != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

// Remembered consent skips the page for OIDC users too.
func TestOIDCConsentRemembered(t *testing.T) {
	_, ts, fakes, _ := newTestServerOIDC(t, walletOnly())
	fake := fakes["wallet"]
	fake.SetSub(aliceSub)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	_, body := get(t, c, oidcCallbackURL(t, c, ts, fake, "wallet", id))
	postForm(t, c, ts.URL+consentPath, url.Values{"request": {consentRequestID(t, body)}, "action": {"approve"}})
	resp, _ := get(t, c, oidcCallbackURL(t, c, ts, fake, "wallet", id))
	if q := redirectQuery(t, resp, claudeRedirect); q.Get("code") == "" {
		t.Fatalf("query %v", q)
	}
}
