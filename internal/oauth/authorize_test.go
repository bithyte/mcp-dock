package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

const (
	testState    = "client-state-xyz"
	testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testClient   = "Test Client"
)

func testChallenge() string {
	sum := sha256.Sum256([]byte(testVerifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newBrowser is an HTTP client with a cookie jar that doesn't follow redirects.
func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func registerClient(t *testing.T, ts *httptest.Server, redirect string) string {
	t.Helper()
	return registerNamedClient(t, ts, testClient, redirect)
}

func registerNamedClient(t *testing.T, ts *httptest.Server, name string, redirects ...string) string {
	t.Helper()
	resp, m := postJSON(t, ts.URL+"/register", map[string]any{
		"client_name":                name,
		"redirect_uris":              redirects,
		"token_endpoint_auth_method": "none",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d: %v", resp.StatusCode, m)
	}
	return m["client_id"].(string)
}

// authorizeURL builds a valid /authorize URL; set overrides values, and an
// empty override value deletes the parameter.
func authorizeURL(ts *httptest.Server, clientID, redirect string, set map[string]string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", testState)
	q.Set("code_challenge", testChallenge())
	q.Set("code_challenge_method", "S256")
	q.Set("resource", ts.URL+"/mcp")
	for k, v := range set {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	return ts.URL + "/authorize?" + q.Encode()
}

// get performs a GET and returns the response with its body read.
func get(t *testing.T, c *http.Client, u string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func postForm(t *testing.T, c *http.Client, u string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// login runs /authorize, the fake GitHub hop, and the callback. It returns
// the callback response.
func login(t *testing.T, c *http.Client, ts *httptest.Server, fake *testutil.FakeGitHub, clientID, redirect string) (*http.Response, string) {
	t.Helper()
	return loginAt(t, c, ts, fake, authorizeURL(ts, clientID, redirect, nil))
}

// loginAt is login starting from a given /authorize URL.
func loginAt(t *testing.T, c *http.Client, ts *httptest.Server, fake *testutil.FakeGitHub, authURL string) (*http.Response, string) {
	t.Helper()
	resp, body := get(t, c, authURL)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d: %s", resp.StatusCode, body)
	}
	gh := resp.Header.Get("Location")
	if !strings.HasPrefix(gh, fake.URL+"/login/oauth/authorize?") {
		t.Fatalf("authorize Location %q", gh)
	}
	resp, body = get(t, c, gh)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("fake github: status %d: %s", resp.StatusCode, body)
	}
	cb := resp.Header.Get("Location")
	if !strings.HasPrefix(cb, ts.URL+"/oauth/github/callback?") {
		t.Fatalf("github Location %q", cb)
	}
	return get(t, c, cb)
}

var requestField = regexp.MustCompile(`name="request" value="([^"]+)"`)

func consentRequestID(t *testing.T, body string) string {
	t.Helper()
	m := requestField.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no request field in consent page: %s", body)
	}
	return m[1]
}

// redirectQuery parses Location and checks it targets redirect.
func redirectQuery(t *testing.T, resp *http.Response, redirect string) url.Values {
	t.Helper()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := url.Parse(redirect)
	if loc.Scheme != want.Scheme || loc.Host != want.Host || loc.Path != want.Path {
		t.Fatalf("Location %q not at redirect %q", loc, redirect)
	}
	return loc.Query()
}

func checkHTMLHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	h := resp.Header
	if !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type %q", h.Get("Content-Type"))
	}
	if h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("X-Frame-Options %q", h.Get("X-Frame-Options"))
	}
	if h.Get("Content-Security-Policy") != "frame-ancestors 'none'" {
		t.Errorf("CSP %q", h.Get("Content-Security-Policy"))
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", h.Get("Cache-Control"))
	}
}

// checkHTMLError asserts an HTML error page with no redirect.
func checkHTMLError(t *testing.T, resp *http.Response, status int) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status %d, want %d", resp.StatusCode, status)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("unexpected Location %q", loc)
	}
	checkHTMLHeaders(t, resp)
}

func jarAuthCookie(c *http.Client, ts *httptest.Server) *http.Cookie {
	u, _ := url.Parse(ts.URL + "/oauth/")
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "mcpdock_auth" {
			return ck
		}
	}
	return nil
}

