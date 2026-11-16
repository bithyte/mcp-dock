package oidclogin_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/oidclogin"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

const (
	clientID     = "dock-client"
	clientSecret = "s3cret-value"
	redirectURI  = "http://localhost:8080/oauth/oidc/idp/callback"
)

func newFake(t *testing.T, o testutil.FakeOIDCOptions) *testutil.FakeOIDC {
	t.Helper()
	return testutil.NewFakeOIDC(t, clientID, clientSecret, o)
}

func newClient(f *testutil.FakeOIDC, mod func(*oidclogin.Config)) *oidclogin.Client {
	c := oidclogin.Config{
		Name:         "idp",
		Issuer:       f.Issuer(),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{"openid"},
		UserClaim:    "sub",
		HTTP:         f.Client(),
	}
	if mod != nil {
		mod(&c)
	}
	return oidclogin.New(c)
}

// authorize follows the auth URL on the fake without a browser and returns
// the code from the redirect back to redirectURI.
func authorize(t *testing.T, authURL string) string {
	t.Helper()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", loc)
	}
	return code
}

// login runs a full login and returns Finish's result and the pending state.
func login(t *testing.T, c *oidclogin.Client) (string, oidclogin.Pending, error) {
	t.Helper()
	ctx := context.Background()
	authURL, p, err := c.Start(ctx, redirectURI)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	code := authorize(t, authURL)
	v, err := c.Finish(ctx, p, code, redirectURI)
	return v, p, err
}

