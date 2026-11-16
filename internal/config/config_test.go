package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bithyte/mcp-dock/internal/identity"
)

const validYAML = `
public_url: https://mcp.example.com/
data_dir: /var/lib/mcp-dock
providers:
  github:
    client_id: Ov23liXXXXXXXXXXXXXX
    client_secret_env: GITHUB_CLIENT_SECRET
allowed_users:
  github: [Alice]
upstreams:
  - name: jaypost
    url: http://jaypost:8080/mcp
    bearer_token_env: JAYPOST_RUNNER_TOKEN
    deny_args: [attachment.path]
`

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var goodEnv = map[string]string{"GITHUB_CLIENT_SECRET": "s", "JAYPOST_RUNNER_TOKEN": "t", "WALLET_SECRET": "ws"}

func TestLoadValid(t *testing.T) {
	c, err := Load("../../config.example.yaml", env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Providers.GitHub.ClientSecret != "s" || len(c.Upstreams) != 1 {
		t.Fatalf("bad config: %+v", c)
	}
	c, err = Load(write(t, validYAML), env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://mcp.example.com" {
		t.Fatalf("PublicURL = %q", c.PublicURL)
	}
	if c.ResourceURL() != "https://mcp.example.com/mcp" {
		t.Fatalf("ResourceURL = %q", c.ResourceURL())
	}
	if c.Listen != ":8080" {
		t.Fatalf("Listen = %q", c.Listen)
	}
}

func TestLoadRejects(t *testing.T) {
	r := strings.NewReplacer
	cases := []struct {
		name, yaml, field string
		env               map[string]string
	}{
		{"missing public_url", r("public_url: https://mcp.example.com/\n", "").Replace(validYAML), "public_url", goodEnv},
		{"public_url scheme", r("https://mcp.example.com/", "ftp://x").Replace(validYAML), "public_url", goodEnv},
		{"public_url path", r("https://mcp.example.com/", "https://mcp.example.com/dock").Replace(validYAML), "public_url", goodEnv},
		{"public_url path trailing slash", r("https://mcp.example.com/", "https://mcp.example.com/dock/").Replace(validYAML), "public_url", goodEnv},
		{"public_url query", r("https://mcp.example.com/", "https://mcp.example.com/?a=1").Replace(validYAML), "public_url", goodEnv},
		{"public_url empty query", r("https://mcp.example.com/", "https://mcp.example.com?").Replace(validYAML), "public_url", goodEnv},
		{"public_url fragment", r("https://mcp.example.com/", "\"https://mcp.example.com#x\"").Replace(validYAML), "public_url", goodEnv},
		{"public_url empty fragment", r("https://mcp.example.com/", "\"https://mcp.example.com/#\"").Replace(validYAML), "public_url", goodEnv},
		{"missing data_dir", r("data_dir: /var/lib/mcp-dock\n", "").Replace(validYAML), "data_dir", goodEnv},
		{"missing client_id", r("client_id: Ov23liXXXXXXXXXXXXXX", "client_id: \"\"").Replace(validYAML), "client_id", goodEnv},
		{"secret env empty", validYAML, "client_secret_env", map[string]string{"JAYPOST_RUNNER_TOKEN": "t"}},
		{"name uppercase", r("name: jaypost", "name: Jay").Replace(validYAML), "name", goodEnv},
		{"name double underscore", r("name: jaypost", "name: jay__post").Replace(validYAML), "name", goodEnv},
		{"name empty", r("name: jaypost", "name: \"\"").Replace(validYAML), "name", goodEnv},
		{"name trailing dash", r("name: jaypost", "name: a-").Replace(validYAML), "name", goodEnv},
		{"duplicate names", validYAML + "  - name: jaypost\n    url: http://x/mcp\n", "duplicate", goodEnv},
		{"deny_args empty", r("[attachment.path]", "[\"\"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args dotdot", r("[attachment.path]", "[a..b]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args trailing dot", r("[attachment.path]", "[\"attachment.\"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args leading dot", r("[attachment.path]", "[\".path\"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args leading space", r("[attachment.path]", "[\" attachment.path\"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args trailing space", r("[attachment.path]", "[\"attachment.path \"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args inner space", r("[attachment.path]", "[\"attachment. path\"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args tab", r("[attachment.path]", "[\"attachment.pa\\tth\"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args control", r("[attachment.path]", "[\"attachment.pa\\x01th\"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args dot only", r("[attachment.path]", "[\".\"]").Replace(validYAML), "deny_args", goodEnv},
		{"no providers", noProviders, "providers.oidc", goodEnv},
		{"allowed github without provider", oidcOnlyWithGitHubAllow, "allowed_users.github", goodEnv},
		{"unknown field", validYAML + "bogus: 1\n", "bogus", goodEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.yaml), env(tc.env))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error %q does not name %q", err, tc.field)
			}
		})
	}
}

func TestIsAllowed(t *testing.T) {
	c := &Config{AllowedUsers: AllowedUsers{GitHub: []string{"Alice"}}}
	for _, id := range []string{"github:alice", "github:ALICE"} {
		if !c.IsAllowed(id) {
			t.Fatalf("expected %q allowed", id)
		}
	}
	for _, id := range []string{
		"github:other", "alice", "github:https://ilp.interledger-test.dev/alice", "",
	} {
		if c.IsAllowed(id) {
			t.Fatalf("expected %q denied", id)
		}
	}
}

func TestLoadProviderCombos(t *testing.T) {
	for name, y := range map[string]string{"github only": githubOnly, "oidc only": oidcOnly} {
		if _, err := Load(write(t, y), env(goodEnv)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestOldConfigShape(t *testing.T) {
	old := "public_url: https://mcp.example.com\ndata_dir: /d\n"
	cases := map[string]struct{ yaml, want string }{
		"github":        {old + "github:\n  client_id: x\n  client_secret_env: GITHUB_CLIENT_SECRET\nallowed_users:\n  github: [a]\n", "providers.github"},
		"allowed_users": {old + "providers:\n  github:\n    client_id: x\n    client_secret_env: GITHUB_CLIENT_SECRET\nallowed_users: [alice]\n", "allowed_users.github"},
	}
	for name, tc := range cases {
		_, err := Load(write(t, tc.yaml), env(goodEnv))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %v does not name %q", name, err, tc.want)
		}
	}
	const removed = "config: built-in Open Payments login was removed; run open-payments-oidc and add it under providers.oidc"
	gh := "providers:\n  github:\n    client_id: x\n    client_secret_env: GITHUB_CLIENT_SECRET\n"
	for name, y := range map[string]string{
		"providers.open_payments":     old + "providers:\n  open_payments:\n    wallet_address: https://ilp.example/mcp\n    key_id: k\n    private_key_env: OP_PRIVATE_KEY\n",
		"allowed_users.open_payments": old + gh + "allowed_users:\n  github: [a]\n  open_payments: [\"$ilp.example/a\"]\n",
	} {
		_, err := Load(write(t, y), env(goodEnv))
		if err == nil || err.Error() != removed {
			t.Fatalf("%s: error %v, want %q", name, err, removed)
		}
	}
}

const head = "public_url: https://mcp.example.com\ndata_dir: /d\n"
const ghBlock = "  github:\n    client_id: x\n    client_secret_env: GITHUB_CLIENT_SECRET\n"
const oidcBlock = "  oidc:\n    - name: wallet\n      issuer: https://idp.example.com\n      client_id: wid\n      client_secret_env: WALLET_SECRET\n"

var (
	noProviders             = head + "allowed_users:\n  github: [a]\n"
	githubOnly              = head + "providers:\n" + ghBlock + "allowed_users:\n  github: [a]\n"
	oidcOnly                = head + "providers:\n" + oidcBlock + "allowed_users:\n  oidc:\n    wallet: [a]\n"
	oidcOnlyWithGitHubAllow = head + "providers:\n" + oidcBlock + "allowed_users:\n  github: [a]\n"
)

func TestDenyArgsAccepted(t *testing.T) {
	y := strings.Replace(validYAML, "[attachment.path]", `[attachment.path, path, "a-b.c_d.Éé"]`, 1)
	c, err := Load(write(t, y), env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Upstreams[0].DenyArgs; len(got) != 3 {
		t.Fatalf("deny_args = %q", got)
	}
}

func TestAccepts(t *testing.T) {
	r := strings.NewReplacer
	for _, tc := range []struct{ name, yaml string }{
		{"public_url root slash", validYAML},
		{"public_url no slash", r("https://mcp.example.com/", "https://mcp.example.com").Replace(validYAML)},
		{"public_url port", r("https://mcp.example.com/", "http://127.0.0.1:8080/").Replace(validYAML)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(write(t, tc.yaml), env(goodEnv)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// upstreamYAML is a minimal config with one upstream whose body is given.
func upstreamYAML(body string) string {
	return githubOnly + "upstreams:\n  - name: up\n" + body
}

func TestUpstreamURL(t *testing.T) {
	for _, u := range []string{"http://jaypost:8080/mcp", "https://h/mcp/", "http://h:1"} {
		c, err := Load(write(t, upstreamYAML("    url: \""+u+"\"\n")), env(goodEnv))
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		if got := c.Upstreams[0].URL; got != u {
			t.Fatalf("URL = %q, want %q", got, u)
		}
	}
	for _, u := range []string{"ftp://h", "http://", "http://u:p@h/mcp", "http://h/mcp?key=x", "http://h/mcp?", "http://h/mcp#f", ""} {
		_, err := Load(write(t, upstreamYAML("    url: \""+u+"\"\n")), env(goodEnv))
		if err == nil || !strings.Contains(err.Error(), "upstreams[0].url") {
			t.Fatalf("%q: err = %v, want one naming upstreams[0].url", u, err)
		}
	}
	// url missing entirely
	_, err := Load(write(t, upstreamYAML("    deny_args: [a]\n")), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "upstreams[0].url") {
		t.Fatalf("missing url: err = %v", err)
	}
}

func TestUpstreamBearerEnv(t *testing.T) {
	y := upstreamYAML("    url: http://h/mcp\n    bearer_token_env: RUNNER_TOK\n")
	c, err := Load(write(t, y), env(map[string]string{"GITHUB_CLIENT_SECRET": "s", "RUNNER_TOK": "tok"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Upstreams[0].BearerToken; got != "tok" {
		t.Fatalf("BearerToken = %q", got)
	}
	c, err = Load(write(t, upstreamYAML("    url: http://h/mcp\n")), env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstreams[0].BearerToken != "" {
		t.Fatalf("BearerToken = %q without bearer_token_env", c.Upstreams[0].BearerToken)
	}
	want := "upstreams[0].bearer_token_env: env var RUNNER_TOK is empty or unset"
	for name, e := range map[string]map[string]string{
		"unset": {"GITHUB_CLIENT_SECRET": "s"},
		"empty": {"GITHUB_CLIENT_SECRET": "s", "RUNNER_TOK": ""},
	} {
		_, err := Load(write(t, y), env(e))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", name, err, want)
		}
	}
}

func TestUpstreamOldShapeRejected(t *testing.T) {
	for field, body := range map[string]string{
		"command":  "    command: jaypost\n",
		"args":     "    args: [mcp]\n",
		"env":      "    env:\n      A: b\n",
		"env_from": "    env_from: [A]\n",
	} {
		_, err := Load(write(t, upstreamYAML("    url: http://h/mcp\n"+body)), env(goodEnv))
		want := "config: upstreams[0]." + field + ": stdio upstreams moved to mcp-runner; run the server in an mcp-runner container and set url: instead"
		if err == nil || err.Error() != want {
			t.Fatalf("%s: err = %v, want %q", field, err, want)
		}
	}
}

const oidcHead = "public_url: https://mcp.example.com\ndata_dir: /d\nproviders:\n" + ghBlock + "  oidc:\n"

const oidcWallet = "    - name: wallet\n      issuer: http://localhost:3000\n      client_id: wid\n      client_secret_env: WALLET_SECRET\n"

func oidcCorp(name, issuer, clientID, secretEnv string) string {
	return "    - name: " + name + "\n      issuer: " + issuer + "\n      client_id: " + clientID + "\n      client_secret_env: " + secretEnv + "\n      scopes: [email]\n      user_claim: email\n"
}

var oidcEnv = map[string]string{"GITHUB_CLIENT_SECRET": "s", "WALLET_SECRET": "ws", "CORP_SECRET": "cs"}

func TestLoadOIDC(t *testing.T) {
	y := oidcHead + oidcWallet + oidcCorp("corp", "https://idp.example.com/", "cid", "CORP_SECRET") +
		"allowed_users:\n  github: [a]\n  oidc:\n    wallet: [\" https://ilp.interledger-test.dev/Alice \"]\n    corp: [Bob@Example.com]\n"
	c, err := Load(write(t, y), env(oidcEnv))
	if err != nil {
		t.Fatal(err)
	}
	w, corp := c.OIDCProvider("wallet"), c.OIDCProvider("corp")
	if w == nil || corp == nil || c.OIDCProvider("nope") != nil {
		t.Fatalf("OIDCProvider lookup: %v %v", w, corp)
	}
	if w.Label != "wallet" || !reflect.DeepEqual(w.Scopes, []string{"openid"}) || w.UserClaim != "sub" || w.ClientSecret != "ws" {
		t.Fatalf("wallet = %+v", w)
	}
	if corp.Issuer != "https://idp.example.com/" || !reflect.DeepEqual(corp.Scopes, []string{"openid", "email"}) || corp.UserClaim != "email" {
		t.Fatalf("corp = %+v", corp)
	}
	if got := c.AllowedUsers.OIDC["wallet"]; !reflect.DeepEqual(got, []string{"https://ilp.interledger-test.dev/Alice"}) {
		t.Fatalf("wallet allowlist = %q", got)
	}
	if got := c.AllowedUsers.OIDC["corp"]; !reflect.DeepEqual(got, []string{"bob@example.com"}) {
		t.Fatalf("corp allowlist = %q", got)
	}
}

func TestLoadOIDCOnly(t *testing.T) {
	y := "public_url: https://mcp.example.com\ndata_dir: /d\nproviders:\n  oidc:\n" + oidcWallet
	if _, err := Load(write(t, y), env(oidcEnv)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOIDCScopesKeepOpenIDFirst(t *testing.T) {
	y := "public_url: https://mcp.example.com\ndata_dir: /d\nproviders:\n  oidc:\n" + oidcWallet + "      scopes: [profile, openid]\n"
	c, err := Load(write(t, y), env(oidcEnv))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Providers.OIDC[0].Scopes; !reflect.DeepEqual(got, []string{"openid", "profile"}) {
		t.Fatalf("scopes = %q", got)
	}
}

func TestLoadOIDCRejects(t *testing.T) {
	good := func(name, issuer, clientID, secretEnv string) string {
		return oidcHead + oidcWallet + oidcCorp(name, issuer, clientID, secretEnv) + "allowed_users:\n  github: [a]\n"
	}
	base := func(issuer string) string { return good("corp", issuer, "cid", "CORP_SECRET") }
	if _, err := Load(write(t, base("https://idp.example.com/")), env(oidcEnv)); err != nil {
		t.Fatalf("base case must load: %v", err)
	}
	cases := []struct{ name, yaml, field string }{
		{"name empty", good(`""`, "https://idp.example.com/", "cid", "CORP_SECRET"), "providers.oidc[1].name"},
		{"name bad", good(`"Bad Name"`, "https://idp.example.com/", "cid", "CORP_SECRET"), "providers.oidc[1].name"},
		{"name duplicate", good("wallet", "https://idp.example.com/", "cid", "CORP_SECRET"), "providers.oidc[1].name"},
		{"name github", good("github", "https://idp.example.com/", "cid", "CORP_SECRET"), "providers.oidc[1].name"},
		{"issuer empty", base(`""`), "providers.oidc[1].issuer"},
		{"issuer no scheme", base("idp.example.com"), "providers.oidc[1].issuer"},
		{"issuer query", base(`"https://idp.example.com?x=1"`), "providers.oidc[1].issuer"},
		{"issuer fragment", base(`"https://idp.example.com#f"`), "providers.oidc[1].issuer"},
		{"issuer userinfo", base("https://u:p@idp.example.com"), "providers.oidc[1].issuer"},
		{"issuer http", base("http://idp.example.com"), "providers.oidc[1].issuer"},
		{"issuer http lookalike loopback", base("http://localhost.example.com"), "providers.oidc[1].issuer"},
		{"issuer duplicate modulo slash", oidcHead + oidcCorp("first", "https://idp.example.com/", "cid", "CORP_SECRET") + oidcCorp("second", "https://idp.example.com", "cid", "CORP_SECRET"), "providers.oidc[1].issuer"},
		{"client_id empty", good("corp", "https://idp.example.com/", `""`, "CORP_SECRET"), "providers.oidc[1].client_id"},
		{"secret env empty", good("corp", "https://idp.example.com/", "cid", `""`), "providers.oidc[1].client_secret_env"},
		{"secret env unset", good("corp", "https://idp.example.com/", "cid", "UNSET_VAR"), "providers.oidc[1].client_secret_env"},
		{"allowlist unknown provider", base("https://idp.example.com/") + "  oidc:\n    nope: [x]\n", "allowed_users.oidc.nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.yaml), env(oidcEnv))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error %q does not name %q", err, tc.field)
			}
		})
	}
}

func TestLoadOIDCLoopbackHTTP(t *testing.T) {
	for _, issuer := range []string{"http://127.0.0.1:3000", "http://[::1]:3000", "http://localhost"} {
		y := "public_url: https://mcp.example.com\ndata_dir: /d\nproviders:\n  oidc:\n" + strings.Replace(oidcWallet, "http://localhost:3000", issuer, 1)
		c, err := Load(write(t, y), env(oidcEnv))
		if err != nil {
			t.Fatalf("%s: %v", issuer, err)
		}
		if c.Providers.OIDC[0].Issuer != issuer {
			t.Fatalf("issuer changed: %q", c.Providers.OIDC[0].Issuer)
		}
	}
}

func oidcTestConfig() *Config {
	return &Config{
		Providers: ProvidersConfig{OIDC: []OIDCConfig{
			{Name: "wallet", Label: "Open Payments wallet", UserClaim: "sub"},
			{Name: "corp", Label: "Corp", UserClaim: "email"},
		}},
		AllowedUsers: AllowedUsers{OIDC: map[string][]string{
			"wallet": {"https://w.example/alice", "alice"},
			"corp":   {"bob@example.com"},
		}},
	}
}

func TestIsAllowedOIDC(t *testing.T) {
	c := oidcTestConfig()
	for _, id := range []string{"oidc:wallet:https://w.example/alice", "oidc:corp:bob@example.com"} {
		if !c.IsAllowed(id) {
			t.Fatalf("expected %q allowed", id)
		}
	}
	for _, id := range []string{
		"oidc:wallet:https://w.example/Alice", "oidc:corp:alice", "oidc:other:alice", "github:alice",
	} {
		if c.IsAllowed(id) {
			t.Fatalf("expected %q denied", id)
		}
	}
	// Allowlist entries for a provider that isn't configured never admit.
	c.Providers.OIDC = c.Providers.OIDC[:1]
	if c.IsAllowed("oidc:corp:bob@example.com") {
		t.Fatal("unconfigured provider admitted")
	}
}

func TestCanonicalUser(t *testing.T) {
	c := oidcTestConfig()
	for in, want := range map[string]string{
		"oidc:corp:Bob@Example.com":        "oidc:corp:bob@example.com",
		"oidc:wallet:https://w.example/Al": "oidc:wallet:https://w.example/Al",
		"oidc:other:Bob@Example.com":       "oidc:other:Bob@Example.com",
		"github:alice":                     "github:alice",
	} {
		if got := c.CanonicalUser(in); got != want {
			t.Fatalf("CanonicalUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDescribeAndLabelOIDC(t *testing.T) {
	c := oidcTestConfig()
	if got := c.ProviderLabel(identity.OIDCProvider("wallet")); got != "Open Payments wallet" {
		t.Fatalf("label = %q", got)
	}
	if got := c.ProviderLabel(identity.GitHub); got != "GitHub" {
		t.Fatalf("label = %q", got)
	}
	if got := c.ProviderLabel(identity.OIDCProvider("other")); got != "oidc:other" {
		t.Fatalf("unknown label = %q", got)
	}
	if got := c.DescribeUser("oidc:wallet:https://w.example/a"); got != "Open Payments wallet user https://w.example/a" {
		t.Fatalf("describe = %q", got)
	}
	if got := c.DescribeUser("github:alice"); got != "GitHub account alice" {
		t.Fatalf("describe = %q", got)
	}
	if got := c.DescribeUser("weird"); got != "weird" {
		t.Fatalf("describe = %q", got)
	}
}
