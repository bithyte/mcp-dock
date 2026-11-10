// Package github is a minimal GitHub OAuth web-flow client.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxBody        = 1 << 20
	defaultTimeout = 15 * time.Second
)

// User is the GitHub account identity.
type User struct {
	Login string
	ID    int64
}

// Client talks to GitHub's OAuth and REST endpoints.
type Client struct {
	ClientID, ClientSecret string
	WebBaseURL             string // default "https://github.com"
	APIBaseURL             string // default "https://api.github.com"
	HTTP                   *http.Client
}

var defaultHTTP = &http.Client{Timeout: defaultTimeout}

func (c *Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

func (c *Client) web() string {
	if c.WebBaseURL != "" {
		return strings.TrimRight(c.WebBaseURL, "/")
	}
	return "https://github.com"
}

func (c *Client) api() string {
	if c.APIBaseURL != "" {
		return strings.TrimRight(c.APIBaseURL, "/")
	}
	return "https://api.github.com"
}

// AuthorizeURL returns the GitHub authorize URL. No scope is requested.
func (c *Client) AuthorizeURL(state, redirectURI string) string {
	q := url.Values{}
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	return c.web() + "/login/oauth/authorize?" + q.Encode()
}

// do sends req and decodes a size-limited JSON body into out. Errors never
// include request headers, form values, or the response body.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.httpc().Do(req)
	if err != nil {
		// url.Error may embed the URL; keep only the underlying cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("github request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("github read body: %w", err)
	}
	if len(body) > maxBody {
		return errors.New("github response too large")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github returned status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("github returned invalid JSON")
	}
	return nil
}

// Exchange trades an authorization code for an access token.
func (c *Client) Exchange(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.web()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("github: bad request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("github oauth error: %s", sanitize(out.Error))
	}
	if out.AccessToken == "" {
		return "", errors.New("github returned no access token")
	}
	return out.AccessToken, nil
}

// User fetches the authenticated user's identity.
func (c *Client) User(ctx context.Context, accessToken string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api()+"/user", nil)
	if err != nil {
		return User{}, errors.New("github: bad request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	var out struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Error string `json:"error"`
	}
	if err := c.do(req, &out); err != nil {
		return User{}, err
	}
	if out.Error != "" {
		return User{}, fmt.Errorf("github error: %s", sanitize(out.Error))
	}
	if out.Login == "" || out.ID == 0 {
		return User{}, errors.New("github returned an invalid user")
	}
	return User{Login: out.Login, ID: out.ID}, nil
}

// sanitize bounds server-provided error codes before they enter messages.
func sanitize(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}
