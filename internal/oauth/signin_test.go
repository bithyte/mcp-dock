package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

const (
	wantOPStart    = "/oauth/open-payments/start"
	wantOPCallback = "/oauth/open-payments/callback"
	wantGHStart    = "/oauth/github/start"
	badWalletMsg   = "Enter a wallet address like $wallet.example/alice."
	noWalletMsg    = "Couldn&#39;t reach that wallet address. Check it and try again."
)

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

func submitWallet(t *testing.T, c *http.Client, ts *httptest.Server, reqID, wallet string) (*http.Response, string) {
	t.Helper()
	return postForm(t, c, ts.URL+wantOPStart, url.Values{"request": {reqID}, "wallet": {wallet}})
}

// startWallet submits wallet and expects a redirect to the fake's
// interaction page, which it returns.
func startWallet(t *testing.T, c *http.Client, ts *httptest.Server, fop *testutil.FakeOpenPayments, reqID, wallet string) string {
	t.Helper()
	resp, body := submitWallet(t, c, ts, reqID, wallet)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("wallet start: status %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, fop.URL+"/interact/") {
		t.Fatalf("wallet start Location %q", loc)
	}
	return loc
}

// interactOP plays the user approving at the wallet provider and returns
// the callback URL it sends the browser to.
func interactOP(t *testing.T, ts *httptest.Server, fop *testutil.FakeOpenPayments, redirectURL string) string {
	t.Helper()
	hc := *fop.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Get(redirectURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("fake interact: status %d", resp.StatusCode)
	}
	cb := resp.Header.Get("Location")
	if !strings.HasPrefix(cb, ts.URL+wantOPCallback+"?") {
		t.Fatalf("interact Location %q", cb)
	}
	return cb
}

// walletLogin runs /authorize, the wallet form, the fake interaction and the
// callback for wallet name, and returns the callback response.
func walletLogin(t *testing.T, c *http.Client, ts *httptest.Server, fop *testutil.FakeOpenPayments, clientID, name string) (*http.Response, string) {
	t.Helper()
	reqID, _ := signinPage(t, c, ts, clientID)
	cb := interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet(name)))
	return get(t, c, cb)
}

// hasRequest reports whether the fake got a request starting with prefix
// ("POST /auth", "POST /continue/").
func hasRequest(fop *testutil.FakeOpenPayments, prefix string) bool {
	for _, r := range fop.Requests() {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func authRequestGone(t *testing.T, srv *Server, id string) {
	t.Helper()
	if _, err := srv.store.GetAuthRequest(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("auth request still there: %v", err)
	}
}

func TestAuthorizeShowsSigninPage(t *testing.T) {
	_, ts, _, _ := newTestServerOP(t, []string{"alice"}, []string{"alice"}, true)
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
		`action="/oauth/open-payments/start"`, "Open Payments wallet address", `name="wallet"`, "Continue with wallet",
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
	if consentRequestID(t, body) != jarAuthCookie(c, ts).Value {
		t.Error("request field doesn't match cookie")
	}
}

// With only GitHub configured, /authorize still goes straight to GitHub
// (TestAuthorizeRedirectsToGitHub covers the details) and the wallet
// routes don't exist.
func TestAuthorizeGitHubOnlySkipsPage(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, nil))
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), fake.URL+"/login/oauth/authorize?") {
		t.Fatalf("status %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	reqID := jarAuthCookie(c, ts).Value
	resp, _ = submitWallet(t, c, ts, reqID, "$wallet.example/alice")
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("wallet start: status %d", resp.StatusCode)
	}
	resp, _ = get(t, c, ts.URL+wantOPCallback+"?interact_ref=x&hash=y")
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("wallet callback: status %d", resp.StatusCode)
	}
}

