package oauth

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const (
	maxClientNameRunes = 100
	maxRedirectURIs    = 10
	maxRedirectURILen  = 2000
)

var (
	authMethods = []string{"none", "client_secret_post", "client_secret_basic"}
	grantTypes  = []string{"authorization_code", "refresh_token"}

	// loopbackHost matches the parsed URL.Host of an http loopback redirect:
	// localhost, 127.0.0.1 or [::1], with an optional port.
	loopbackHost = regexp.MustCompile(`^(localhost|127\.0\.0\.1|\[::1\])(:[0-9]{1,5})?$`)

	// deniedSchemes are never accepted as custom app schemes: they run code
	// in the browser, read local or browser-internal content, send the code
	// to a remote network host or person (mail, SMS, phone), launch
	// arbitrary Android apps (intent), or hand it to a browser extension.
	deniedSchemes = map[string]bool{
		"javascript": true, "data": true, "file": true, "vbscript": true,
		"about": true, "blob": true, "filesystem": true, "view-source": true,
		"ftp": true, "ws": true, "wss": true,
		"mailto": true, "sms": true, "tel": true, "intent": true,
		"chrome-extension": true, "moz-extension": true,
	}
)

var errBadRedirect = errors.New("invalid redirect_uri")

// validateRedirectURI decides whether a client may register raw. Rules:
//
//   - Only printable ASCII, no backslash, no fragment ('#' anywhere),
//     at most maxRedirectURILen bytes, absolute (non-empty scheme), no
//     userinfo. Checks run on url.Parse fields, not string prefixes.
//   - https: any URI with a host (non-empty Hostname, not opaque), so
//     Claude's https://claude.ai/api/mcp/auth_callback and other web clients
//     work. Redirects aren't otherwise restricted; the consent page (client
//     name and redirect host) is the mitigation.
//   - http: loopback only. URL.Host must be exactly localhost, 127.0.0.1 or
//     [::1], any port 1-65535 or none; any path and query. So
//     localhost.evil.com, 127.0.0.1.evil.com and localhost@evil.com fail.
//   - Any other scheme is treated as a native app's custom scheme (cursor://,
//     com.example.app:/cb). It's accepted unless it's in deniedSchemes, and
//     it needs a non-empty scheme-specific part (opaque data, a host, or a
//     path other than "/"). The consent page is what protects users from a
//     malicious app claiming a scheme, as it does for https redirects.
func validateRedirectURI(raw string) error {
	if raw == "" || len(raw) > maxRedirectURILen {
		return errBadRedirect
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= 0x20 || c >= 0x7f || c == '\\' || c == '#' {
			return errBadRedirect
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.User != nil || u.Fragment != "" {
		return errBadRedirect
	}
	switch u.Scheme {
	case "https":
		if u.Opaque != "" || u.Hostname() == "" {
			return errBadRedirect
		}
		return nil
	case "http":
		if u.Opaque != "" || !loopbackHost.MatchString(u.Host) {
			return errBadRedirect
		}
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
		default:
			return errBadRedirect
		}
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
				return errBadRedirect
			}
		}
		return nil
	default:
		if deniedSchemes[u.Scheme] {
			return errBadRedirect
		}
		if u.Opaque == "" && u.Host == "" && (u.Path == "" || u.Path == "/") {
			return errBadRedirect
		}
		return nil
	}
}

// sanitizeClientName drops non-printable runes (controls, bidi overrides,
// zero-width chars), trims, and caps the name at maxClientNameRunes. The
// name is attacker-controlled and shown on the consent page.
func sanitizeClientName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxClientNameRunes {
		s = strings.TrimSpace(string([]rune(s)[:maxClientNameRunes]))
	}
	return s
}

// handleRegister implements RFC 7591 dynamic client registration.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, "invalid_client_metadata", "content type must be application/json")
			return
		}
	}
	var meta oauthex.ClientRegistrationMetadata
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&meta)
	if err == nil {
		// Exactly one JSON object; this also surfaces an oversize body.
		if _, err2 := dec.Token(); err2 != io.EOF {
			err = errors.Join(errors.New("trailing data"), err2)
		}
	}
	if err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be a JSON object")
		return
	}

	if len(meta.RedirectURIs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	if len(meta.RedirectURIs) > maxRedirectURIs {
		writeError(w, http.StatusBadRequest, "invalid_redirect_uri", "too many redirect_uris")
		return
	}
	for _, u := range meta.RedirectURIs {
		if err := validateRedirectURI(u); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect_uris must be https URLs, http loopback URLs, or native app schemes, without a fragment or userinfo")
			return
		}
	}

	method := meta.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_basic" // RFC 7591 section 2 default
	}
	if !slices.Contains(authMethods, method) {
		writeError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	grants := meta.GrantTypes
	if len(grants) == 0 {
		grants = grantTypes
	}
	for _, g := range grants {
		if !slices.Contains(grantTypes, g) {
			writeError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_types")
			return
		}
	}
	if !slices.Contains(grants, "authorization_code") {
		writeError(w, http.StatusBadRequest, "invalid_client_metadata", "grant_types must include authorization_code")
		return
	}
	for _, rt := range meta.ResponseTypes {
		if rt != "code" {
			writeError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported response_types")
			return
		}
	}

	now := s.now()
	c := store.Client{
		ID:           newSecret(),
		Name:         sanitizeClientName(meta.ClientName),
		RedirectURIs: meta.RedirectURIs,
		AuthMethod:   method,
		CreatedAt:    now,
	}
	var secret string
	if method != "none" {
		secret = newSecret()
		c.SecretHash = hashSecret(secret)
	}
	if err := s.store.CreateClient(r.Context(), c); err != nil {
		s.log.Error("oauth client registration failed", "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	s.log.Info("oauth client registered", "client_id", c.ID, "client_name", c.Name, "auth_method", method)

	resp := &oauthex.ClientRegistrationResponse{
		ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{
			RedirectURIs:            c.RedirectURIs,
			TokenEndpointAuthMethod: method,
			GrantTypes:              grants,
			ResponseTypes:           []string{"code"},
			ClientName:              c.Name,
		},
		ClientID:         c.ID,
		ClientSecret:     secret,
		ClientIDIssuedAt: now,
	}
	b, err := marshalRegistration(resp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	writeRawJSON(w, http.StatusCreated, b)
}

// marshalRegistration adds client_secret_expires_at: 0 when a secret is
// issued. RFC 7591 3.2.1 requires it then, but the SDK's MarshalJSON omits
// a zero value, and 0 ("never expires") is what we mean.
func marshalRegistration(resp *oauthex.ClientRegistrationResponse) ([]byte, error) {
	b, err := json.Marshal(resp)
	if err != nil || resp.ClientSecret == "" {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["client_secret_expires_at"] = json.RawMessage("0")
	return json.Marshal(m)
}
