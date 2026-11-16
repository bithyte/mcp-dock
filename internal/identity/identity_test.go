package identity

import "testing"

func TestSplitAndUsers(t *testing.T) {
	if got := GitHubUser("Alice"); got != "github:alice" {
		t.Errorf("GitHubUser = %q", got)
	}
	p, name, ok := Split("github:alice")
	if p != GitHub || name != "alice" || !ok {
		t.Errorf("Split github = %q, %q, %v", p, name, ok)
	}
	for _, id := range []string{"alice", "github:", "google:x", "", "open_payments:https://w.example/a"} {
		if _, _, ok := Split(id); ok {
			t.Errorf("Split(%q) ok = true", id)
		}
	}
}

func TestOIDCIds(t *testing.T) {
	if got := OIDCUser("wallet", "https://w.example/Al"); got != "oidc:wallet:https://w.example/Al" {
		t.Errorf("OIDCUser = %q", got)
	}
	p, name, ok := Split("oidc:wallet:https://w.example/Al")
	if p != OIDCProvider("wallet") || name != "https://w.example/Al" || !ok {
		t.Errorf("Split = %q, %q, %v", p, name, ok)
	}
	if n, ok := p.OIDCName(); n != "wallet" || !ok {
		t.Errorf("OIDCName = %q, %v", n, ok)
	}
	if _, ok := GitHub.OIDCName(); ok {
		t.Error("github has an OIDC name")
	}
	p, name, ok = Split("oidc:corp:a:b:c")
	if p != "oidc:corp" || name != "a:b:c" || !ok {
		t.Errorf("Split colons = %q, %q, %v", p, name, ok)
	}
	for _, id := range []string{"oidc:", "oidc:wallet", "oidc:wallet:", "oidc::x"} {
		if _, _, ok := Split(id); ok {
			t.Errorf("Split(%q) ok = true", id)
		}
	}
}

func TestParseCLIUser(t *testing.T) {
	ok := map[string]string{
		"Alice":                            "github:alice",
		"github:Alice":                     "github:alice",
		"oidc:wallet:https://w.example/Al": "oidc:wallet:https://w.example/Al",
		" oidc:corp: Bob@X.com ":           "oidc:corp:Bob@X.com",
	}
	for in, want := range ok {
		got, err := ParseCLIUser(in)
		if err != nil || got != want {
			t.Errorf("ParseCLIUser(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "http://w.example/a", "google:x", "github:", "open_payments:", "open_payments:http://w.example/a",
		"$w.example/Al", "https://w.example/Al", "oidc:wallet:", "oidc::x", "oidc:wallet"} {
		if got, err := ParseCLIUser(in); err == nil {
			t.Errorf("ParseCLIUser(%q) = %q, want error", in, got)
		}
	}
}
