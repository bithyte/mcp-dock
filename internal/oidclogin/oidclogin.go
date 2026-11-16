// Package oidclogin signs users in through one OpenID Connect provider:
// lazy discovery, an authorization code flow with PKCE (S256) and nonce, and
// ID token verification. It returns the value of the configured user claim.
//
// Nothing here logs. Errors never carry the client secret, the code, state,
// nonce, PKCE verifier, or any token, and never wrap token endpoint bodies.
package oidclogin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const defaultTimeout = 15 * time.Second

var (
	// ErrUnavailable: discovery failed, the network failed, or the token
	// endpoint refused the exchange.
	ErrUnavailable = errors.New("oidc provider unavailable")
	// ErrInvalidToken: the ID token is missing or fails verification
	// (signature, iss, aud, exp, azp, nonce).
	ErrInvalidToken = errors.New("oidc id token invalid")
	// ErrClaim: the user claim is missing, not a non-empty string, or (for
	// email) not verified.
	ErrClaim = errors.New("oidc user claim missing or unverified")
)

// Config describes one OIDC provider.
type Config struct {
	Name, Issuer, ClientID, ClientSecret string
	Scopes                               []string // already defaulted by config
	UserClaim                            string
	HTTP                                 *http.Client     // default: &http.Client{Timeout: 15 * time.Second}
	Now                                  func() time.Time // for ID token expiry; default time.Now
}

// Pending is stored in the auth request's pending blob. Secret; never log.
type Pending struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
}

// Client is a login client for one provider. Safe for concurrent use.
type Client struct {
	c Config

	mu       sync.Mutex
	provider *oidc.Provider // nil until discovery succeeds
}

// New returns a client for c. It makes no network calls.
func New(c Config) *Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: defaultTimeout}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	c.Scopes = append([]string(nil), c.Scopes...)
	return &Client{c: c}
}

// Name returns the configured provider name.
func (c *Client) Name() string { return c.c.Name }

// discover returns the cached provider, fetching it on first use. Failures
// aren't cached; concurrent first calls may both fetch.
func (c *Client) discover() (*oidc.Provider, error) {
	c.mu.Lock()
	p := c.provider
	c.mu.Unlock()
	if p != nil {
		return p, nil
	}
	// Background, not the request context: the provider's remote key set
	// outlives this request. The HTTP client's timeout bounds the fetch.
	p, err := oidc.NewProvider(oidc.ClientContext(context.Background(), c.c.HTTP), c.c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: discovery: %s", ErrUnavailable, clip(err.Error()))
	}
	c.mu.Lock()
	if c.provider == nil {
		c.provider = p
	}
	p = c.provider
	c.mu.Unlock()
	return p, nil
}

func (c *Client) oauth2Config(p *oidc.Provider, redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.c.ClientID,
		ClientSecret: c.c.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  redirectURI,
		Scopes:       c.c.Scopes,
	}
}

// Start discovers (lazily, cached on success) and returns the IdP authorize
// URL with response_type=code, client_id, redirect_uri, scope, state, nonce,
// code_challenge, code_challenge_method=S256. State, nonce and verifier are
// fresh 256-bit randoms.
func (c *Client) Start(ctx context.Context, redirectURI string) (authURL string, p Pending, err error) {
	prov, err := c.discover()
	if err != nil {
		return "", Pending{}, err
	}
	p = Pending{State: random(), Nonce: random(), Verifier: random()}
	authURL = c.oauth2Config(prov, redirectURI).AuthCodeURL(p.State,
		oidc.Nonce(p.Nonce), oauth2.S256ChallengeOption(p.Verifier))
	return authURL, p, nil
}

// Finish exchanges code with p.Verifier, verifies the ID token and p.Nonce,
// and returns the user value (lowercased when UserClaim is "email").
// The caller must already have checked the callback's state against p.State.
func (c *Client) Finish(ctx context.Context, p Pending, code, redirectURI string) (value string, err error) {
	prov, err := c.discover()
	if err != nil {
		return "", err
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.c.HTTP)
	tok, err := c.oauth2Config(prov, redirectURI).Exchange(ctx, code, oauth2.VerifierOption(p.Verifier))
	if err != nil {
		return "", exchangeError(err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return "", fmt.Errorf("%w: no id_token in token response", ErrInvalidToken)
	}
	idt, err := prov.Verifier(&oidc.Config{ClientID: c.c.ClientID, Now: c.c.Now}).Verify(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidToken, clip(err.Error()))
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return "", fmt.Errorf("%w: claims don't decode", ErrInvalidToken)
	}
	// go-oidc doesn't check azp; OIDC Core 3.1.3.7 says it must be us when present.
	if azp, present := claims["azp"]; present {
		if s, _ := azp.(string); s != c.c.ClientID {
			return "", fmt.Errorf("%w: azp isn't client_id", ErrInvalidToken)
		}
	}
	if p.Nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(p.Nonce)) != 1 {
		return "", fmt.Errorf("%w: nonce mismatch", ErrInvalidToken)
	}
	v, _ := claims[c.c.UserClaim].(string)
	if v == "" {
		return "", fmt.Errorf("%w: %q not a non-empty string", ErrClaim, c.c.UserClaim)
	}
	if c.c.UserClaim == "email" {
		if verified, _ := claims["email_verified"].(bool); !verified {
			return "", fmt.Errorf("%w: email_verified isn't true", ErrClaim)
		}
		v = strings.ToLower(v)
	}
	return v, nil
}

// exchangeError maps a code exchange failure to ErrUnavailable without the
// token endpoint's response body.
func exchangeError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		return fmt.Errorf("%w: token endpoint status %d", ErrUnavailable, status)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%w: token request: %v", ErrUnavailable, ue.Err)
	}
	return fmt.Errorf("%w: token request failed", ErrUnavailable)
}

// random returns 256 random bits, base64url without padding (43 chars,
// valid as a PKCE verifier).
func random() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// clip bounds library error text, which can quote a discovery response, so
// a misbehaving IdP can't flood the logs.
func clip(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