func setJarCookie(t *testing.T, c *http.Client, ts *httptest.Server, value string) {
	t.Helper()
	u, _ := url.Parse(ts.URL + "/oauth/")
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "mcpdock_auth", Value: value, Path: "/oauth/"}})
}

func TestAuthorizeRedirectsToGitHub(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	resp, body := get(t, c, authorizeURL(ts, id, claudeRedirect, nil))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc.String(), fake.URL+"/login/oauth/authorize?") {
		t.Fatalf("Location %q", loc)
	}
	if got := loc.Query().Get("redirect_uri"); got != ts.URL+"/oauth/github/callback" {
		t.Errorf("github redirect_uri %q", got)
	}
	var ck *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == "mcpdock_auth" {
			ck = x
		}
	}
	if ck == nil {
		t.Fatal("no mcpdock_auth cookie")
	}
	if ck.Value == "" || loc.Query().Get("state") != ck.Value {
		t.Fatalf("state %q != cookie %q", loc.Query().Get("state"), ck.Value)
	}
	if ck.Value == testState {
		t.Fatal("client state leaked into GitHub state")
	}
	if !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/oauth/" || ck.Secure {
		t.Fatalf("cookie attrs %+v", ck)
	}
}

func TestAuthorizeCookieSecureOnHTTPS(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	srv.cfg.PublicURL = "https://gw.example.com"
	mux := http.NewServeMux()
	srv.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", authorizeURL(ts, id, claudeRedirect, map[string]string{"resource": "https://gw.example.com/mcp"}), nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	cks := rec.Result().Cookies()
	if len(cks) != 1 || !cks[0].Secure {
		t.Fatalf("cookies %+v", cks)
	}
	if got, _ := url.Parse(rec.Header().Get("Location")); got.Query().Get("redirect_uri") != "https://gw.example.com/oauth/github/callback" {
		t.Errorf("github redirect_uri %q", got.Query().Get("redirect_uri"))
	}
}

func TestAuthorizeUnknownClient(t *testing.T) {
	_, ts, _ := newTestServer(t)
	c := newBrowser(t)
	for _, id := range []string{"nope", ""} {
		resp, _ := get(t, c, authorizeURL(ts, "x", claudeRedirect, map[string]string{"client_id": id}))
		checkHTMLError(t, resp, http.StatusBadRequest)
	}
}

func TestAuthorizeWrongRedirect(t *testing.T) {
	_, ts, _ := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	loop := registerClient(t, ts, "http://127.0.0.1:1111/cb")
	c := newBrowser(t)
	for _, tc := range []struct{ client, redirect string }{
		{id, "https://evil.com/cb"},
		{id, ""},
		{id, claudeRedirect + "/extra"},
		{id, claudeRedirect + "?x=1"},
		{id, claudeRedirect + "#frag"},
		{id, "https://claude.ai.evil.com/api/mcp/auth_callback"},
		{id, "https://claude.ai:8443/api/mcp/auth_callback"},
		{id, "http://claude.ai/api/mcp/auth_callback"},
		{id, "https://user@claude.ai/api/mcp/auth_callback"},
		{loop, "http://127.0.0.1:2222/other"},
		{loop, "http://localhost:1111/cb"},
		{loop, "https://127.0.0.1:1111/cb"},
		{loop, "http://127.0.0.1:1111/cb?x=1"},
		{loop, "http://127.0.0.1.evil.com:1111/cb"},
	} {
		resp, _ := get(t, c, authorizeURL(ts, tc.client, tc.redirect, map[string]string{"redirect_uri": tc.redirect}))
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("%q: status %d Location %q", tc.redirect, resp.StatusCode, resp.Header.Get("Location"))
			continue
		}
		checkHTMLHeaders(t, resp)
	}
}

