package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/testutil"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

// testClock is an adjustable clock for srv.now.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTokenServer is newTestServer with a fake clock and "cozmin" (id 1)
// as the GitHub user.
func newTokenServer(t *testing.T) (*Server, *httptest.Server, *testutil.FakeGitHub, *testClock) {
	t.Helper()
	srv, ts, fake := newTestServer(t)
	clk := &testClock{t: time.Now()}
	srv.now = clk.Now
	fake.SetUser("cozmin", 1)
	return srv, ts, fake, clk
}

// obtainCode runs the authorize flow (approving consent if asked) and
// returns the authorization code. set overrides /authorize parameters as
// in authorizeURL.
func obtainCode(t *testing.T, ts *httptest.Server, fake *testutil.FakeGitHub, clientID, redirect string, set map[string]string) string {
	t.Helper()
	c := newBrowser(t)
	resp, body := loginAt(t, c, ts, fake, authorizeURL(ts, clientID, redirect, set))
	if resp.StatusCode == http.StatusOK {
		resp, _ = postForm(t, c, ts.URL+"/oauth/consent", url.Values{"request": {consentRequestID(t, body)}, "action": {"approve"}})
	}
	code := redirectQuery(t, resp, redirect).Get("code")
	if code == "" {
		t.Fatalf("no code in redirect %q", resp.Header.Get("Location"))
	}
	return code
}

type tokenResp struct {
	*http.Response
	body map[string]any
	raw  string
}

func (r tokenResp) str(k string) string { s, _ := r.body[k].(string); return s }

// tokenRequest POSTs a raw body to /token with extra headers.
func tokenRequest(t *testing.T, ts *httptest.Server, body, contentType string, header http.Header) tokenResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/token", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := tokenResp{Response: resp, raw: string(b)}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out.body); err != nil {
			t.Fatalf("token response not JSON (status %d): %q", resp.StatusCode, b)
		}
	}
	return out
}

func postToken(t *testing.T, ts *httptest.Server, form url.Values) tokenResp {
	t.Helper()
	return tokenRequest(t, ts, form.Encode(), "application/x-www-form-urlencoded", nil)
}

func postTokenBasic(t *testing.T, ts *httptest.Server, form url.Values, user, pass string) tokenResp {
	t.Helper()
	h := http.Header{}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	return tokenRequest(t, ts, form.Encode(), "application/x-www-form-urlencoded", h)
}

func codeForm(clientID, code, redirect string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {testVerifier},
	}
}

func refreshForm(clientID, refresh string) url.Values {
	return url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refresh},
	}
}

func wantOK(t *testing.T, r tokenResp) {
	t.Helper()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", r.StatusCode, r.raw)
	}
	if r.str("access_token") == "" || r.str("refresh_token") == "" {
		t.Fatalf("missing tokens: %s", r.raw)
	}
}

func wantError(t *testing.T, r tokenResp, status int, code string) {
	t.Helper()
	if r.StatusCode != status || r.str("error") != code {
		t.Fatalf("got %d %s, want %d %s", r.StatusCode, r.raw, status, code)
	}
	if r.str("access_token") != "" {
		t.Fatalf("error response carries a token: %s", r.raw)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", r.Header.Get("Cache-Control"))
	}
}

// exchange obtains a code for a fresh public client and redeems it.
func exchange(t *testing.T, ts *httptest.Server, fake *testutil.FakeGitHub) (clientID string, r tokenResp) {
	t.Helper()
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	r = postToken(t, ts, codeForm(id, code, claudeRedirect))
	wantOK(t, r)
	return id, r
}

