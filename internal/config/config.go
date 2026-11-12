// Package config loads and validates the mcp-dock YAML config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/bithyte/mcp-dock/internal/identity"
)

type Config struct {
	PublicURL    string           `yaml:"public_url"`
	Listen       string           `yaml:"listen"`
	DataDir      string           `yaml:"data_dir"`
	GitHub       GitHubConfig     `yaml:"github"`
	AllowedUsers []string         `yaml:"allowed_users"`
	Upstreams    []UpstreamConfig `yaml:"upstreams"`
}

type GitHubConfig struct {
	ClientID        string `yaml:"client_id"`
	ClientSecretEnv string `yaml:"client_secret_env"`
	ClientSecret    string `yaml:"-"`
}

type UpstreamConfig struct {
	Name     string            `yaml:"name"`
	Command  string            `yaml:"command"`
	Args     []string          `yaml:"args"`
	Env      map[string]string `yaml:"env"`
	EnvFrom  []string          `yaml:"env_from"`
	DenyArgs []string          `yaml:"deny_args"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*$`)

// NormalizeURL trims trailing slashes.
func NormalizeURL(s string) string { return strings.TrimRight(s, "/") }

// Load reads, parses and validates the config at path. Secrets are resolved via getenv.
func Load(path string, getenv func(string) string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.validate(getenv); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate(getenv func(string) string) error {
	c.PublicURL = NormalizeURL(strings.TrimSpace(c.PublicURL))
	if c.PublicURL == "" {
		return errors.New("public_url is required")
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("public_url must be an http(s) URL, got %q", c.PublicURL)
	}
	// The OAuth routes are served at the root and the login cookie is
	// scoped to /oauth/, so a public_url with a path can't work.
	if u.Path != "" || u.RawPath != "" || u.Opaque != "" || strings.ContainsAny(c.PublicURL, "?#") {
		return fmt.Errorf("public_url must be a bare origin like https://host[:port], with no path, query or fragment; got %q", c.PublicURL)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		return errors.New("data_dir is required")
	}
	if c.GitHub.ClientID == "" {
		return errors.New("github.client_id is required")
	}
	if c.GitHub.ClientSecretEnv == "" {
		return errors.New("github.client_secret_env is required")
	}
	c.GitHub.ClientSecret = getenv(c.GitHub.ClientSecretEnv)
	if c.GitHub.ClientSecret == "" {
		return fmt.Errorf("github.client_secret_env: env var %s is empty or unset", c.GitHub.ClientSecretEnv)
	}
	seen := map[string]bool{}
	for i, up := range c.Upstreams {
		p := fmt.Sprintf("upstreams[%d]", i)
		if !nameRe.MatchString(up.Name) || strings.Contains(up.Name, "__") {
			return fmt.Errorf("%s.name %q invalid: use lowercase letters, digits, single _ or -", p, up.Name)
		}
		if seen[up.Name] {
			return fmt.Errorf("%s.name %q is a duplicate", p, up.Name)
		}
		seen[up.Name] = true
		if up.Command == "" {
			return fmt.Errorf("%s.command is required", p)
		}
		// Upstreams run in an empty working dir, so a relative path like
		// ./bin/x would resolve against that dir and never be found.
		if strings.Contains(up.Command, "/") && !filepath.IsAbs(up.Command) {
			return fmt.Errorf("%s.command %q must be absolute or on PATH (a bare name); relative paths don't work because upstreams run in an empty working dir", p, up.Command)
		}
		for _, v := range up.EnvFrom {
			if getenv(v) == "" {
				return fmt.Errorf("%s.env_from: env var %s is empty or unset", p, v)
			}
		}
		for _, d := range up.DenyArgs {
			if !validDenyPath(d) {
				return fmt.Errorf("%s.deny_args entry %q invalid: use dot-separated property names with no empty parts, spaces or control characters", p, d)
			}
		}
	}
	return nil
}

// validDenyPath reports whether d is a dotted path whose every segment is
// non-empty and free of whitespace and control characters. A typo such as
// "attachment." or " attachment.path" would otherwise deny nothing.
func validDenyPath(d string) bool {
	for _, seg := range strings.Split(d, ".") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError {
				return false
			}
		}
	}
	return true
}

// IsAllowed reports whether the user id is on the allowlist. For now only
// GitHub ids are recognized, matched case-insensitively.
func (c *Config) IsAllowed(userID string) bool {
	p, name, ok := identity.Split(userID)
	if !ok || p != identity.GitHub {
		return false
	}
	for _, u := range c.AllowedUsers {
		if strings.EqualFold(u, name) {
			return true
		}
	}
	return false
}

// ResourceURL is the MCP endpoint, also the OAuth resource.
func (c *Config) ResourceURL() string { return c.PublicURL + "/mcp" }

// Environ builds the minimal upstream process env as "K=V" entries.
func (u UpstreamConfig) Environ(getenv func(string) string) []string {
	m := map[string]string{}
	for _, k := range []string{"PATH", "HOME"} {
		if v := getenv(k); v != "" {
			m[k] = v
		}
	}
	for k, v := range u.Env {
		m[k] = v
	}
	for _, k := range u.EnvFrom {
		m[k] = getenv(k)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