func TestSigninOpenPaymentsOnly(t *testing.T) {
	_, ts, _, _ := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, body := signinPage(t, c, ts, id)
	if strings.Contains(body, "Continue with GitHub") || strings.Contains(body, wantGHStart) {
		t.Errorf("GitHub button shown: %s", body)
	}
	if !strings.Contains(body, `name="wallet"`) || !strings.Contains(body, "Continue with wallet") {
		t.Errorf("wallet field missing: %s", body)
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
	_, ts, gh, _ := newTestServerOP(t, []string{"alice"}, []string{"alice"}, true)
	gh.SetUser("alice", 1)
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

func TestWalletLoginFlow(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	wallet := fop.Wallet("alice")
	cb := interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, " "+wallet+" "))

	bodies := fop.GrantBodies()
	if len(bodies) != 1 {
		t.Fatalf("%d grant requests", len(bodies))
	}
	var grant struct {
		Interact struct {
			Finish struct{ URI string } `json:"finish"`
		} `json:"interact"`
	}
	if err := json.Unmarshal(bodies[0], &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Interact.Finish.URI != ts.URL+wantOPCallback {
		t.Errorf("finish uri %q", grant.Interact.Finish.URI)
	}
	if strings.Contains(cb, reqID) {
		t.Error("callback URL carries the auth request id")
	}

	resp, body := get(t, c, cb)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	checkHTMLHeaders(t, resp)
	if !strings.Contains(body, "Signed in with Open Payments as <strong>"+wallet+"</strong>") {
		t.Fatalf("consent: %s", body)
	}
	if fop.Cancels() != 1 {
		t.Errorf("cancels %d, want 1", fop.Cancels())
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
	if info.UserID != "open_payments:"+wallet {
		t.Fatalf("user %q", info.UserID)
	}
}

func TestWalletNotAllowedMakesNoRequest(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	resp, body := submitWallet(t, c, ts, reqID, fop.Wallet("stranger"))
	checkHTMLError(t, resp, http.StatusForbidden)
	if !strings.Contains(body, "Open Payments wallet address "+fop.Wallet("stranger")) {
		t.Errorf("body: %s", body)
	}
	if got := fop.Requests(); len(got) != 0 {
		t.Fatalf("fake got requests: %v", got)
	}
	authRequestGone(t, srv, reqID)
}

func TestWalletBadInput(t *testing.T) {
	_, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	for typed, shown := range map[string]string{
		"http://wallet.example/alice": `value="http://wallet.example/alice"`,
		`http://a"><script>x`:         `value="http://a&#34;&gt;&lt;script&gt;x"`,
		"":                            `value=""`,
	} {
		resp, body := submitWallet(t, c, ts, reqID, typed)
		checkHTMLError(t, resp, http.StatusBadRequest)
		if !strings.Contains(body, badWalletMsg) || !strings.Contains(body, shown) {
			t.Errorf("%q: body %s", typed, body)
		}
		if strings.Contains(body, "<script>") {
			t.Errorf("%q: typed value not escaped", typed)
		}
		if consentRequestID(t, body) != reqID {
			t.Errorf("%q: re-render lost the request id", typed)
		}
	}
	if got := fop.Requests(); len(got) != 0 {
		t.Fatalf("fake got requests: %v", got)
	}
	// The auth request is still good.
	cb := interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet("alice")))
	resp, body := get(t, c, cb)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
}

// Wallet fetch and grant request failures re-render the page; the user can
// fix the address and retry.
func TestWalletUnreachable(t *testing.T) {
	_, ts, _, fop := newTestServerOP(t, nil, []string{"alice", "broken", "noauth"}, false)
	fop.SetWalletID("broken", "")
	fop.SetAuthServer("noauth", fop.URL+"/missing")
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	for _, name := range []string{"broken", "noauth"} {
		resp, body := submitWallet(t, c, ts, reqID, fop.Wallet(name))
		checkHTMLError(t, resp, http.StatusBadGateway)
		if !strings.Contains(body, noWalletMsg) || !strings.Contains(body, `value="`+fop.Wallet(name)+`"`) {
			t.Errorf("%s: body %s", name, body)
		}
	}
	cb := interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet("alice")))
	if resp, body := get(t, c, cb); resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
}