func registerConfidential(t *testing.T, ts *httptest.Server, method, redirect string) (id, secret string) {
	t.Helper()
	resp, m := postJSON(t, ts.URL+"/register", map[string]any{
		"client_name":                "conf",
		"redirect_uris":              []string{redirect},
		"token_endpoint_auth_method": method,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d: %v", resp.StatusCode, m)
	}
	return m["client_id"].(string), m["client_secret"].(string)
}

func TestTokenExchange(t *testing.T) {
	srv, ts, fake, clk := newTokenServer(t)
	_, r := exchange(t, ts, fake)
	if r.str("token_type") != "Bearer" || r.body["expires_in"] != float64(3600) {
		t.Fatalf("response %s", r.raw)
	}
	if _, ok := r.body["scope"]; ok {
		t.Errorf("scope present without a requested scope: %s", r.raw)
	}
	h := r.Header
	if h.Get("Cache-Control") != "no-store" || h.Get("Pragma") != "no-cache" || h.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", h)
	}
	if h.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("no CORS header")
	}
	access, refresh := r.str("access_token"), r.str("refresh_token")
	if access == refresh || len(access) < 43 {
		t.Fatalf("tokens %q %q", access, refresh)
	}
	info, err := srv.Verify(context.Background(), access, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "github:cozmin" || info.Scopes != nil {
		t.Errorf("info %+v", info)
	}
	if d := info.Expiration.Sub(clk.Now()); d != time.Hour {
		t.Errorf("expiration in %v", d)
	}
	// Stored by hash only, bound to the canonical resource.
	ctx := context.Background()
	at, err := srv.store.GetToken(ctx, hashSecret(access), clk.Now())
	if err != nil || at.Kind != "access" || at.Resource != ts.URL+"/mcp" {
		t.Fatalf("stored access %+v %v", at, err)
	}
	rt, err := srv.store.GetToken(ctx, hashSecret(refresh), clk.Now())
	if err != nil || rt.Kind != "refresh" || rt.Resource != ts.URL+"/mcp" {
		t.Fatalf("stored refresh %+v %v", rt, err)
	}
	if d := rt.ExpiresAt.Sub(clk.Now()); d != 30*24*time.Hour {
		t.Errorf("refresh TTL %v", d)
	}
	for _, raw := range []string{access, refresh} {
		if _, err := srv.store.GetToken(ctx, raw, clk.Now()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("raw token stored as key")
		}
	}
}

func TestTokenScopeEchoed(t *testing.T) {
	srv, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, map[string]string{"scope": "read write"})
	r := postToken(t, ts, codeForm(id, code, claudeRedirect))
	wantOK(t, r)
	if r.str("scope") != "read write" {
		t.Fatalf("scope %q", r.str("scope"))
	}
	info, err := srv.Verify(context.Background(), r.str("access_token"), nil)
	if err != nil || info.Scopes != nil {
		t.Fatalf("verify %+v %v", info, err)
	}
}

func TestTokenPKCEMismatch(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	f := codeForm(id, code, claudeRedirect)
	f.Set("code_verifier", strings.Repeat("a", 43))
	wantError(t, postToken(t, ts, f), http.StatusBadRequest, "invalid_grant")
	// The failed attempt burned the code.
	wantError(t, postToken(t, ts, codeForm(id, code, claudeRedirect)), http.StatusBadRequest, "invalid_grant")
}

func TestTokenBadVerifierSyntax(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	for _, v := range []string{"", "short", strings.Repeat("a", 129), strings.Repeat("a", 42) + "!"} {
		f := codeForm(id, code, claudeRedirect)
		if v == "" {
			f.Del("code_verifier")
		} else {
			f.Set("code_verifier", v)
		}
		wantError(t, postToken(t, ts, f), http.StatusBadRequest, "invalid_request")
	}
	// Malformed requests don't consume the code.
	wantOK(t, postToken(t, ts, codeForm(id, code, claudeRedirect)))
}

func TestTokenCodeReplay(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	wantOK(t, postToken(t, ts, codeForm(id, code, claudeRedirect)))
	wantError(t, postToken(t, ts, codeForm(id, code, claudeRedirect)), http.StatusBadRequest, "invalid_grant")
}

