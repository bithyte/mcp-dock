package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
public_url: https://mcp.cozminu.com/
data_dir: /var/lib/mcp-dock
github:
  client_id: Iv1.xxxxx
  client_secret_env: GITHUB_CLIENT_SECRET
allowed_users: [Cozmin]
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

var goodEnv = map[string]string{"GITHUB_CLIENT_SECRET": "s", "JAYPOST_TOKEN": "t"}

func TestLoadValid(t *testing.T) {
	c, err := Load("../../config.example.yaml", env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.GitHub.ClientSecret != "s" || len(c.Upstreams) != 1 {
		t.Fatalf("bad config: %+v", c)
	}
	c, err = Load(write(t, validYAML), env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://mcp.cozminu.com" {
		t.Fatalf("PublicURL = %q", c.PublicURL)
	}
	if c.ResourceURL() != "https://mcp.cozminu.com/mcp" {
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
		{"missing public_url", r("public_url: https://mcp.cozminu.com/\n", "").Replace(validYAML), "public_url", goodEnv},
		{"public_url scheme", r("https://mcp.cozminu.com/", "ftp://x").Replace(validYAML), "public_url", goodEnv},
		{"missing data_dir", r("data_dir: /var/lib/mcp-dock\n", "").Replace(validYAML), "data_dir", goodEnv},
		{"missing client_id", r("client_id: Iv1.xxxxx", "client_id: \"\"").Replace(validYAML), "client_id", goodEnv},
		{"secret env empty", validYAML, "client_secret_env", map[string]string{"JAYPOST_TOKEN": "t"}},
		{"name uppercase", r("name: jaypost", "name: Jay").Replace(validYAML), "name", goodEnv},
		{"name double underscore", r("name: jaypost", "name: jay__post").Replace(validYAML), "name", goodEnv},
		{"name empty", r("name: jaypost", "name: \"\"").Replace(validYAML), "name", goodEnv},
		{"name trailing dash", r("name: jaypost", "name: a-").Replace(validYAML), "name", goodEnv},
		{"duplicate names", validYAML + "  - name: jaypost\n    command: x\n", "duplicate", goodEnv},
		{"empty command", r("command: jaypost", "command: \"\"").Replace(validYAML), "command", goodEnv},
		{"env_from unset", validYAML, "JAYPOST_TOKEN", map[string]string{"GITHUB_CLIENT_SECRET": "s"}},
		{"deny_args empty", r("[attachment.path]", "[\"\"]").Replace(validYAML), "deny_args", goodEnv},
		{"deny_args dotdot", r("[attachment.path]", "[a..b]").Replace(validYAML), "deny_args", goodEnv},
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

func TestIsAllowedCaseInsensitive(t *testing.T) {
	c := &Config{AllowedUsers: []string{"Cozmin"}}
	if !c.IsAllowed("cozmin") || !c.IsAllowed("COZMIN") {
		t.Fatal("expected allowed")
	}
	if c.IsAllowed("other") {
		t.Fatal("expected denied")
	}
}

func TestEnvironMinimal(t *testing.T) {
	c, err := Load(write(t, validYAML), env(map[string]string{
		"PATH": "/bin", "HOME": "/h", "JAYPOST_TOKEN": "t", "GITHUB_CLIENT_SECRET": "s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Upstreams[0].Environ(env(map[string]string{
		"PATH": "/bin", "HOME": "/h", "JAYPOST_TOKEN": "t", "GITHUB_CLIENT_SECRET": "s",
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
