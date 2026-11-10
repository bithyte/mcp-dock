package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

const (
	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
)

// tokenError is an OAuth error response from /token (RFC 6749 5.2).
type tokenError struct {
	status         int
	code, desc     string
	basicChallenge bool // add WWW-Authenticate: Basic (client tried Basic auth)
}

func (e *tokenError) write(w http.ResponseWriter) {
	if e.basicChallenge {
		w.Header().Set("WWW-Authenticate", `Basic realm="mcp-dock"`)
	}
	writeError(w, e.status, e.code, e.desc)
}

func invalidRequest(desc string) *tokenError {
	return &tokenError{status: http.StatusBadRequest, code: "invalid_request", desc: desc}
}

func invalidGrant() *tokenError {
	return &tokenError{status: http.StatusBadRequest, code: "invalid_grant"}
}

func invalidTarget() *tokenError {
	return &tokenError{status: http.StatusBadRequest, code: "invalid_target"}
}

var errServer = &tokenError{status: http.StatusInternalServerError, code: "server_error"}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`
}

// handleToken is the RFC 6749 token endpoint for the authorization_code
// (PKCE S256 required) and refresh_token grants. Codes and refresh tokens
// are consumed atomically before any further check, so a failed attempt
// burns them.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	form, terr := parseTokenForm(r)
	if terr != nil {
		terr.write(w)
		return
	}
	grant := form.Get("grant_type")
	switch grant {
	case "":
		invalidRequest("grant_type is required").write(w)
		return
	case "authorization_code", "refresh_token":
	default:
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}
	client, terr := s.authenticateClient(r, form)
	if terr != nil {
		terr.write(w)
		return
	}
	var resp *tokenResponse
	if grant == "authorization_code" {
		resp, terr = s.grantAuthorizationCode(r.Context(), client, form)
	} else {
		resp, terr = s.grantRefreshToken(r.Context(), client, form)
	}
	if terr != nil {
		if terr.code != "invalid_request" {
			s.log.Info("oauth token request rejected", "grant_type", grant, "client_id", client.ID, "error", terr.code)
		}
		terr.write(w)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseTokenForm reads the form-encoded body. Query parameters are
// ignored, and no parameter may appear twice (RFC 6749 3.2).
func parseTokenForm(r *http.Request) (url.Values, *tokenError) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, invalidRequest("content type must be application/x-www-form-urlencoded")
	}
	if err := r.ParseForm(); err != nil {
		if isTooLarge(err) {
			return nil, &tokenError{status: http.StatusRequestEntityTooLarge, code: "invalid_request", desc: "request body too large"}
		}
		return nil, invalidRequest("malformed request body")
	}
	for k, v := range r.PostForm {
		if len(v) > 1 {
			return nil, invalidRequest("duplicate parameter " + k)
		}
	}
	return r.PostForm, nil
}

// authenticateClient identifies and authenticates the client with its
// registered method. Using any other method fails. A public ("none") client
// is identified by client_id alone; a client_secret it sends is ignored.
func (s *Server) authenticateClient(r *http.Request, form url.Values) (*store.Client, *tokenError) {
	unauthorized := &tokenError{status: http.StatusUnauthorized, code: "invalid_client"}
	basicFail := &tokenError{status: http.StatusUnauthorized, code: "invalid_client", basicChallenge: true}

	_, hasAuthz := r.Header["Authorization"]
	var basicID, basicSecret string
	if hasAuthz {
		u, p, ok := r.BasicAuth()
		if !ok {
			return nil, basicFail
		}
		// RFC 6749 2.3.1: both parts are form-urlencoded before Basic encoding.
		var err1, err2 error
		basicID, err1 = url.QueryUnescape(u)
		basicSecret, err2 = url.QueryUnescape(p)
		if err1 != nil || err2 != nil || basicID == "" {
			return nil, basicFail
		}
		if fid, ok := form["client_id"]; ok && fid[0] != basicID {
			return nil, basicFail
		}
	}

	id := basicID
	if id == "" {
		id = form.Get("client_id")
	}
	if id == "" {
		return nil, unauthorized
	}
	fail := unauthorized
	if hasAuthz {
		fail = basicFail
	}
	client, err := s.store.GetClient(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fail
	}
	if err != nil {
		s.log.Error("oauth token: client lookup failed", "err", err)
		return nil, errServer
	}

	_, formSecret := form["client_secret"]
	switch client.AuthMethod {
	case "none":
		if hasAuthz {
			return nil, fail
		}
		return client, nil
	case "client_secret_post":
		if hasAuthz || !formSecret || !secretMatches(form.Get("client_secret"), client.SecretHash) {
			return nil, fail
		}
		return client, nil
	case "client_secret_basic":
		if !hasAuthz || formSecret || !secretMatches(basicSecret, client.SecretHash) {
			return nil, fail
		}
		return client, nil
	}
	return nil, fail
}

// secretMatches compares secret's hash to the stored hash in constant time.
func secretMatches(secret, storedHash string) bool {
	if secret == "" || storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(storedHash)) == 1
}

// checkResource validates an optional RFC 8707 resource parameter: if
// sent, it must be our /mcp endpoint (a trailing slash is tolerated).
func (s *Server) checkResource(form url.Values) *tokenError {
	if rs, ok := form["resource"]; ok && config.NormalizeURL(rs[0]) != s.cfg.ResourceURL() {
		return invalidTarget()
	}
	return nil
}

func (s *Server) grantAuthorizationCode(ctx context.Context, client *store.Client, form url.Values) (*tokenResponse, *tokenError) {
	code, verifier := form.Get("code"), form.Get("code_verifier")
	if code == "" {
		return nil, invalidRequest("code is required")
	}
	if _, ok := form["redirect_uri"]; !ok {
		// Every authorization request carries redirect_uri, so it's
		// required here (RFC 6749 4.1.3).
		return nil, invalidRequest("redirect_uri is required")
	}
	if !validVerifier(verifier) {
		return nil, invalidRequest("code_verifier must be 43-128 unreserved characters")
	}
	if terr := s.checkResource(form); terr != nil {
		return nil, terr
	}

	c, err := s.store.ConsumeCode(ctx, hashSecret(code), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, invalidGrant()
	}
	if err != nil {
		s.log.Error("oauth token: code lookup failed", "err", err)
		return nil, errServer
	}
	// The code is gone now; every failure below leaves it consumed.
	if c.ClientID != client.ID || c.RedirectURI != form.Get("redirect_uri") || !pkceMatches(verifier, c.CodeChallenge) {
		return nil, invalidGrant()
	}
	// A code with some other resource predates a public_url change.
	if c.Resource != "" && c.Resource != s.cfg.ResourceURL() {
		return nil, invalidGrant()
	}
	if !s.cfg.IsAllowed(c.Login) {
		return nil, invalidGrant()
	}
	return s.issueTokens(ctx, client.ID, c.Login, c.Scope)
}

func (s *Server) grantRefreshToken(ctx context.Context, client *store.Client, form url.Values) (*tokenResponse, *tokenError) {
	refresh := form.Get("refresh_token")
	if refresh == "" {
		return nil, invalidRequest("refresh_token is required")
	}
	if terr := s.checkResource(form); terr != nil {
		return nil, terr
	}

	t, err := s.store.ConsumeRefreshToken(ctx, hashSecret(refresh), s.now())
	if errors.Is(err, store.ErrNotFound) {
		// Unknown, expired, or already rotated.
		return nil, invalidGrant()
	}
	if err != nil {
		s.log.Error("oauth token: refresh lookup failed", "err", err)
		return nil, errServer
	}
	// Consumed; every failure below leaves the client to log in again.
	if t.ClientID != client.ID || t.Resource != s.cfg.ResourceURL() || !s.cfg.IsAllowed(t.Login) {
		return nil, invalidGrant()
	}
	scope := t.Scope
	if req, ok := form["scope"]; ok {
		if !scopeSubset(req[0], t.Scope) {
			return nil, &tokenError{status: http.StatusBadRequest, code: "invalid_scope"}
		}
		scope = strings.Join(strings.Fields(req[0]), " ")
	}
	return s.issueTokens(ctx, client.ID, t.Login, scope)
}

// issueTokens stores a new access/refresh pair (hashes only), both bound
// to the canonical resource.
func (s *Server) issueTokens(ctx context.Context, clientID, login, scope string) (*tokenResponse, *tokenError) {
	now := s.now()
	access, refresh := newSecret(), newSecret()
	for _, t := range []store.Token{
		{Hash: hashSecret(access), Kind: "access", ExpiresAt: now.Add(accessTokenTTL)},
		{Hash: hashSecret(refresh), Kind: "refresh", ExpiresAt: now.Add(refreshTokenTTL)},
	} {
		t.ClientID, t.Login, t.Resource, t.Scope = clientID, login, s.cfg.ResourceURL(), scope
		if err := s.store.SaveToken(ctx, t); err != nil {
			s.log.Error("oauth token: save failed", "err", err)
			return nil, errServer
		}
	}
	s.log.Info("oauth tokens issued", "client_id", clientID, "login", login)
	return &tokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int(accessTokenTTL / time.Second),
		RefreshToken: refresh,
		Scope:        scope,
	}, nil
}

// validVerifier checks RFC 7636 4.1: 43-128 unreserved characters. Same
// rule as a challenge.
func validVerifier(v string) bool { return validChallenge(v) }

// pkceMatches checks S256: base64url(sha256(verifier)) == challenge.
func pkceMatches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// scopeSubset reports whether every space-separated scope in req is in granted.
func scopeSubset(req, granted string) bool {
	have := map[string]bool{}
	for _, g := range strings.Fields(granted) {
		have[g] = true
	}
	for _, q := range strings.Fields(req) {
		if !have[q] {
			return false
		}
	}
	return true
}

// Verify is the auth.TokenVerifier for /mcp. It accepts only unexpired
// access tokens issued for our resource to a login that is still on the
// allowlist (read live, so config changes apply to existing tokens). Every
// failure is auth.ErrInvalidToken with no detail.
func (s *Server) Verify(ctx context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
	if token == "" {
		return nil, auth.ErrInvalidToken
	}
	now := s.now()
	t, err := s.store.GetToken(ctx, hashSecret(token), now)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("oauth verify: token lookup failed", "err", err)
		}
		return nil, auth.ErrInvalidToken
	}
	if t.Kind != "access" || !now.Before(t.ExpiresAt) || t.Resource != s.cfg.ResourceURL() || !s.cfg.IsAllowed(t.Login) {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: t.Login, Expiration: t.ExpiresAt}, nil
}