func TestAuthorizeLoopbackAnyPort(t *testing.T) {
	_, ts, fake := newTestServer(t)
	c := newBrowser(t)
	for _, tc := range []struct{ registered, requested string }{
		{"http://127.0.0.1:1111/cb", "http://127.0.0.1:2222/cb"},
		{"http://127.0.0.1/cb", "http://127.0.0.1:2222/cb"},
		{"http://localhost:6274/oauth/callback", "http://localhost:9999/oauth/callback"},
		{"http://[::1]:1/cb", "http://[::1]:65535/cb"},
	} {
		id := registerClient(t, ts, tc.registered)
		resp, body := get(t, c, authorizeURL(ts, id, tc.requested, nil))
		if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), fake.URL+"/login/oauth/authorize?") {
			t.Errorf("%s -> %s: status %d Location %q %s", tc.registered, tc.requested, resp.StatusCode, resp.Header.Get("Location"), body)
		}
	}
}

func TestAuthorizeRequiresS256(t *testing.T) {
	_, ts, _ := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	for _, set := range []map[string]string{
		{"code_challenge_method": "plain"},
		{"code_challenge_method": ""},
		{"code_challenge": ""},
		{"code_challenge": "short"},
		{"code_challenge": strings.Repeat("a", 42) + "!"},
	} {
		resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, set))
		q := redirectQuery(t, resp, claudeRedirect)
		if q.Get("error") != "invalid_request" || q.Get("state") != testState || q.Get("iss") != ts.URL || q.Get("code") != "" {
			t.Errorf("%v: Location query %v", set, q)
		}
	}
}

func TestAuthorizeUnsupportedResponseType(t *testing.T) {
	_, ts, _ := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	for _, rt := range []string{"token", ""} {
		resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, map[string]string{"response_type": rt}))
		q := redirectQuery(t, resp, claudeRedirect)
		if q.Get("error") != "unsupported_response_type" || q.Get("state") != testState || q.Get("iss") != ts.URL {
			t.Errorf("response_type %q: %v", rt, q)
		}
	}
}

func TestAuthorizeErrorKeepsRedirectQuery(t *testing.T) {
	_, ts, _ := newTestServer(t)
	redirect := "http://127.0.0.1:1111/cb?tenant=a"
	id := registerClient(t, ts, redirect)
	c := newBrowser(t)
	resp, _ := get(t, c, authorizeURL(ts, id, redirect, map[string]string{"response_type": "token"}))
	q := redirectQuery(t, resp, redirect)
	if q.Get("tenant") != "a" || q.Get("error") != "unsupported_response_type" {
		t.Fatalf("query %v", q)
	}
}

func TestAuthorizeBadResource(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	for _, res := range []string{"https://other/mcp", ts.URL, ts.URL + "/mcp/x", ts.URL + "/MCP"} {
		resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, map[string]string{"resource": res}))
		q := redirectQuery(t, resp, claudeRedirect)
		if q.Get("error") != "invalid_target" || q.Get("state") != testState || q.Get("iss") != ts.URL {
			t.Errorf("resource %q: %v", res, q)
		}
	}
	for _, res := range []string{ts.URL + "/mcp", ts.URL + "/mcp/", ""} {
		resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, map[string]string{"resource": res}))
		if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), fake.URL) {
			t.Errorf("resource %q rejected: %d %q", res, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestAuthorizeDuplicateParams(t *testing.T) {
	_, ts, _ := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, nil)+"&client_id="+url.QueryEscape(id))
	checkHTMLError(t, resp, http.StatusBadRequest)
	resp, _ = get(t, c, authorizeURL(ts, id, claudeRedirect, nil)+"&code_challenge_method=plain")
	if q := redirectQuery(t, resp, claudeRedirect); q.Get("error") != "invalid_request" {
		t.Fatalf("dup code_challenge_method: %v", q)
	}
}

func TestCallbackNotAllowed(t *testing.T) {
	srv, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("stranger", 99)
	c := newBrowser(t)
	resp, body := login(t, c, ts, fake, id, claudeRedirect)
	checkHTMLError(t, resp, http.StatusForbidden)
	if !strings.Contains(body, "GitHub account stranger isn&#39;t allowed on this gateway.") {
		t.Fatalf("body: %s", body)
	}
	if _, ok, _ := srv.store.GitHubID(context.Background(), "stranger"); ok {
		t.Fatal("stranger's GitHub ID was pinned")
	}
}

