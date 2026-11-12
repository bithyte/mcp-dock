// Package identity defines provider-prefixed user ids such as
// "github:cozmin" and "open_payments:https://wallet.example/alice".
package identity

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type Provider string

const (
	GitHub         Provider = "github"
	OpenPayments   Provider = "open_payments"
	walletFallback          = "/.well-known/pay"
)

// GitHubUser returns the user id for a GitHub login.
func GitHubUser(login string) string {
	return string(GitHub) + ":" + strings.ToLower(login)
}

// WalletUser returns the user id for an already normalized wallet address.
func WalletUser(normalizedWallet string) string {
	return string(OpenPayments) + ":" + normalizedWallet
}

// Split breaks an id on its first colon. ok is false when there is no colon,
// the name is empty or the provider is unknown.
func Split(id string) (p Provider, name string, ok bool) {
	prefix, name, found := strings.Cut(id, ":")
	if !found || name == "" {
		return "", "", false
	}
	switch Provider(prefix) {
	case GitHub, OpenPayments:
		return Provider(prefix), name, true
	}
	return "", "", false
}

// NormalizeWalletAddress turns a payment pointer or https URL into its
// canonical https form. Path case is kept; scheme and host are lowercased.
func NormalizeWalletAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "$") {
		s = "https://" + s[1:]
	}
	if strings.ContainsAny(s, "?#") {
		return "", errors.New("wallet address must not have a query or fragment")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("invalid wallet address")
	}
	if u.Scheme != "https" {
		return "", errors.New("wallet address must use https")
	}
	if u.Opaque != "" || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid wallet address")
	}
	if u.User != nil {
		return "", errors.New("wallet address must not contain credentials")
	}
	host := strings.ToLower(u.Host)
	host = strings.TrimSuffix(host, ":443")
	if u.Hostname() == "" || host == "" {
		return "", errors.New("wallet address has no host")
	}
	path := u.EscapedPath()
	if path == "" || path == "/" {
		path = walletFallback
	} else {
		path = strings.TrimSuffix(path, "/")
	}
	return "https://" + host + path, nil
}

// Label is the display name of a provider.
func Label(p Provider) string {
	switch p {
	case GitHub:
		return "GitHub"
	case OpenPayments:
		return "Open Payments"
	}
	return string(p)
}

// Describe is a human readable description of a user id. Unknown ids are
// returned unchanged.
func Describe(id string) string {
	p, name, ok := Split(id)
	if !ok {
		return id
	}
	switch p {
	case GitHub:
		return "GitHub account " + name
	case OpenPayments:
		return "Open Payments wallet address " + name
	}
	return id
}

// ParseCLIUser parses a user argument from the command line into a user id.
func ParseCLIUser(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", errors.New("empty user")
	}
	wallet := func(s string) (string, error) {
		n, err := NormalizeWalletAddress(s)
		if err != nil {
			return "", err
		}
		return WalletUser(n), nil
	}
	switch {
	case strings.HasPrefix(arg, string(OpenPayments)+":"):
		return wallet(strings.TrimPrefix(arg, string(OpenPayments)+":"))
	case strings.HasPrefix(arg, string(GitHub)+":"):
		login := strings.TrimSpace(strings.TrimPrefix(arg, string(GitHub)+":"))
		if login == "" {
			return "", errors.New("empty github login")
		}
		return GitHubUser(login), nil
	case strings.HasPrefix(arg, "$"), strings.HasPrefix(arg, "https://"):
		return wallet(arg)
	case strings.Contains(arg, ":"):
		return "", fmt.Errorf("unrecognized user %q (use a github login, github:<login>, or an Open Payments wallet address)", arg)
	}
	return GitHubUser(arg), nil
}