func TestTokenUnknownCode(t *testing.T) {
	_, ts, _, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	wantError(t, postToken(t, ts, codeForm(id, newSecret(), claudeRedirect)), http.StatusBadRequest, "invalid_grant")
	f := codeForm(id, "", claudeRedirect)
	wantError(t, postToken(t, ts, f), http.StatusBadRequest, "invalid_request")
}

func TestTokenExpiredCode(t *testing.T) {
	_, ts, fake, clk := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	clk.Advance(codeTTL + time.Second)
	wantError(t, postToken(t, ts, codeForm(id, code, claudeRedirect)), http.StatusBadRequest, "invalid_grant")
}

func TestTokenRedirectMismatch(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	other := "https://example.com/cb"
	id := registerNamedClient(t, ts, testClient, claudeRedirect, other)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	wantError(t, postToken(t, ts, codeForm(id, code, other)), http.StatusBadRequest, "invalid_grant")
	wantError(t, postToken(t, ts, codeForm(id, code, claudeRedirect)), http.StatusBadRequest, "invalid_grant")

	// Exact match: a trailing slash is a different URI.
	code = obtainCode(t, ts, fake, id, claudeRedirect, nil)
	wantError(t, postToken(t, ts, codeForm(id, code, claudeRedirect+"/")), http.StatusBadRequest, "invalid_grant")

	// redirect_uri is required since the authorization request had one.
	code = obtainCode(t, ts, fake, id, claudeRedirect, nil)
	f := codeForm(id, code, claudeRedirect)
	f.Del("redirect_uri")
	wantError(t, postToken(t, ts, f), http.StatusBadRequest, "invalid_request")
}

func TestTokenLoopbackRedirectExact(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, "http://127.0.0.1/cb")
	code := obtainCode(t, ts, fake, id, "http://127.0.0.1:5555/cb", nil)
	// Any port was fine at /authorize, but /token needs the one used there.
	wantError(t, postToken(t, ts, codeForm(id, code, "http://127.0.0.1:5556/cb")), http.StatusBadRequest, "invalid_grant")
	code = obtainCode(t, ts, fake, id, "http://127.0.0.1:5555/cb", nil)
	wantOK(t, postToken(t, ts, codeForm(id, code, "http://127.0.0.1:5555/cb")))
}

func TestTokenWrongClient(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	a := registerClient(t, ts, claudeRedirect)
	b := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, a, claudeRedirect, nil)
	wantError(t, postToken(t, ts, codeForm(b, code, claudeRedirect)), http.StatusBadRequest, "invalid_grant")
	// Burned for A too.
	wantError(t, postToken(t, ts, codeForm(a, code, claudeRedirect)), http.StatusBadRequest, "invalid_grant")
}

func TestTokenUnknownClient(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	r := postToken(t, ts, codeForm("nope", code, claudeRedirect))
	wantError(t, r, http.StatusUnauthorized, "invalid_client")
	if r.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("WWW-Authenticate on non-Basic failure: %q", r.Header.Get("WWW-Authenticate"))
	}
	f := codeForm("", code, claudeRedirect)
	f.Del("client_id")
	wantError(t, postToken(t, ts, f), http.StatusUnauthorized, "invalid_client")
	// Client auth failures don't consume the code.
	wantOK(t, postToken(t, ts, codeForm(id, code, claudeRedirect)))
}