func TestCallbackCookieMismatch(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, nil))
	resp, _ = get(t, c, resp.Header.Get("Location"))
	cb := resp.Header.Get("Location")

	// No cookie (another browser).
	resp, _ = get(t, newBrowser(t), cb)
	checkHTMLError(t, resp, http.StatusBadRequest)

	// A different browser with its own flow (cookie set but not matching).
	other := newBrowser(t)
	get(t, other, authorizeURL(ts, id, claudeRedirect, nil))
	if jarAuthCookie(other, ts) == nil {
		t.Fatal("other browser has no cookie")
	}
	resp, _ = get(t, other, cb)
	checkHTMLError(t, resp, http.StatusBadRequest)

	// State that doesn't match the cookie.
	resp, _ = get(t, c, ts.URL+"/oauth/github/callback?code=x&state=bogus")
	checkHTMLError(t, resp, http.StatusBadRequest)

	// Cookie and state agree, but no such AuthRequest.
	setJarCookie(t, other, ts, "bogus")
	resp, _ = get(t, other, ts.URL+"/oauth/github/callback?code=x&state=bogus")
	checkHTMLError(t, resp, http.StatusBadRequest)

	// The original browser still completes.
	resp, body := get(t, c, cb)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("original browser: status %d: %s", resp.StatusCode, body)
	}
}

func TestCallbackGitHubDenied(t *testing.T) {
	_, ts, _ := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	c := newBrowser(t)
	resp, _ := get(t, c, authorizeURL(ts, id, claudeRedirect, nil))
	gh, _ := url.Parse(resp.Header.Get("Location"))
	state := gh.Query().Get("state")
	resp, _ = get(t, c, ts.URL+"/oauth/github/callback?error=access_denied&state="+url.QueryEscape(state))
	q := redirectQuery(t, resp, claudeRedirect)
	if q.Get("error") != "access_denied" || q.Get("state") != testState || q.Get("iss") != ts.URL || q.Get("code") != "" {
		t.Fatalf("query %v", q)
	}
}

func TestFirstLoginShowsConsent(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	resp, body := login(t, c, ts, fake, id, claudeRedirect)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	checkHTMLHeaders(t, resp)
	for _, want := range []string{testClient, "claude.ai", "cozmin", `action="/oauth/consent"`, `value="approve"`, `value="deny"`} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	if consentRequestID(t, body) != jarAuthCookie(c, ts).Value {
		t.Error("request field doesn't match cookie")
	}
	// Replaying the callback in the same browser is rejected outright.
	resp, _ = get(t, c, resp.Request.URL.String())
	checkHTMLError(t, resp, http.StatusBadRequest)
}

func TestConsentEscapesClientName(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerNamedClient(t, ts, `<script>alert(1)</script>`, claudeRedirect)
	fake.SetUser("cozmin", 1)
	_, body := login(t, newBrowser(t), ts, fake, id, claudeRedirect)
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("client name not escaped: %s", body)
	}
}

func TestConsentApproveIssuesCode(t *testing.T) {
	srv, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	_, body := login(t, c, ts, fake, id, claudeRedirect)
	reqID := consentRequestID(t, body)
	resp, _ := postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {reqID}, "action": {"approve"}})
	q := redirectQuery(t, resp, claudeRedirect)
	code := q.Get("code")
	if code == "" || q.Get("state") != testState || q.Get("iss") != ts.URL || q.Get("error") != "" {
		t.Fatalf("query %v", q)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", resp.Header.Get("Cache-Control"))
	}
	if jarAuthCookie(c, ts) != nil {
		t.Error("cookie not cleared")
	}
	ctx := context.Background()
	got, err := srv.store.ConsumeCode(ctx, hashSecret(code), srv.now())
	if err != nil {
		t.Fatalf("code not stored by hash: %v", err)
	}
	if got.ClientID != id || got.RedirectURI != claudeRedirect || got.CodeChallenge != testChallenge() ||
		got.Login != "cozmin" || got.Resource != ts.URL+"/mcp" {
		t.Fatalf("stored code %+v", got)
	}
	if d := got.ExpiresAt.Sub(srv.now()); d < 4*60e9 || d > 5*60e9+1e9 {
		t.Errorf("code TTL %v", d)
	}
	if _, err := srv.store.GetAuthRequest(ctx, reqID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("auth request not deleted: %v", err)
	}
	if ok, _ := srv.store.HasConsent(ctx, "cozmin", id, claudeRedirect); !ok {
		t.Error("consent not saved")
	}
	if gid, ok, _ := srv.store.GitHubID(ctx, "cozmin"); !ok || gid != 1 {
		t.Errorf("github id %d %v", gid, ok)
	}
	// Replaying the consent POST, even with the cookie restored, issues nothing.
	setJarCookie(t, c, ts, reqID)
	resp, _ = postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {reqID}, "action": {"approve"}})
	checkHTMLError(t, resp, http.StatusBadRequest)
}

