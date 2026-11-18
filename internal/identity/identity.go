// Package identity defines provider-prefixed user ids such as
// "github:alice" and "oidc:wallet:https://wallet.example/alice".
package identity

import (
	"errors"
	"fmt"
	"strings"
)

type Provider string

const GitHub Provider = "github"

// OIDCPrefix starts every OIDC provider and user id: "oidc:<name>[:<value>]".
const OIDCPrefix = "oidc"

// OIDCProvider returns the provider for a configured OIDC provider name.
func OIDCProvider(name string) Provider {
	return Provider(OIDCPrefix + ":" + name)
}

// OIDCUser returns the user id for a claim value at an OIDC provider. The
// value is used as is; callers normalize it first.
func OIDCUser(name, value string) string {
	return string(OIDCProvider(name)) + ":" + value
}

// OIDCName returns the configured name of an OIDC provider. ok is false for
// any other provider.
func (p Provider) OIDCName() (name string, ok bool) {
	name, found := strings.CutPrefix(string(p), OIDCPrefix+":")
	if !found || name == "" || strings.Contains(name, ":") {
		return "", false
	}
	return name, true
}

// GitHubUser returns the user id for a GitHub login.
func GitHubUser(login string) string {
	return string(GitHub) + ":" + strings.ToLower(login)
}

// Split breaks an id on its first colon. OIDC ids ("oidc:<name>:<value>") are
// cut on the first two colons, so the value may contain colons. ok is false
// when there is no colon, the name is empty or the provider is unknown.
func Split(id string) (p Provider, name string, ok bool) {
	if rest, isOIDC := strings.CutPrefix(id, OIDCPrefix+":"); isOIDC {
		provName, value, found := strings.Cut(rest, ":")
		if !found || provName == "" || value == "" {
			return "", "", false
		}
		return OIDCProvider(provName), value, true
	}
	prefix, name, found := strings.Cut(id, ":")
	if !found || name == "" {
		return "", "", false
	}
	if Provider(prefix) != GitHub {
		return "", "", false
	}
	return GitHub, name, true
}

// ParseCLIUser parses a user argument from the command line into a user id.
func ParseCLIUser(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", errors.New("empty user")
	}
	switch {
	case strings.HasPrefix(arg, string(GitHub)+":"):
		login := strings.TrimSpace(strings.TrimPrefix(arg, string(GitHub)+":"))
		if login == "" {
			return "", errors.New("empty github login")
		}
		return GitHubUser(login), nil
	case strings.HasPrefix(arg, OIDCPrefix+":"):
		name, value, _ := strings.Cut(strings.TrimPrefix(arg, OIDCPrefix+":"), ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "" || value == "" {
			return "", errors.New("oidc user must be oidc:<provider>:<value>")
		}
		return OIDCUser(name, value), nil
	case strings.Contains(arg, ":"), strings.HasPrefix(arg, "$"):
		return "", fmt.Errorf("unrecognized user %q (use a github login, github:<login> or oidc:<provider>:<value>)", arg)
	}
	return GitHubUser(arg), nil
}
