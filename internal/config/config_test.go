package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bithyte/mcp-dock/internal/oplogin"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

const validYAML = `
public_url: https://mcp.example.com/
data_dir: /var/lib/mcp-dock
providers:
  github:
    client_id: Ov23liXXXXXXXXXXXXXX
    client_secret_env: GITHUB_CLIENT_SECRET
  open_payments:
    wallet_address: $ilp.interledger-test.dev/mcp
    key_id: key-1
    private_key_env: OP_PRIVATE_KEY
allowed_users:
  github: [Alice]
  open_payments: ["$ilp.interledger-test.dev/alice"]
upstreams:
  - name: jaypost
    command: jaypost
    args: ["mcp"]
    env:
      JAYPOST_URL: http://127.0.0.1:8787
    env_from: [JAYPOST_TOKEN]
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

var goodPEM, _ = testutil.Ed25519PEM(&testing.T{})

var goodEnv = map[string]string{"GITHUB_CLIENT_SECRET": "s", "JAYPOST_TOKEN": "t", "OP_PRIVATE_KEY": goodPEM}

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
	if c.AllowedUsers.OpenPayments[0] != "https://ilp.interledger-test.dev/alice" {
		t.Fatalf("allowlist not normalized: %v", c.AllowedUsers.OpenPayments)
	}
	if c.Providers.OpenPayments.WalletAddress != "https://ilp.interledger-test.dev/mcp" || c.Providers.OpenPayments.PrivateKey != strings.TrimSpace(goodPEM) {
		t.Fatalf("bad open_payments: %+v", c.Providers.OpenPayments)
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
		{"secret env empty", validYAML, "client_secret_env", map[string]string{"JAYPOST_TOKEN": "t"}},
		{"name uppercase", r("name: jaypost", "name: Jay").Replace(validYAML), "name", goodEnv},
		{"name double underscore", r("name: jaypost", "name: jay__post").Replace(validYAML), "name", goodEnv},
		{"name empty", r("name: jaypost", "name: \"\"").Replace(validYAML), "name", goodEnv},
		{"name trailing dash", r("name: jaypost", "name: a-").Replace(validYAML), "name", goodEnv},
		{"duplicate names", validYAML + "  - name: jaypost\n    command: x\n", "duplicate", goodEnv},
		{"empty command", r("command: jaypost", "command: \"\"").Replace(validYAML), "command", goodEnv},
		{"relative command dot", r("command: jaypost", "command: ./bin/jaypost").Replace(validYAML), "absolute or on PATH", goodEnv},
		{"relative command dir", r("command: jaypost", "command: bin/jaypost").Replace(validYAML), "absolute or on PATH", goodEnv},
		{"relative command parent", r("command: jaypost", "command: ../jaypost").Replace(validYAML), "absolute or on PATH", goodEnv},
		{"env_from unset", validYAML, "JAYPOST_TOKEN", map[string]string{"GITHUB_CLIENT_SECRET": "s", "OP_PRIVATE_KEY": goodPEM}},
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
		{"no providers", noProviders, "providers", goodEnv},
		{"wallet http", r("$ilp.interledger-test.dev/mcp", "http://ilp.example/mcp").Replace(validYAML), "wallet_address", goodEnv},
		{"wallet empty", r("wallet_address: $ilp.interledger-test.dev/mcp", "wallet_address: \"\"").Replace(validYAML), "wallet_address", goodEnv},
		{"key_id empty", r("key_id: key-1", "key_id: \"\"").Replace(validYAML), "key_id", goodEnv},
		{"key env unset", validYAML, "private_key_env", map[string]string{"GITHUB_CLIENT_SECRET": "s", "JAYPOST_TOKEN": "t"}},
		{"key env file path", validYAML, "private_key_env", withKey(keyFile(t))},
		{"key env rsa", validYAML, "private_key_env", withKey(rsaPEM(t))},
		{"key env ecdsa", validYAML, "private_key_env", withKey(ecPEM(t))},
		{"key env garbage", validYAML, "private_key_env", withKey("!!!notbase64!!!")},
		{"key env base64 non pem", validYAML, "private_key_env", withKey(base64.StdEncoding.EncodeToString([]byte("hello")))},
		{"allowed op http", r("$ilp.interledger-test.dev/alice", "http://w.example/a").Replace(validYAML), "allowed_users.open_payments", goodEnv},
		{"allowed github without provider", r("  github:\n    client_id: Ov23liXXXXXXXXXXXXXX\n    client_secret_env: GITHUB_CLIENT_SECRET\n", "").Replace(validYAML), "allowed_users.github", goodEnv},
		{"allowed op without provider", opOnlyWithGitHubAllow, "allowed_users.github", goodEnv},
		{"allowed op without op provider", githubOnlyWithOPAllow, "allowed_users.open_payments", goodEnv},
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
	c := &Config{AllowedUsers: AllowedUsers{
		GitHub:       []string{"Alice"},
		OpenPayments: []string{"https://ilp.interledger-test.dev/alice"},
	}}
	for _, id := range []string{"github:alice", "github:ALICE", "open_payments:https://ilp.interledger-test.dev/alice"} {
		if !c.IsAllowed(id) {
			t.Fatalf("expected %q allowed", id)
		}
	}
	for _, id := range []string{
		"github:other", "alice", "open_payments:alice",
		"open_payments:https://ilp.interledger-test.dev/Alice",
		"github:https://ilp.interledger-test.dev/alice",
		"open_payments:Alice", "",
	} {
		if c.IsAllowed(id) {
			t.Fatalf("expected %q denied", id)
		}
	}
}

func TestLoadProviderCombos(t *testing.T) {
	for name, y := range map[string]string{"github only": githubOnly, "op only": opOnly} {
		if _, err := Load(write(t, y), env(goodEnv)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestLoadAcceptsBase64Key(t *testing.T) {
	e := withKey(base64.StdEncoding.EncodeToString([]byte(goodPEM)))
	c, err := Load(write(t, validYAML), env(e))
	if err != nil {
		t.Fatal(err)
	}
	if c.Providers.OpenPayments.PrivateKey != strings.TrimSpace(goodPEM) {
		t.Fatal("PrivateKey should be the decoded PEM")
	}
}

func TestLoadNormalizesPaddedKey(t *testing.T) {
	for name, v := range map[string]string{
		"pem":    "  \n" + goodPEM + "\n  ",
		"base64": "  " + base64.StdEncoding.EncodeToString([]byte(goodPEM)) + " \n",
	} {
		c, err := Load(write(t, validYAML), env(withKey(v)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		op := c.Providers.OpenPayments
		if op.PrivateKey != strings.TrimSpace(goodPEM) {
			t.Fatalf("%s: PrivateKey not normalized", name)
		}
		if _, err := oplogin.New(oplogin.Config{WalletAddress: op.WalletAddress, KeyID: op.KeyID, PrivateKey: op.PrivateKey}); err != nil {
			t.Fatalf("%s: oplogin.New: %v", name, err)
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
}

func TestKeyErrorsDoNotLeakValue(t *testing.T) {
	secret := "SUPERSECRETVALUE123"
	_, err := Load(write(t, validYAML), env(withKey(secret)))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
}

const head = "public_url: https://mcp.example.com\ndata_dir: /d\n"
const ghBlock = "  github:\n    client_id: x\n    client_secret_env: GITHUB_CLIENT_SECRET\n"
const opBlock = "  open_payments:\n    wallet_address: https://ilp.example/mcp\n    key_id: k\n    private_key_env: OP_PRIVATE_KEY\n"

var (
	noProviders           = head + "allowed_users:\n  github: [a]\n"
	githubOnly            = head + "providers:\n" + ghBlock + "allowed_users:\n  github: [a]\n"
	opOnly                = head + "providers:\n" + opBlock + "allowed_users:\n  open_payments: [\"$ilp.example/a\"]\n"
	opOnlyWithGitHubAllow = head + "providers:\n" + opBlock + "allowed_users:\n  github: [a]\n"
	githubOnlyWithOPAllow = head + "providers:\n" + ghBlock + "allowed_users:\n  open_payments: [\"$ilp.example/a\"]\n"
)

func withKey(v string) map[string]string {
	return map[string]string{"GITHUB_CLIENT_SECRET": "s", "JAYPOST_TOKEN": "t", "OP_PRIVATE_KEY": v}
}

func keyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(p, []byte(goodPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func rsaPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func ecPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestEnvironMinimal(t *testing.T) {
	c, err := Load(write(t, validYAML), env(map[string]string{
		"PATH": "/bin", "HOME": "/h", "JAYPOST_TOKEN": "t", "GITHUB_CLIENT_SECRET": "s", "OP_PRIVATE_KEY": goodPEM,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Upstreams[0].Environ(env(map[string]string{
		"PATH": "/bin", "HOME": "/h", "JAYPOST_TOKEN": "t", "GITHUB_CLIENT_SECRET": "s", "OP_PRIVATE_KEY": goodPEM,
	}))
	set := map[string]bool{}
	for _, kv := range got {
		set[kv] = true
	}
	for _, want := range []string{"PATH=/bin", "HOME=/h", "JAYPOST_URL=http://127.0.0.1:8787", "JAYPOST_TOKEN=t"} {
		if !set[want] {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "GITHUB_CLIENT_SECRET") {
			t.Errorf("leaked %q", kv)
		}
	}
}

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
		{"command bare", validYAML},
		{"command absolute", r("command: jaypost", "command: /usr/local/bin/jaypost").Replace(validYAML)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(write(t, tc.yaml), env(goodEnv)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