func TestLogin(t *testing.T) {
	f := newFake(t, testutil.FakeOIDCOptions{})
	c := newClient(f, nil)
	if c.Name() != "idp" {
		t.Fatalf("Name = %q", c.Name())
	}
	v, p, err := login(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if v != "alice" {
		t.Fatalf("value = %q", v)
	}
	q := f.LastAuthorize()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             clientID,
		"code_challenge_method": "S256",
		"nonce":                 p.Nonce,
		"state":                 p.State,
		"scope":                 "openid",
		"redirect_uri":          redirectURI,
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if q.Get("code_challenge") == "" {
		t.Error("empty code_challenge")
	}
	for name, s := range map[string]string{"state": p.State, "nonce": p.Nonce, "verifier": p.Verifier} {
		if len(s) < 43 {
			t.Errorf("%s too short: %d", name, len(s))
		}
	}
	if p.State == p.Nonce || p.Nonce == p.Verifier || p.State == p.Verifier {
		t.Error("state, nonce, verifier not distinct")
	}
}

func TestScopesSent(t *testing.T) {
	f := newFake(t, testutil.FakeOIDCOptions{})
	c := newClient(f, func(c *oidclogin.Config) { c.Scopes = []string{"openid", "email"} })
	if _, _, err := login(t, c); err != nil {
		t.Fatal(err)
	}
	if got := f.LastAuthorize().Get("scope"); got != "openid email" {
		t.Fatalf("scope = %q", got)
	}
}

func TestDiscoveryCached(t *testing.T) {
	f := newFake(t, testutil.FakeOIDCOptions{})
	c := newClient(f, nil)
	for i := 0; i < 2; i++ {
		if _, _, err := login(t, c); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.Discoveries(); n != 1 {
		t.Fatalf("discoveries = %d", n)
	}
}

func TestDiscoveryRetriedAfterFailure(t *testing.T) {
	f := newFake(t, testutil.FakeOIDCOptions{})
	c := newClient(f, nil)
	f.SetDown(true)
	if _, _, err := c.Start(context.Background(), redirectURI); !errors.Is(err, oidclogin.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	f.SetDown(false)
	if _, _, err := login(t, c); err != nil {
		t.Fatal(err)
	}
	if n := f.Discoveries(); n != 2 {
		t.Fatalf("discoveries = %d", n)
	}
}

func TestIssuerTrailingSlash(t *testing.T) {
	f := newFake(t, testutil.FakeOIDCOptions{IssuerTrailingSlash: true})
	if !strings.HasSuffix(f.Issuer(), "/") {
		t.Fatalf("issuer %q", f.Issuer())
	}
	c := newClient(f, nil)
	if _, _, err := login(t, c); err != nil {
		t.Fatal(err)
	}
}

func TestTokenRejected(t *testing.T) {
	cases := []struct {
		name string
		set  func(f *testutil.FakeOIDC)
	}{
		{"aud", func(f *testutil.FakeOIDC) { f.SetClaims(map[string]any{"aud": "other"}) }},
		{"iss", func(f *testutil.FakeOIDC) { f.SetClaims(map[string]any{"iss": "https://evil.example"}) }},
		{"exp", func(f *testutil.FakeOIDC) {
			f.SetClaims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})
		}},
		{"nonce wrong", func(f *testutil.FakeOIDC) { f.SetClaims(map[string]any{"nonce": "wrong"}) }},
		{"nonce missing", func(f *testutil.FakeOIDC) { f.SetClaims(map[string]any{"nonce": nil}) }},
		{"azp", func(f *testutil.FakeOIDC) { f.SetClaims(map[string]any{"azp": "other"}) }},
		{"other key", func(f *testutil.FakeOIDC) { f.SetOtherKey(true) }},
		{"alg none", func(f *testutil.FakeOIDC) { f.SetAlgNone(true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, testutil.FakeOIDCOptions{})
			c := newClient(f, nil)
			tc.set(f)
			v, _, err := login(t, c)
			if !errors.Is(err, oidclogin.ErrInvalidToken) {
				t.Fatalf("value %q, err = %v", v, err)
			}
		})
	}
}

func TestAzpMatchingAccepted(t *testing.T) {
	f := newFake(t, testutil.FakeOIDCOptions{})
	c := newClient(f, nil)
	f.SetClaims(map[string]any{"azp": clientID, "aud": []string{clientID, "other"}})
	v, _, err := login(t, c)
	if err != nil || v != "alice" {
		t.Fatalf("value %q, err = %v", v, err)
	}
}

func TestClaim(t *testing.T) {
	cases := []struct {
		name    string
		claim   string
		claims  map[string]any
		want    string
		wantErr bool
	}{
		{"wallet", "wallet_address", map[string]any{"wallet_address": "https://w.example/a"}, "https://w.example/a", false},
		{"wallet absent", "wallet_address", nil, "", true},
		{"wallet number", "wallet_address", map[string]any{"wallet_address": 5}, "", true},
		{"wallet empty", "wallet_address", map[string]any{"wallet_address": ""}, "", true},
		{"email", "email", map[string]any{"email": "Bob@X.com", "email_verified": true}, "bob@x.com", false},
		{"email unverified", "email", map[string]any{"email": "Bob@X.com", "email_verified": false}, "", true},
		{"email verified absent", "email", map[string]any{"email": "Bob@X.com"}, "", true},
		{"email verified string", "email", map[string]any{"email": "Bob@X.com", "email_verified": "true"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, testutil.FakeOIDCOptions{})
			c := newClient(f, func(c *oidclogin.Config) { c.UserClaim = tc.claim })
			if tc.claims != nil {
				f.SetClaims(tc.claims)
			}
			v, _, err := login(t, c)
			if tc.wantErr {
				if !errors.Is(err, oidclogin.ErrClaim) {
					t.Fatalf("value %q, err = %v", v, err)
				}
				return
			}
			if err != nil || v != tc.want {
				t.Fatalf("value %q, err = %v", v, err)
			}
		})
	}
}

func TestIdPCodeRejected(t *testing.T) {
	f := newFake(t, testutil.FakeOIDCOptions{})
	c := newClient(f, nil)
	ctx := context.Background()
	_, p, err := c.Start(ctx, redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	const code = "made-up-code-12345"
	_, err = c.Finish(ctx, p, code, redirectURI)
	if !errors.Is(err, oidclogin.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	for _, s := range []string{code, clientSecret, p.Verifier} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error leaks %q: %v", s, err)
		}
	}
}

func TestBadClientSecretNamesErrorCode(t *testing.T) {
	f := newFake(t, testutil.FakeOIDCOptions{})
	c := newClient(f, func(c *oidclogin.Config) { c.ClientSecret = "not-the-secret" })
	ctx := context.Background()
	_, p, err := c.Start(ctx, redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Finish(ctx, p, "made-up-code-12345", redirectURI)
	if !errors.Is(err, oidclogin.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "status 401: invalid_client") {
		t.Errorf("error lacks status and OAuth error code: %v", err)
	}
	if strings.Contains(err.Error(), "not-the-secret") {
		t.Errorf("error leaks the secret: %v", err)
	}
}