func TestWalletIDCanonicalizesSame(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	wallet := fop.Wallet("alice")
	fop.SetWalletID("alice", "HTTPS://"+strings.TrimPrefix(wallet, "https://")+"/")
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	resp, body := walletLogin(t, c, ts, fop, id, "alice")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Signed in with Open Payments as <strong>"+wallet+"</strong>") {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	ar, err := srv.store.GetAuthRequest(context.Background(), consentRequestID(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if ar.Login != "open_payments:"+wallet {
		t.Fatalf("login %q", ar.Login)
	}
}

func TestWalletIDDiffers(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	fop.SetWalletID("alice", fop.Wallet("other"))
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	resp, body := submitWallet(t, c, ts, reqID, fop.Wallet("alice"))
	checkHTMLError(t, resp, http.StatusForbidden)
	if !strings.Contains(body, "Open Payments wallet address "+fop.Wallet("other")) {
		t.Errorf("body: %s", body)
	}
	if !hasRequest(fop, "GET /alice") || hasRequest(fop, "POST /auth") {
		t.Fatalf("requests %v", fop.Requests())
	}
	authRequestGone(t, srv, reqID)
}

// A wallet document may not claim an id on another origin, even an
// allowlisted one: its authServer would then vouch for a wallet it doesn't
// host.
func TestWalletIDOtherOrigin(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	const victim = "https://other.example/alice"
	srv.cfg.AllowedUsers.OpenPayments = append(srv.cfg.AllowedUsers.OpenPayments, victim)
	for _, id := range []string{victim, strings.Replace(fop.Wallet("alice"), "127.0.0.1", "localhost", 1)} {
		if id != victim {
			srv.cfg.AllowedUsers.OpenPayments = append(srv.cfg.AllowedUsers.OpenPayments, id)
		}
		fop.SetWalletID("alice", id)
		c := newBrowser(t)
		reqID, _ := signinPage(t, c, ts, registerClient(t, ts, claudeRedirect))
		resp, body := submitWallet(t, c, ts, reqID, fop.Wallet("alice"))
		checkHTMLError(t, resp, http.StatusForbidden)
		if !strings.Contains(body, "Open Payments wallet address "+fop.Wallet("alice")) || strings.Contains(body, id) {
			t.Errorf("%s: body %s", id, body)
		}
		if hasRequest(fop, "POST /auth") {
			t.Fatalf("%s: grant requested: %v", id, fop.Requests())
		}
		authRequestGone(t, srv, reqID)
	}
}

// The id may differ from the typed address in path only (same origin), if
// it is allowlisted too.
func TestWalletIDSameOriginOtherPath(t *testing.T) {
	_, ts, _, fop := newTestServerOP(t, nil, []string{"alice", "alice2"}, false)
	fop.SetWalletID("alice", fop.Wallet("alice2"))
	c := newBrowser(t)
	resp, body := walletLogin(t, c, ts, fop, registerClient(t, ts, claudeRedirect), "alice")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Signed in with Open Payments as <strong>"+fop.Wallet("alice2")+"</strong>") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// afterLogin on a request that already has a user is a stale link, and the
// first user stays.
func TestAfterLoginOnce(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, []string{"alice"}, []string{"alice"}, true)
	c := newBrowser(t)
	_, body := walletLogin(t, c, ts, fop, registerClient(t, ts, claudeRedirect), "alice")
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
	if err != nil || got.Login != "open_payments:"+fop.Wallet("alice") || got.GitHubID != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWalletDeclined(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	fop.SetDecline(true)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	resp, _ := get(t, c, interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))))
	q := redirectQuery(t, resp, claudeRedirect)
	if q.Get("error") != "access_denied" || q.Get("state") != testState || q.Get("iss") != ts.URL || q.Get("code") != "" {
		t.Fatalf("query %v", q)
	}
	authRequestGone(t, srv, reqID)
}