// Two approvals that both got past the AuthRequest lookup (a double
// click, or a racing replay) must not both get a code.
func TestIssueCodeOnlyOnce(t *testing.T) {
	srv, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)
	_, body := login(t, newBrowser(t), ts, fake, id, claudeRedirect)
	ar, err := srv.store.GetAuthRequest(context.Background(), consentRequestID(t, body))
	if err != nil {
		t.Fatal(err)
	}
	codes := 0
	for range 2 {
		w := httptest.NewRecorder()
		srv.issueCode(w, httptest.NewRequest(http.MethodPost, "/oauth/consent", nil), ar)
		if loc, _ := url.Parse(w.Header().Get("Location")); w.Code == http.StatusFound && loc.Query().Get("code") != "" {
			codes++
		} else if w.Code != http.StatusBadRequest {
			t.Errorf("second issue: status %d, want 400", w.Code)
		}
	}
	if codes != 1 {
		t.Fatalf("issued %d codes, want 1", codes)
	}
}

func TestConsentConcurrentApprovals(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	_, body := login(t, c, ts, fake, id, claudeRedirect)
	reqID := consentRequestID(t, body)
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := 0
	for range n {
		wg.Go(func() {
			// Each request carries the cookie, as a double submit would.
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/oauth/consent",
				strings.NewReader(url.Values{"request": {reqID}, "action": {"approve"}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(&http.Cookie{Name: authCookie, Value: reqID})
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if loc, _ := url.Parse(resp.Header.Get("Location")); loc != nil && loc.Query().Get("code") != "" {
				mu.Lock()
				codes++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if codes != 1 {
		t.Fatalf("issued %d codes for one authorization, want 1", codes)
	}
}

func TestConsentDeny(t *testing.T) {
	srv, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	_, body := login(t, c, ts, fake, id, claudeRedirect)
	resp, _ := postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {consentRequestID(t, body)}, "action": {"deny"}})
	q := redirectQuery(t, resp, claudeRedirect)
	if q.Get("error") != "access_denied" || q.Get("state") != testState || q.Get("iss") != ts.URL || q.Get("code") != "" {
		t.Fatalf("query %v", q)
	}
	if ok, _ := srv.store.HasConsent(context.Background(), "cozmin", id, claudeRedirect); ok {
		t.Error("consent saved on deny")
	}
}

func TestConsentRequiresCookieAndLogin(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)

	// Logged in, but POST from a browser without the cookie.
	c := newBrowser(t)
	_, body := login(t, c, ts, fake, id, claudeRedirect)
	reqID := consentRequestID(t, body)
	resp, _ := postForm(t, newBrowser(t), ts.URL+"/oauth/consent", url.Values{"request": {reqID}, "action": {"approve"}})
	checkHTMLError(t, resp, http.StatusBadRequest)

	// Cookie of a different flow.
	other := newBrowser(t)
	get(t, other, authorizeURL(ts, id, claudeRedirect, nil))
	resp, _ = postForm(t, other, ts.URL+"/oauth/consent", url.Values{"request": {reqID}, "action": {"approve"}})
	checkHTMLError(t, resp, http.StatusBadRequest)

	// Cookie matches, but GitHub login never happened.
	pre := newBrowser(t)
	get(t, pre, authorizeURL(ts, id, claudeRedirect, nil))
	preID := jarAuthCookie(pre, ts).Value
	resp, _ = postForm(t, pre, ts.URL+"/oauth/consent", url.Values{"request": {preID}, "action": {"approve"}})
	checkHTMLError(t, resp, http.StatusBadRequest)

	// Unknown action.
	resp, _ = postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {reqID}, "action": {"maybe"}})
	checkHTMLError(t, resp, http.StatusBadRequest)

	// The legit browser still works.
	resp, _ = postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {reqID}, "action": {"approve"}})
	if q := redirectQuery(t, resp, claudeRedirect); q.Get("code") == "" {
		t.Fatalf("query %v", q)
	}
}