func TestTokenConfidentialClient(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id, secret := registerConfidential(t, ts, "client_secret_basic", claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	f := codeForm(id, code, claudeRedirect)

	r := postTokenBasic(t, ts, f, id, secret+"x")
	wantError(t, r, http.StatusUnauthorized, "invalid_client")
	if !strings.HasPrefix(r.Header.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("WWW-Authenticate %q", r.Header.Get("WWW-Authenticate"))
	}
	// No credentials at all.
	wantError(t, postToken(t, ts, f), http.StatusUnauthorized, "invalid_client")
	// Wrong method: secret in the form.
	fp := codeForm(id, code, claudeRedirect)
	fp.Set("client_secret", secret)
	wantError(t, postToken(t, ts, fp), http.StatusUnauthorized, "invalid_client")
	// Basic for a different client id than the form's.
	other := registerClient(t, ts, claudeRedirect)
	fo := codeForm(other, code, claudeRedirect)
	wantError(t, postTokenBasic(t, ts, fo, id, secret), http.StatusUnauthorized, "invalid_client")

	// Right secret; client_id in the form is optional with Basic.
	f.Del("client_id")
	wantOK(t, postTokenBasic(t, ts, f, id, secret))
}

func TestTokenBasicURLDecoded(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id, secret := registerConfidential(t, ts, "client_secret_basic", claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	// RFC 6749 2.3.1: credentials are form-urlencoded before Basic encoding.
	enc := func(s string) string { return fmt.Sprintf("%%%02X", s[0]) + s[1:] }
	wantOK(t, postTokenBasic(t, ts, codeForm(id, code, claudeRedirect), enc(id), enc(secret)))

	code = obtainCode(t, ts, fake, id, claudeRedirect, nil)
	h := http.Header{"Authorization": {"Basic !!!notbase64"}}
	r := tokenRequest(t, ts, codeForm(id, code, claudeRedirect).Encode(), "application/x-www-form-urlencoded", h)
	wantError(t, r, http.StatusUnauthorized, "invalid_client")
	h = http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(id+":%zz"))}}
	r = tokenRequest(t, ts, codeForm(id, code, claudeRedirect).Encode(), "application/x-www-form-urlencoded", h)
	wantError(t, r, http.StatusUnauthorized, "invalid_client")
}

func TestTokenClientSecretPost(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id, secret := registerConfidential(t, ts, "client_secret_post", claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	f := codeForm(id, code, claudeRedirect)
	f.Set("client_secret", "wrong")
	r := postToken(t, ts, f)
	wantError(t, r, http.StatusUnauthorized, "invalid_client")
	if r.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("WWW-Authenticate %q", r.Header.Get("WWW-Authenticate"))
	}
	wantError(t, postToken(t, ts, codeForm(id, code, claudeRedirect)), http.StatusUnauthorized, "invalid_client")
	// Wrong method: Basic.
	r = postTokenBasic(t, ts, codeForm(id, code, claudeRedirect), id, secret)
	wantError(t, r, http.StatusUnauthorized, "invalid_client")
	// Two methods at once (RFC 6749 2.3).
	f.Set("client_secret", secret)
	wantError(t, postTokenBasic(t, ts, f, id, secret), http.StatusUnauthorized, "invalid_client")
	wantOK(t, postToken(t, ts, f))
}

func TestTokenPublicClientAuth(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	// Basic isn't this client's method.
	wantError(t, postTokenBasic(t, ts, codeForm(id, code, claudeRedirect), id, "x"), http.StatusUnauthorized, "invalid_client")
	// A stray client_secret on a public client is ignored, not checked.
	f := codeForm(id, code, claudeRedirect)
	f.Set("client_secret", "anything")
	wantOK(t, postToken(t, ts, f))
}