func TestWalletResultOther(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))
	resp, _ := get(t, c, ts.URL+wantOPCallback+"?result=something_else")
	if q := redirectQuery(t, resp, claudeRedirect); q.Get("error") != "server_error" || q.Get("code") != "" {
		t.Fatalf("query %v", q)
	}
	authRequestGone(t, srv, reqID)
}

func TestWalletCallbackMissingParams(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	for _, query := range []string{"", "?interact_ref=x", "?hash=y"} {
		c := newBrowser(t)
		reqID, _ := signinPage(t, c, ts, id)
		startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))
		resp, _ := get(t, c, ts.URL+wantOPCallback+query)
		checkHTMLError(t, resp, http.StatusBadRequest)
		authRequestGone(t, srv, reqID)
	}
}

func TestWalletBadHash(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	fop.SetBadHash(true)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	cb := interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet("alice")))
	resp, body := get(t, c, cb)
	checkHTMLError(t, resp, http.StatusBadRequest)
	if !strings.Contains(body, "Wallet login failed") || !strings.Contains(body, "Couldn&#39;t confirm you own that wallet address. Start again from your app.") {
		t.Fatalf("body: %s", body)
	}
	if hasRequest(fop, "POST /continue/") {
		t.Error("grant continued despite bad hash")
	}
	authRequestGone(t, srv, reqID)
	// Even with the cookie put back, the second hit is a stale link.
	setJarCookie(t, c, ts, reqID)
	resp, body = get(t, c, cb)
	checkHTMLError(t, resp, http.StatusBadRequest)
	if !strings.Contains(body, "Login link expired") {
		t.Fatalf("second hit: %s", body)
	}
}

func TestWalletSubjectMismatch(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice", "other"}, false)
	fop.SetSubject(fop.Wallet("other"))
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	resp, body := get(t, c, interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))))
	checkHTMLError(t, resp, http.StatusBadRequest)
	if !strings.Contains(body, "Couldn&#39;t confirm you own that wallet address.") {
		t.Fatalf("body: %s", body)
	}
	authRequestGone(t, srv, reqID)
}

func TestWalletNotFinalized(t *testing.T) {
	srv, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	fop.SetContinueInteract(true)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	resp, body := get(t, c, interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))))
	checkHTMLError(t, resp, http.StatusBadGateway)
	if !strings.Contains(body, "Wallet login failed") || !strings.Contains(body, "Couldn&#39;t complete the wallet login. Try again.") {
		t.Fatalf("body: %s", body)
	}
	authRequestGone(t, srv, reqID)
}

func TestWalletCallbackOtherBrowser(t *testing.T) {
	_, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	cb := interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet("alice")))

	resp, body := get(t, newBrowser(t), cb)
	checkHTMLError(t, resp, http.StatusBadRequest)
	if !strings.Contains(body, "Login link expired") {
		t.Fatalf("body: %s", body)
	}
	// A browser in the middle of its own flow doesn't get to use it either.
	other := newBrowser(t)
	signinPage(t, other, ts, id)
	resp, _ = get(t, other, cb)
	checkHTMLError(t, resp, http.StatusBadRequest)
	// A cookie naming no auth request.
	stranger := newBrowser(t)
	setJarCookie(t, stranger, ts, "bogus")
	resp, _ = get(t, stranger, cb)
	checkHTMLError(t, resp, http.StatusBadRequest)

	resp, body = get(t, c, cb)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
		t.Fatalf("original browser: status %d: %s", resp.StatusCode, body)
	}
}