func TestConsentRemembered(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	_, body := login(t, c, ts, fake, id, claudeRedirect)
	postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {consentRequestID(t, body)}, "action": {"approve"}})

	resp, body := login(t, c, ts, fake, id, claudeRedirect)
	q := redirectQuery(t, resp, claudeRedirect)
	if q.Get("code") == "" || q.Get("state") != testState || q.Get("iss") != ts.URL {
		t.Fatalf("query %v %s", q, body)
	}

	// Consent is per client: a second client still gets the page.
	id2 := registerClient(t, ts, claudeRedirect)
	resp, body = login(t, c, ts, fake, id2, claudeRedirect)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
		t.Fatalf("second client: status %d", resp.StatusCode)
	}
}

// approve runs a full login and approves the consent page.
func approve(t *testing.T, c *http.Client, ts *httptest.Server, fake *testutil.FakeGitHub, clientID, redirect string) {
	t.Helper()
	resp, body := login(t, c, ts, fake, clientID, redirect)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected consent page, got %d", resp.StatusCode)
	}
	resp, _ = postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {consentRequestID(t, body)}, "action": {"approve"}})
	if q := redirectQuery(t, resp, redirect); q.Get("code") == "" {
		t.Fatalf("approve: %v", q)
	}
}

func TestConsentRememberedPerRedirectURI(t *testing.T) {
	_, ts, fake := newTestServer(t)
	const evil = "https://evil.example/cb"
	id := registerNamedClient(t, ts, "Claude", claudeRedirect, evil)
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	approve(t, c, ts, fake, id, claudeRedirect)

	// Same client, other registered redirect: consent is asked again and
	// no code goes to the other URI.
	resp, body := login(t, c, ts, fake, id, evil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) || !strings.Contains(body, "evil.example") {
		t.Fatalf("other redirect skipped consent: status %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The approved URI is still remembered.
	resp, _ = login(t, c, ts, fake, id, claudeRedirect)
	if q := redirectQuery(t, resp, claudeRedirect); q.Get("code") == "" {
		t.Fatalf("approved redirect not remembered: %v", q)
	}
}

func TestConsentRememberedLoopbackAnyPort(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerNamedClient(t, ts, "cli", "http://127.0.0.1:1111/cb", "http://127.0.0.1/other")
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	approve(t, c, ts, fake, id, "http://127.0.0.1:2222/cb")

	resp, _ := login(t, c, ts, fake, id, "http://127.0.0.1:3333/cb")
	if q := redirectQuery(t, resp, "http://127.0.0.1:3333/cb"); q.Get("code") == "" {
		t.Fatalf("loopback port change not remembered: %v", q)
	}
	// A different loopback path is a different redirect.
	resp, _ = login(t, c, ts, fake, id, "http://127.0.0.1:2222/other")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other loopback path skipped consent: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestConsentShowsHomographHostAsPunycode(t *testing.T) {
	_, ts, fake := newTestServer(t)
	// %D0%B0 is Cyrillic small a (U+0430): "аpple.com" looks like apple.com.
	redirect := "https://%D0%B0pple.com/cb"
	id := registerClient(t, ts, redirect)
	fake.SetUser("cozmin", 1)
	resp, body := login(t, newBrowser(t), ts, fake, id, redirect)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(body, "xn--pple-43d.com") {
		t.Errorf("punycode host missing: %s", body)
	}
	if strings.Contains(body, "\u0430pple.com") {
		t.Errorf("unicode lookalike host shown: %s", body)
	}
}

// For anything but https the consent page shows the whole redirect URI,
// so an app scheme can't hide where the code goes behind generic text.
func TestConsentShowsFullNonHTTPSRedirect(t *testing.T) {
	_, ts, fake := newTestServer(t)
	fake.SetUser("cozmin", 1)
	for redirect, want := range map[string]string{
		"com.example.app:/oauth2redirect?a=1&b=<x>": "com.example.app:/oauth2redirect?a=1&amp;b=&lt;x&gt;",
		"cursor://anysphere.cursor/cb?x=1":          "cursor://anysphere.cursor/cb?x=1",
		"http://127.0.0.1:2222/cb":                  "http://127.0.0.1:2222/cb",
	} {
		id := registerClient(t, ts, redirect)
		resp, body := login(t, newBrowser(t), ts, fake, id, redirect)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", redirect, resp.StatusCode)
		}
		if !strings.Contains(body, "<strong>"+want+"</strong>") {
			t.Errorf("%s: full redirect %q not shown: %s", redirect, want, body)
		}
		if strings.Contains(body, "app on this device") {
			t.Errorf("%s: generic text shown", redirect)
		}
	}
}

func TestRedirectDisplay(t *testing.T) {
	for in, want := range map[string]string{
		claudeRedirect:                       "claude.ai",
		"https://claude.ai:8443/cb?x=1":      "claude.ai:8443",
		"http://localhost:6274/cb":           "http://localhost:6274/cb",
		"com.example.app:/cb":                "com.example.app:/cb",
		"vscode://vscode.github-auth/did?x=": "vscode://vscode.github-auth/did?x=",
		"x-app:opaque-data":                  "x-app:opaque-data",
	} {
		if got := redirectDisplay(in); got != want {
			t.Errorf("redirectDisplay(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitHubIDMismatchDenied(t *testing.T) {
	srv, ts, fake, logs := newTestServerLogs(t)
	id := registerClient(t, ts, claudeRedirect)
	if err := srv.store.SetGitHubID(context.Background(), "cozmin", 1); err != nil {
		t.Fatal(err)
	}
	fake.SetUser("cozmin", 2)
	resp, body := login(t, newBrowser(t), ts, fake, id, claudeRedirect)
	checkHTMLError(t, resp, http.StatusForbidden)
	if !strings.Contains(body, "cozmin") {
		t.Errorf("body: %s", body)
	}
	var warn string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, `"login":"cozmin"`) {
			warn = line
		}
	}
	if warn == "" || !strings.Contains(warn, `:1`) || !strings.Contains(warn, `:2`) {
		t.Fatalf("no warn log with login and both IDs: %s", logs.String())
	}
	if gid, _, _ := srv.store.GitHubID(context.Background(), "cozmin"); gid != 1 {
		t.Errorf("pinned id changed to %d", gid)
	}
}

func TestNoSecretsLogged(t *testing.T) {
	_, ts, fake, logs := newTestServerLogs(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("cozmin", 1)
	c := newBrowser(t)
	_, body := login(t, c, ts, fake, id, claudeRedirect)
	reqID := consentRequestID(t, body)
	resp, _ := postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {reqID}, "action": {"approve"}})
	code := redirectQuery(t, resp, claudeRedirect).Get("code")
	out := logs.String()
	for _, secret := range []string{reqID, code, testGitHubClientSecret, "gho_"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains secret %q: %s", secret, out)
		}
	}
}

func TestLoginCaseInsensitive(t *testing.T) {
	_, ts, fake := newTestServer(t)
	id := registerClient(t, ts, claudeRedirect)
	fake.SetUser("Cozmin", 1)
	resp, body := login(t, newBrowser(t), ts, fake, id, claudeRedirect)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="approve"`) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func TestConsentKey(t *testing.T) {
	for in, want := range map[string]string{
		claudeRedirect:                   claudeRedirect,
		"https://claude.ai:443/cb":       "https://claude.ai:443/cb",
		"http://127.0.0.1:2222/cb?x=1":   "http://127.0.0.1/cb?x=1",
		"http://localhost:6274/callback": "http://localhost/callback",
		"http://[::1]:9/cb":              "http://[::1]/cb",
		"http://127.0.0.1/cb":            "http://127.0.0.1/cb",
		"cursor://anysphere.cursor/cb":   "cursor://anysphere.cursor/cb",
	} {
		if got := consentKey(in); got != want {
			t.Errorf("consentKey(%q) = %q, want %q", in, got, want)
		}
	}
}