func TestTokenResourceTrailingSlash(t *testing.T) {
	srv, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, map[string]string{"resource": ts.URL + "/mcp/"})
	f := codeForm(id, code, claudeRedirect)
	f.Set("resource", ts.URL+"/mcp/")
	r := postToken(t, ts, f)
	wantOK(t, r)
	if _, err := srv.Verify(context.Background(), r.str("access_token"), nil); err != nil {
		t.Fatal(err)
	}
	rf := refreshForm(id, r.str("refresh_token"))
	rf.Set("resource", ts.URL+"/mcp/")
	r = postToken(t, ts, rf)
	wantOK(t, r)
	if _, err := srv.Verify(context.Background(), r.str("access_token"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestTokenWithoutResource(t *testing.T) {
	srv, ts, fake, clk := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, map[string]string{"resource": ""})
	r := postToken(t, ts, codeForm(id, code, claudeRedirect))
	wantOK(t, r)
	if _, err := srv.Verify(context.Background(), r.str("access_token"), nil); err != nil {
		t.Fatal(err)
	}
	tok, err := srv.store.GetToken(context.Background(), hashSecret(r.str("access_token")), clk.Now())
	if err != nil || tok.Resource != ts.URL+"/mcp" {
		t.Fatalf("resource %+v %v", tok, err)
	}
	// Resource only at /token is fine too.
	code = obtainCode(t, ts, fake, id, claudeRedirect, map[string]string{"resource": ""})
	f := codeForm(id, code, claudeRedirect)
	f.Set("resource", ts.URL+"/mcp")
	wantOK(t, postToken(t, ts, f))
}

func TestTokenResourceMismatch(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	for _, res := range []string{"https://evil.example/mcp", ts.URL, ts.URL + "/mcp/x", ""} {
		f := codeForm(id, code, claudeRedirect)
		f.Set("resource", res)
		wantError(t, postToken(t, ts, f), http.StatusBadRequest, "invalid_target")
	}
	r := postToken(t, ts, codeForm(id, code, claudeRedirect))
	wantOK(t, r)
	rf := refreshForm(id, r.str("refresh_token"))
	rf.Set("resource", "https://evil.example/mcp")
	wantError(t, postToken(t, ts, rf), http.StatusBadRequest, "invalid_target")
}

func TestTokenAllowlistAtExchange(t *testing.T) {
	srv, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	srv.cfg.AllowedUsers = []string{}
	wantError(t, postToken(t, ts, codeForm(id, code, claudeRedirect)), http.StatusBadRequest, "invalid_grant")
}

func TestTokenUnsupportedGrant(t *testing.T) {
	_, ts, _, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	f := url.Values{"grant_type": {"password"}, "client_id": {id}}
	wantError(t, postToken(t, ts, f), http.StatusBadRequest, "unsupported_grant_type")
	f = url.Values{"grant_type": {"client_credentials"}, "client_id": {id}}
	wantError(t, postToken(t, ts, f), http.StatusBadRequest, "unsupported_grant_type")
}

func TestTokenMissingGrantType(t *testing.T) {
	_, ts, _, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	wantError(t, postToken(t, ts, url.Values{"client_id": {id}}), http.StatusBadRequest, "invalid_request")
}

func TestTokenDuplicateParams(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	for _, k := range []string{"grant_type", "client_id", "code", "redirect_uri", "code_verifier", "resource"} {
		f := codeForm(id, code, claudeRedirect)
		v := f.Get(k)
		if k == "resource" {
			v = ts.URL + "/mcp"
		}
		f[k] = []string{v, v}
		wantError(t, postToken(t, ts, f), http.StatusBadRequest, "invalid_request")
	}
	wantOK(t, postToken(t, ts, codeForm(id, code, claudeRedirect)))
}

func TestTokenRequestFormat(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	body := codeForm(id, code, claudeRedirect).Encode()
	for _, ct := range []string{"", "application/json", "text/plain", "multipart/form-data; boundary=x"} {
		wantError(t, tokenRequest(t, ts, body, ct, nil), http.StatusBadRequest, "invalid_request")
	}
	// Parameters in the query string don't count.
	r := tokenRequest(t, ts, "", "application/x-www-form-urlencoded", nil)
	wantError(t, r, http.StatusBadRequest, "invalid_request")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/token?"+body, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("query params: status %d", resp.StatusCode)
	}
	// Charset parameter is fine.
	wantOK(t, tokenRequest(t, ts, body, "application/x-www-form-urlencoded; charset=utf-8", nil))

	resp, err = http.Get(ts.URL + "/token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /token: status %d", resp.StatusCode)
	}
}

func TestTokenBodyLimit(t *testing.T) {
	_, ts, _, _ := newTokenServer(t)
	body := "grant_type=authorization_code&x=" + strings.Repeat("a", maxBodyBytes)
	r := tokenRequest(t, ts, body, "application/x-www-form-urlencoded", nil)
	wantError(t, r, http.StatusRequestEntityTooLarge, "invalid_request")
}

