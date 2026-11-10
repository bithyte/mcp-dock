package oauth

import (
	"net/http"
	"slices"
	"testing"
)

func TestProtectedResourceMetadata(t *testing.T) {
	_, ts, _ := newTestServer(t)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		resp, m := getJSON(t, ts.URL+p)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", p, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content-type %q", p, ct)
		}
		if m["resource"] != ts.URL+"/mcp" {
			t.Errorf("%s: resource %v", p, m["resource"])
		}
		if got := strs(m["authorization_servers"]); !slices.Equal(got, []string{ts.URL}) {
			t.Errorf("%s: authorization_servers %v", p, got)
		}
	}
}

func TestResourceMetadataURL(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	if got, want := srv.ResourceMetadataURL(), ts.URL+"/.well-known/oauth-protected-resource/mcp"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestAuthServerMetadata(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, m := getJSON(t, ts.URL+"/.well-known/oauth-authorization-server")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for k, want := range map[string]string{
		"issuer":                 ts.URL,
		"authorization_endpoint": ts.URL + "/authorize",
		"token_endpoint":         ts.URL + "/token",
		"registration_endpoint":  ts.URL + "/register",
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %s", k, m[k], want)
		}
	}
	for k, want := range map[string][]string{
		"code_challenge_methods_supported":      {"S256"},
		"grant_types_supported":                 {"authorization_code", "refresh_token"},
		"response_types_supported":              {"code"},
		"token_endpoint_auth_methods_supported": {"none", "client_secret_post", "client_secret_basic"},
	} {
		if got := strs(m[k]); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	// The SDK struct has no omitempty on jwks_uri; an empty string there
	// fails URL validation in some clients, so it must be absent.
	if _, ok := m["jwks_uri"]; ok {
		t.Errorf("jwks_uri present: %v", m["jwks_uri"])
	}
	if m["authorization_response_iss_parameter_supported"] != true {
		t.Errorf("authorization_response_iss_parameter_supported = %v", m["authorization_response_iss_parameter_supported"])
	}
}

func TestMetadataMethodNotAllowed(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/.well-known/oauth-authorization-server", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestMetadataCORS(t *testing.T) {
	_, ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/.well-known/oauth-authorization-server", nil)
	req.Header.Set("Origin", "http://localhost:6274")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: status %d, ACAO %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestLaterTaskRoutesRegistered(t *testing.T) {
	_, ts, _ := newTestServer(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/authorize"},
		{"GET", "/oauth/github/callback"},
		{"POST", "/consent"},
		{"POST", "/token"},
	} {
		req, _ := http.NewRequest(c.method, ts.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s %s not registered", c.method, c.path)
		}
	}
}

func TestSecretHelpers(t *testing.T) {
	a, b := newSecret(), newSecret()
	if len(a) != 43 || a == b {
		t.Fatalf("newSecret: %q %q", a, b)
	}
	for _, r := range a {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			t.Fatalf("non-base64url char %q in %q", r, a)
		}
	}
	// sha256("abc")
	if got := hashSecret("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hashSecret: %s", got)
	}
}
