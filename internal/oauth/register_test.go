package oauth

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const claudeRedirect = "https://claude.ai/api/mcp/auth_callback"

func TestRegisterPublicClient(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	resp, m := postJSON(t, ts.URL+"/register", map[string]any{
		"client_name":                "Claude",
		"redirect_uris":              []string{claudeRedirect},
		"token_endpoint_auth_method": "none",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %v", resp.StatusCode, m)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	id, _ := m["client_id"].(string)
	if id == "" {
		t.Fatalf("no client_id: %v", m)
	}
	if _, ok := m["client_secret"]; ok {
		t.Fatalf("public client got a secret: %v", m)
	}
	if m["token_endpoint_auth_method"] != "none" || m["client_name"] != "Claude" {
		t.Errorf("echoed metadata wrong: %v", m)
	}
	if got := strs(m["redirect_uris"]); !slices.Equal(got, []string{claudeRedirect}) {
		t.Errorf("redirect_uris %v", got)
	}
	if _, ok := m["client_id_issued_at"].(float64); !ok {
		t.Errorf("client_id_issued_at missing: %v", m)
	}
	c, err := srv.store.GetClient(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthMethod != "none" || c.SecretHash != "" || c.Name != "Claude" || !slices.Equal(c.RedirectURIs, []string{claudeRedirect}) {
		t.Fatalf("stored client %+v", c)
	}
}

func TestRegisterConfidentialClient(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	resp, m := postJSON(t, ts.URL+"/register", map[string]any{
		"client_name":                "cli",
		"redirect_uris":              []string{"http://localhost:6274/cb"},
		"token_endpoint_auth_method": "client_secret_post",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %v", resp.StatusCode, m)
	}
	secret, _ := m["client_secret"].(string)
	if len(secret) < 43 {
		t.Fatalf("client_secret %q", secret)
	}
	// RFC 7591 3.2.1: REQUIRED when a secret is issued; 0 = never expires.
	if v, ok := m["client_secret_expires_at"]; !ok || v != float64(0) {
		t.Errorf("client_secret_expires_at = %v (present %v)", v, ok)
	}
	c, err := srv.store.GetClient(context.Background(), m["client_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthMethod != "client_secret_post" {
		t.Errorf("auth method %q", c.AuthMethod)
	}
	if c.SecretHash != hashSecret(secret) || strings.Contains(c.SecretHash, secret) {
		t.Fatalf("store should hold only the hash: %q", c.SecretHash)
	}
}

func TestRegisterDefaultsAuthMethod(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	resp, m := postJSON(t, ts.URL+"/register", map[string]any{
		"redirect_uris": []string{claudeRedirect},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %v", resp.StatusCode, m)
	}
	if m["token_endpoint_auth_method"] != "client_secret_basic" {
		t.Errorf("auth method %v", m["token_endpoint_auth_method"])
	}
	secret, _ := m["client_secret"].(string)
	if secret == "" {
		t.Fatalf("no secret: %v", m)
	}
	c, err := srv.store.GetClient(context.Background(), m["client_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthMethod != "client_secret_basic" || c.SecretHash != hashSecret(secret) {
		t.Fatalf("stored client %+v", c)
	}
}

func TestRegisterRedirectValidation(t *testing.T) {
	_, ts, _ := newTestServer(t)
	accept := []string{
		claudeRedirect,
		"http://localhost:6274/cb",
		"http://localhost/cb",
		"http://localhost:6274/oauth/callback?x=1",
		"http://127.0.0.1/cb",
		"http://127.0.0.1:33418",
		"http://[::1]:9/cb",
		"cursor://anysphere.cursor-retrieval/oauth",
		"com.example.app:/oauth2redirect",
		"vscode://vscode.github-authentication/did-authenticate",
		"https://evil.com/cb",
		"https://claude.ai/api/mcp/auth_callback/extra",
		"https://claude.ai/api/mcp/auth_callbackx",
		"https://claude.ai/api/mcp/auth_callback?x=1",
		"https://claude.ai:8443/api/mcp/auth_callback",
		"https://claude.ai.evil.com/api/mcp/auth_callback",
		"https://evil.com/api/mcp/auth_callback",
		"https://localhost/cb",
	}
	reject := []string{
		"http://evil.com/cb",
		"https:///cb",
		"https:foo",
		"https://:443/cb",
		"https://u@host/cb",
		"https://host/cb#x",
		"http://localhost.evil.com/cb",
		"http://127.0.0.1.evil.com/cb",
		"http://localhost@evil.com/cb",
		"http://user:pass@localhost/cb",
		"http://evil.com@localhost/cb",
		"http://localhost/cb#frag",
		"http://localhost/cb#",
		"cursor://anysphere.cursor-retrieval/oauth#x",
		"https://claude.ai/api/mcp/auth_callback#x",
		"https://user@claude.ai/api/mcp/auth_callback",
		"http://claude.ai/api/mcp/auth_callback",
		"http://0.0.0.0/cb",
		"http://127.0.0.2/cb",
		"http://[::2]/cb",
		"http://[::1%25eth0]/cb",
		"http://localhost:99999/cb",
		"http:localhost/cb",
		"http:///cb",
		"http://localhost\\@evil.com/cb",
		"http://localhost/c b",
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///x",
		"vbscript:msgbox(1)",
		"about:blank",
		"blob:https://evil.com/uuid",
		"ftp://evil.com/cb",
		"mailto:attacker@evil.com",
		"MAILTO:attacker@evil.com",
		"sms:+15555550100",
		"tel:+15555550100",
		"intent://evil.com/cb",
		"chrome-extension://abcdefghijklmnop/cb",
		"moz-extension://0d5e7a1c-0000-4000-8000-000000000000/cb",
		"cursor:",
		"cursor://",
		"/relative/cb",
		"//evil.com/cb",
		"",
		"not a url",
	}
	for _, u := range accept {
		resp, m := postJSON(t, ts.URL+"/register", map[string]any{
			"redirect_uris": []string{u}, "token_endpoint_auth_method": "none",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("accept %q: status %d %v", u, resp.StatusCode, m)
		}
	}
	for _, u := range reject {
		resp, m := postJSON(t, ts.URL+"/register", map[string]any{
			"redirect_uris": []string{claudeRedirect, u}, "token_endpoint_auth_method": "none",
		})
		if resp.StatusCode != http.StatusBadRequest || m["error"] != "invalid_redirect_uri" {
			t.Errorf("reject %q: status %d %v", u, resp.StatusCode, m)
		}
	}
	for name, body := range map[string]map[string]any{
		"missing redirect_uris": {"client_name": "x"},
		"empty redirect_uris":   {"redirect_uris": []string{}},
	} {
		resp, m := postJSON(t, ts.URL+"/register", body)
		if resp.StatusCode != http.StatusBadRequest || (m["error"] != "invalid_redirect_uri" && m["error"] != "invalid_client_metadata") {
			t.Errorf("%s: status %d %v", name, resp.StatusCode, m)
		}
	}
}

func TestRegisterBadMetadata(t *testing.T) {
	_, ts, _ := newTestServer(t)
	for name, body := range map[string]any{
		"unknown auth method": map[string]any{"redirect_uris": []string{claudeRedirect}, "token_endpoint_auth_method": "private_key_jwt"},
		"implicit grant":      map[string]any{"redirect_uris": []string{claudeRedirect}, "grant_types": []string{"implicit"}},
		"no auth code grant":  map[string]any{"redirect_uris": []string{claudeRedirect}, "grant_types": []string{"refresh_token"}},
		"token response type": map[string]any{"redirect_uris": []string{claudeRedirect}, "response_types": []string{"token"}},
		"not json":            []byte("{nope"),
		"json array":          []byte(`[1,2]`),
		"redirect not string": []byte(`{"redirect_uris":[1]}`),
	} {
		resp, m := postJSON(t, ts.URL+"/register", body)
		if resp.StatusCode != http.StatusBadRequest || m["error"] != "invalid_client_metadata" {
			t.Errorf("%s: status %d %v", name, resp.StatusCode, m)
		}
	}
}

func TestRegisterWrongContentType(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/register", "text/plain", strings.NewReader(`{"redirect_uris":["`+claudeRedirect+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestRegisterAcceptsGrantTypes(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, m := postJSON(t, ts.URL+"/register", map[string]any{
		"redirect_uris":  []string{claudeRedirect},
		"grant_types":    []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"},
		"scope":          "claudeai",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d %v", resp.StatusCode, m)
	}
	if got := strs(m["grant_types"]); !slices.Equal(got, []string{"authorization_code", "refresh_token"}) {
		t.Errorf("grant_types %v", got)
	}
}

func TestRegisterClientNameCapped(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	long := strings.Repeat("é", 150)
	resp, m := postJSON(t, ts.URL+"/register", map[string]any{
		"client_name":   long,
		"redirect_uris": []string{claudeRedirect},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d %v", resp.StatusCode, m)
	}
	c, err := srv.store.GetClient(context.Background(), m["client_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(c.Name); n != 100 || !utf8.ValidString(c.Name) {
		t.Fatalf("stored name has %d runes", n)
	}
	if m["client_name"] != c.Name {
		t.Errorf("response name differs from stored")
	}
}

func TestRegisterClientNameStripsControls(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	_, m := postJSON(t, ts.URL+"/register", map[string]any{
		"client_name":   "  Good‮evil\x00\n App  ",
		"redirect_uris": []string{claudeRedirect},
	})
	c, err := srv.store.GetClient(context.Background(), m["client_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Goodevil App" {
		t.Fatalf("name %q", c.Name)
	}
}

func TestRegisterBodyLimit(t *testing.T) {
	_, ts, _ := newTestServer(t)
	big := `{"client_name":"` + strings.Repeat("a", 1<<20) + `","redirect_uris":["` + claudeRedirect + `"]}`
	resp, err := http.Post(ts.URL+"/register", "application/json", bytes.NewReader([]byte(big)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestRegisterTooManyRedirects(t *testing.T) {
	_, ts, _ := newTestServer(t)
	var uris []string
	for i := 0; i < 50; i++ {
		uris = append(uris, "http://localhost/cb"+strings.Repeat("x", i))
	}
	resp, m := postJSON(t, ts.URL+"/register", map[string]any{"redirect_uris": uris})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d %v", resp.StatusCode, m)
	}
}