func TestTokenCORSPreflight(t *testing.T) {
	_, ts, _, _ := newTokenServer(t)
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/token", nil)
	req.Header.Set("Origin", "http://localhost:6274")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight %d %v", resp.StatusCode, resp.Header)
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials allowed")
	}
}

func TestRefreshRotates(t *testing.T) {
	srv, ts, fake, _ := newTokenServer(t)
	id, r := exchange(t, ts, fake)
	oldAccess, oldRefresh := r.str("access_token"), r.str("refresh_token")

	r2 := postToken(t, ts, refreshForm(id, oldRefresh))
	wantOK(t, r2)
	if r2.str("refresh_token") == oldRefresh || r2.str("access_token") == oldAccess {
		t.Fatalf("not rotated: %s", r2.raw)
	}
	if r2.str("token_type") != "Bearer" || r2.body["expires_in"] != float64(3600) {
		t.Fatalf("response %s", r2.raw)
	}
	if r2.Header.Get("Cache-Control") != "no-store" || r2.Header.Get("Pragma") != "no-cache" {
		t.Errorf("headers %v", r2.Header)
	}
	info, err := srv.Verify(context.Background(), r2.str("access_token"), nil)
	if err != nil || info.UserID != "github:cozmin" {
		t.Fatalf("new access: %+v %v", info, err)
	}
	// Reusing the rotated refresh token fails.
	wantError(t, postToken(t, ts, refreshForm(id, oldRefresh)), http.StatusBadRequest, "invalid_grant")
	// The new one still works.
	wantOK(t, postToken(t, ts, refreshForm(id, r2.str("refresh_token"))))
}

func TestRefreshUnknownOrMissing(t *testing.T) {
	_, ts, _, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	wantError(t, postToken(t, ts, refreshForm(id, newSecret())), http.StatusBadRequest, "invalid_grant")
	f := refreshForm(id, "")
	f.Del("refresh_token")
	wantError(t, postToken(t, ts, f), http.StatusBadRequest, "invalid_request")
}

func TestRefreshWithAccessToken(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id, r := exchange(t, ts, fake)
	wantError(t, postToken(t, ts, refreshForm(id, r.str("access_token"))), http.StatusBadRequest, "invalid_grant")
}

func TestRefreshWrongClient(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	a, r := exchange(t, ts, fake)
	b := registerClient(t, ts, claudeRedirect)
	wantError(t, postToken(t, ts, refreshForm(b, r.str("refresh_token"))), http.StatusBadRequest, "invalid_grant")
	// A presented-by-the-wrong-client token is burned.
	wantError(t, postToken(t, ts, refreshForm(a, r.str("refresh_token"))), http.StatusBadRequest, "invalid_grant")
}

func TestRefreshScopeNotWidened(t *testing.T) {
	_, ts, fake, _ := newTokenServer(t)
	id := registerClient(t, ts, claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, map[string]string{"scope": "read write"})
	r := postToken(t, ts, codeForm(id, code, claudeRedirect))
	wantOK(t, r)

	// Omitted scope keeps the original.
	r = postToken(t, ts, refreshForm(id, r.str("refresh_token")))
	wantOK(t, r)
	if r.str("scope") != "read write" {
		t.Fatalf("scope %q", r.str("scope"))
	}
	// Narrowing is allowed.
	f := refreshForm(id, r.str("refresh_token"))
	f.Set("scope", "read")
	r = postToken(t, ts, f)
	wantOK(t, r)
	if r.str("scope") != "read" {
		t.Fatalf("scope %q", r.str("scope"))
	}
	// Widening isn't, not even back to the original.
	f = refreshForm(id, r.str("refresh_token"))
	f.Set("scope", "read write")
	wantError(t, postToken(t, ts, f), http.StatusBadRequest, "invalid_scope")
}