func TestWalletResubmit(t *testing.T) {
	_, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)

	t.Run("second attempt completes", func(t *testing.T) {
		c := newBrowser(t)
		reqID, _ := signinPage(t, c, ts, id)
		startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))
		second := startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))
		resp, body := get(t, c, interactOP(t, ts, fop, second))
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("stale attempt fails cleanly", func(t *testing.T) {
		c := newBrowser(t)
		reqID, _ := signinPage(t, c, ts, id)
		first := startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))
		startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))
		resp, body := get(t, c, interactOP(t, ts, fop, first))
		checkHTMLError(t, resp, http.StatusBadRequest)
		if !strings.Contains(body, "Wallet login failed") {
			t.Fatalf("body: %s", body)
		}
		if jarAuthCookie(c, ts) != nil {
			t.Error("cookie not cleared")
		}
		resp, body = walletLogin(t, c, ts, fop, id, "alice")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
			t.Fatalf("fresh authorize: status %d: %s", resp.StatusCode, body)
		}
	})
}

func TestWalletCallbackReplay(t *testing.T) {
	_, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	cb := interactOP(t, ts, fop, startWallet(t, c, ts, fop, reqID, fop.Wallet("alice")))
	resp, body := get(t, c, cb)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	resp, body = get(t, c, cb)
	checkHTMLError(t, resp, http.StatusBadRequest)
	if !strings.Contains(body, "Login link expired") {
		t.Fatalf("replay: %s", body)
	}
	// Starting another wallet login after login is a stale link too.
	resp, _ = submitWallet(t, c, ts, reqID, fop.Wallet("alice"))
	checkHTMLError(t, resp, http.StatusBadRequest)
	// The replay didn't spoil the real consent.
	resp, _ = postForm(t, c, ts.URL+consentPath, url.Values{"request": {reqID}, "action": {"approve"}})
	if q := redirectQuery(t, resp, claudeRedirect); q.Get("code") == "" {
		t.Fatalf("query %v", q)
	}
}

// Remembered consent skips the page for wallet users too.
func TestWalletConsentRemembered(t *testing.T) {
	_, ts, _, fop := newTestServerOP(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	_, body := walletLogin(t, c, ts, fop, id, "alice")
	postForm(t, c, ts.URL+consentPath, url.Values{"request": {consentRequestID(t, body)}, "action": {"approve"}})
	resp, _ := walletLogin(t, c, ts, fop, id, "alice")
	if q := redirectQuery(t, resp, claudeRedirect); q.Get("code") == "" {
		t.Fatalf("query %v", q)
	}
}

func TestWalletNoSecretsLogged(t *testing.T) {
	srv, ts, _, fop, logs := newTestServerOPLogs(t, nil, []string{"alice"}, false)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	reqID, _ := signinPage(t, c, ts, id)
	redirect := startWallet(t, c, ts, fop, reqID, fop.Wallet("alice"))

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
	if err := json.Unmarshal([]byte(raw), &pl); err != nil || pl.OpenPayments == nil {
		t.Fatalf("pending %v", err)
	}
	p := *pl.OpenPayments

	cbURL := interactOP(t, ts, fop, redirect)
	cb, _ := url.Parse(cbURL)
	resp, body := get(t, c, cbURL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	resp, _ = postForm(t, c, ts.URL+consentPath, url.Values{"request": {reqID}, "action": {"approve"}})
	code := redirectQuery(t, resp, claudeRedirect).Get("code")

	secrets := map[string]string{
		"continue token": p.ContinueToken,
		"client nonce":   p.ClientNonce,
		"finish nonce":   p.FinishNonce,
		"interact ref":   cb.Query().Get("interact_ref"),
		"hash":           cb.Query().Get("hash"),
		"request id":     reqID,
		"code":           code,
		"pending blob":   raw,
	}
	if !strings.Contains(string(fop.GrantBodies()[0]), p.ClientNonce) {
		t.Fatal("captured client nonce doesn't match the grant request")
	}
	out := logs.String()
	if !strings.Contains(out, fop.Wallet("alice")) {
		t.Errorf("wallet address not logged: %s", out)
	}
	for what, v := range secrets {
		if v == "" {
			t.Fatalf("%s not captured", what)
		}
		if strings.Contains(out, v) {
			t.Fatalf("log contains %s %q: %s", what, v, out)
		}
	}
}
