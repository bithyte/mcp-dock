// Package config loads and validates the mcp-dock YAML config.
package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
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
	Providers    ProvidersConfig  `yaml:"providers"`
	AllowedUsers AllowedUsers     `yaml:"allowed_users"`
	Upstreams    []UpstreamConfig `yaml:"upstreams"`
}

type ProvidersConfig struct {
	GitHub       *GitHubConfig       `yaml:"github"`
	OpenPayments *OpenPaymentsConfig `yaml:"open_payments"`
}

type OpenPaymentsConfig struct {
	WalletAddress string `yaml:"wallet_address"` // normalized at load
	KeyID         string `yaml:"key_id"`
	PrivateKeyEnv string `yaml:"private_key_env"`
	// PrivateKey is the env value as given (PEM text or base64 of the PEM),
	// checked at load to hold an Ed25519 PKCS#8 key.
	PrivateKey string `yaml:"-"`
}

// AllowedUsers lists who may log in, per provider.
type AllowedUsers struct {
	GitHub       []string `yaml:"github"`
	OpenPayments []string `yaml:"open_payments"` // normalized at load
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
	if err := rejectOldShape(data); err != nil {
		return nil, err
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
	if err := c.validateProviders(getenv); err != nil {
		return err
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

// rejectOldShape gives a targeted error for the pre-providers config layout.
func rejectOldShape(data []byte) error {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil // the strict decode reports syntax errors
	}
	if _, ok := raw["github"]; ok {
		return errors.New("config: github moved to providers.github")
	}
	if _, ok := raw["allowed_users"].([]any); ok {
		return errors.New("config: allowed_users is now per provider, e.g. allowed_users.github: [...]")
	}
	return nil
}

func (c *Config) validateProviders(getenv func(string) string) error {
	gh, op := c.Providers.GitHub, c.Providers.OpenPayments
	if gh == nil && op == nil {
		return errors.New("providers: configure at least one of providers.github or providers.open_payments")
	}
	if gh != nil {
		if gh.ClientID == "" {
			return errors.New("providers.github.client_id is required")
		}
		if gh.ClientSecretEnv == "" {
			return errors.New("providers.github.client_secret_env is required")
		}
		gh.ClientSecret = getenv(gh.ClientSecretEnv)
		if gh.ClientSecret == "" {
			return fmt.Errorf("providers.github.client_secret_env: env var %s is empty or unset", gh.ClientSecretEnv)
		}
	} else if len(c.AllowedUsers.GitHub) > 0 {
		return errors.New("allowed_users.github is set but providers.github is not configured")
	}
	if op != nil {
		w, err := identity.NormalizeWalletAddress(op.WalletAddress)
		if err != nil {
			return fmt.Errorf("providers.open_payments.wallet_address: %w", err)
		}
		op.WalletAddress = w
		if strings.TrimSpace(op.KeyID) == "" {
			return errors.New("providers.open_payments.key_id is required")
		}
		if op.PrivateKeyEnv == "" {
			return errors.New("providers.open_payments.private_key_env is required")
		}
		op.PrivateKey = getenv(op.PrivateKeyEnv)
		if err := checkEd25519Key(op.PrivateKey); err != nil {
			return fmt.Errorf("providers.open_payments.private_key_env: env var %s %w", op.PrivateKeyEnv, err)
		}
	} else if len(c.AllowedUsers.OpenPayments) > 0 {
		return errors.New("allowed_users.open_payments is set but providers.open_payments is not configured")
	}
	for i, e := range c.AllowedUsers.OpenPayments {
		n, err := identity.NormalizeWalletAddress(e)
		if err != nil {
			return fmt.Errorf("allowed_users.open_payments[%d]: %w", i, err)
		}
		c.AllowedUsers.OpenPayments[i] = n
	}
	return nil
}

// checkEd25519Key checks v is PEM text, or base64 of PEM text, holding an
// Ed25519 PKCS#8 key. The returned error never includes v.
func checkEd25519Key(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("is empty or unset")
	}
	if !strings.HasPrefix(v, "-----BEGIN") {
		dec, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return errors.New("must be PEM text or base64 of PEM text, not a file path")
		}
		v = strings.TrimSpace(string(dec))
		if !strings.HasPrefix(v, "-----BEGIN") {
			return errors.New("must be PEM text or base64 of PEM text, not a file path")
		}
	}
	block, _ := pem.Decode([]byte(v))
	if block == nil {
		return errors.New("does not hold a valid PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return errors.New("does not hold a PKCS#8 private key")
	}
	if _, ok := key.(ed25519.PrivateKey); !ok {
		return errors.New("must hold an Ed25519 key")
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

// IsAllowed reports whether the user id is on the allowlist. GitHub logins
// match case-insensitively; wallet addresses match exactly against the
// normalized entries.
func (c *Config) IsAllowed(userID string) bool {
	p, name, ok := identity.Split(userID)
	if !ok {
		return false
	}
	switch p {
	case identity.GitHub:
		for _, u := range c.AllowedUsers.GitHub {
			if strings.EqualFold(u, name) {
				return true
			}
		}
	case identity.OpenPayments:
		for _, u := range c.AllowedUsers.OpenPayments {
			if u == name {
				return true
			}
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