func TestAccessExpiry(t *testing.T) {
	srv, ts, fake, clk := newTokenServer(t)
	_, r := exchange(t, ts, fake)
	clk.Advance(59 * time.Minute)
	if _, err := srv.Verify(context.Background(), r.str("access_token"), nil); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	clk.Advance(2 * time.Minute)
	_, err := srv.Verify(context.Background(), r.str("access_token"), nil)
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestRefreshExpiry(t *testing.T) {
	_, ts, fake, clk := newTokenServer(t)
	id, r := exchange(t, ts, fake)
	clk.Advance(31 * 24 * time.Hour)
	wantError(t, postToken(t, ts, refreshForm(id, r.str("refresh_token"))), http.StatusBadRequest, "invalid_grant")
}

func TestRemovedFromAllowlist(t *testing.T) {
	srv, ts, fake, _ := newTokenServer(t)
	id, r := exchange(t, ts, fake)
	srv.cfg.AllowedUsers = []string{}
	if _, err := srv.Verify(context.Background(), r.str("access_token"), nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("verify: %v", err)
	}
	wantError(t, postToken(t, ts, refreshForm(id, r.str("refresh_token"))), http.StatusBadRequest, "invalid_grant")
}

func TestVerifyRejectsRefreshAsAccess(t *testing.T) {
	srv, ts, fake, _ := newTokenServer(t)
	_, r := exchange(t, ts, fake)
	if _, err := srv.Verify(context.Background(), r.str("refresh_token"), nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("verify refresh: %v", err)
	}
}

func TestVerifyUnknownToken(t *testing.T) {
	srv, _, _, _ := newTokenServer(t)
	for _, tok := range []string{"", "nope", newSecret()} {
		if _, err := srv.Verify(context.Background(), tok, nil); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("verify %q: %v", tok, err)
		}
	}
}

func TestVerifyWrongResource(t *testing.T) {
	srv, _, _, clk := newTokenServer(t)
	tok := newSecret()
	err := srv.store.SaveToken(context.Background(), store.Token{
		Hash: hashSecret(tok), Kind: "access", ClientID: "c", Login: "github:cozmin",
		Resource: "https://other.example/mcp", ExpiresAt: clk.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Verify(context.Background(), tok, nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifierType(t *testing.T) {
	srv, _, _, _ := newTokenServer(t)
	var _ auth.TokenVerifier = srv.Verify
}

func TestTokenNoSecretsLogged(t *testing.T) {
	srv, ts, fake, logs := newTestServerLogs(t)
	clk := &testClock{t: time.Now()}
	srv.now = clk.Now
	fake.SetUser("cozmin", 1)
	id, secret := registerConfidential(t, ts, "client_secret_post", claudeRedirect)
	code := obtainCode(t, ts, fake, id, claudeRedirect, nil)
	f := codeForm(id, code, claudeRedirect)
	f.Set("client_secret", secret)
	r := postToken(t, ts, f)
	wantOK(t, r)
	rf := refreshForm(id, r.str("refresh_token"))
	rf.Set("client_secret", secret)
	r2 := postToken(t, ts, rf)
	wantOK(t, r2)
	postToken(t, ts, rf) // replay, logged as a failure
	out := logs.String()
	for _, s := range []string{code, secret, testVerifier, r.str("access_token"), r.str("refresh_token"), r2.str("access_token"), r2.str("refresh_token")} {
		if strings.Contains(out, s) {
			t.Fatalf("log contains secret %q: %s", s, out)
		}
	}
}

func TestCallbackStoresPrefixedUser(t *testing.T) {
	srv, ts, fake, _ := newTokenServer(t)
	fake.SetUser("Cozmin", 1)
	_, r := exchange(t, ts, fake)
	info, err := srv.Verify(context.Background(), r.str("access_token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "github:cozmin" {
		t.Errorf("UserID %q, want github:cozmin", info.UserID)
	}
}
