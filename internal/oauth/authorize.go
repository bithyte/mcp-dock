package oauth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/store"
)

const (
	authCookie     = "mcpdock_auth"
	authCookiePath = "/oauth/"
	authRequestTTL = 10 * time.Minute
	codeTTL        = 5 * time.Minute

	// consentPath is under authCookiePath so the browser sends the cookie
	// with the consent POST. Plain /consent wouldn't get it.
	consentPath  = "/oauth/consent"
	callbackPath = "/oauth/github/callback"

	maxStateLen = 2048
	maxScopeLen = 1024
)

// singleParams may appear at most once in an authorization request.
var singleParams = []string{"response_type", "code_challenge", "code_challenge_method", "state", "scope"}

// redirectAllowed reports whether requested matches one of the client's
// registered redirect URIs: an exact string match, or for http loopback
// URIs a match on every parsed field except the port (RFC 8252 7.3).
func redirectAllowed(registered []string, requested string) bool {
	if validateRedirectURI(requested) != nil {
		return false
	}
	ru, err := url.Parse(requested)
	if err != nil {
		return false
	}
	for _, reg := range registered {
		if reg == requested {
			return true
		}
		if ru.Scheme != "http" {
			continue
		}
		gu, err := url.Parse(reg)
		if err != nil || gu.Scheme != "http" || !isLoopback(gu) || !isLoopback(ru) {
			continue
		}
		if gu.Hostname() == ru.Hostname() &&
			gu.User == nil && ru.User == nil &&
			gu.Opaque == "" && ru.Opaque == "" &&
			gu.EscapedPath() == ru.EscapedPath() &&
			gu.RawQuery == ru.RawQuery && gu.ForceQuery == ru.ForceQuery &&
			gu.Fragment == "" && ru.Fragment == "" {
			return true
		}
	}
	return false
}

func isLoopback(u *url.URL) bool {
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// validChallenge checks an S256 code_challenge: 43-128 chars of the RFC 7636
// unreserved set (S256 output is always 43).
func validChallenge(c string) bool {
	if len(c) < 43 || len(c) > 128 {
		return false
	}
	for i := 0; i < len(c); i++ {
		b := c[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '.' || b == '_' || b == '~') {
			return false
		}
	}
	return true
}

// handleAuthorize starts an authorization: it validates the client and
// redirect URI, saves an AuthRequest, binds it to this browser with a
// cookie, and sends the user to GitHub.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Until the client and redirect_uri check out, never redirect: an
	// error page is all an unverified redirect_uri gets (RFC 6749 4.1.2.1).
	if len(q["client_id"]) != 1 || len(q["redirect_uri"]) != 1 {
		s.errorPage(w, http.StatusBadRequest, "Invalid request",
			"The authorization request needs exactly one client_id and one redirect_uri.")
		return
	}
	client, err := s.store.GetClient(r.Context(), q.Get("client_id"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, http.StatusBadRequest, "Unknown client",
			"The app that sent you here isn't registered with this gateway. Try connecting again from the app.")
		return
	}
	if err != nil {
		s.log.Error("oauth authorize: client lookup failed", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Server error", "Something went wrong. Try again.")
		return
	}
	redirect := q.Get("redirect_uri")
	if !redirectAllowed(client.RedirectURIs, redirect) {
		s.errorPage(w, http.StatusBadRequest, "Invalid redirect",
			"The redirect address doesn't match what this app registered.")
		return
	}

	// redirect is trusted from here on; errors go back to the client.
	state := q.Get("state")
	if len(state) > maxStateLen {
		state = "" // don't echo an oversized value
		s.redirectError(w, redirect, state, "invalid_request", "state too long")
		return
	}
	for _, p := range singleParams {
		if len(q[p]) > 1 {
			s.redirectError(w, redirect, state, "invalid_request", "duplicate "+p)
			return
		}
	}
	if q.Get("response_type") != "code" {
		s.redirectError(w, redirect, state, "unsupported_response_type", "")
		return
	}
	if q.Get("code_challenge_method") != "S256" || !validChallenge(q.Get("code_challenge")) {
		s.redirectError(w, redirect, state, "invalid_request", "PKCE with code_challenge_method S256 is required")
		return
	}
	if len(q.Get("scope")) > maxScopeLen {
		s.redirectError(w, redirect, state, "invalid_scope", "")
		return
	}
	var resource string
	if rs, ok := q["resource"]; ok {
		// One resource only, and it must be our /mcp endpoint (RFC 8707).
		// A trailing slash is tolerated.
		if len(rs) != 1 || config.NormalizeURL(rs[0]) != s.cfg.ResourceURL() {
			s.redirectError(w, redirect, state, "invalid_target", "")
			return
		}
		resource = s.cfg.ResourceURL()
	}

	ar := store.AuthRequest{
		ID:            newSecret(),
		ClientID:      client.ID,
		RedirectURI:   redirect,
		ClientState:   state,
		CodeChallenge: q.Get("code_challenge"),
		Resource:      resource,
		Scope:         q.Get("scope"),
		ExpiresAt:     s.now().Add(authRequestTTL),
	}
	if err := s.store.SaveAuthRequest(r.Context(), ar); err != nil {
		s.log.Error("oauth authorize: save request failed", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Server error", "Something went wrong. Try again.")
		return
	}
	s.setAuthCookie(w, ar.ID, int(authRequestTTL/time.Second))
	s.redirect(w, s.gh.AuthorizeURL(ar.ID, s.cfg.PublicURL+callbackPath))
}

// handleGitHubCallback finishes the GitHub login. The state must name an
// unexpired AuthRequest and equal this browser's cookie, so a GitHub
// callback link can't be completed in someone else's browser.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ar, ok := s.boundRequest(w, r, q.Get("state"))
	if !ok {
		return
	}
	if ar.Login != "" {
		// Callback replayed after login; GitHub codes are single-use anyway.
		s.badLink(w)
		return
	}
	if e := q.Get("error"); e != "" {
		code := "server_error"
		if e == "access_denied" {
			code = "access_denied"
		}
		s.abandon(w, r, ar)
		s.redirectError(w, ar.RedirectURI, ar.ClientState, code, "")
		return
	}
	ghCode := q.Get("code")
	if ghCode == "" {
		s.badLink(w)
		return
	}

	tok, err := s.gh.Exchange(r.Context(), ghCode, s.cfg.PublicURL+callbackPath)
	if err != nil {
		s.log.Warn("github code exchange failed", "err", err)
		s.errorPage(w, http.StatusBadGateway, "GitHub login failed", "Couldn't complete the GitHub login. Try again.")
		return
	}
	user, err := s.gh.User(r.Context(), tok)
	if err != nil {
		s.log.Warn("github user lookup failed", "err", err)
		s.errorPage(w, http.StatusBadGateway, "GitHub login failed", "Couldn't complete the GitHub login. Try again.")
		return
	}

	if !s.cfg.IsAllowed(user.Login) {
		s.log.Warn("github login not on allowlist", "login", user.Login)
		s.abandon(w, r, ar)
		s.notAllowed(w, user.Login)
		return
	}
	pinned, known, err := s.store.GitHubID(r.Context(), user.Login)
	if err != nil {
		s.serverError(w, "github id lookup failed", err)
		return
	}
	if known && pinned != user.ID {
		// Same login, different account: GitHub logins can be renamed and
		// re-registered. Deny until an admin runs `mcp-dock users reset`.
		s.log.Warn("github id mismatch for known login", "login", user.Login, "stored_github_id", pinned, "github_id", user.ID)
		s.abandon(w, r, ar)
		s.notAllowed(w, user.Login)
		return
	}
	if !known {
		if err := s.store.SetGitHubID(r.Context(), user.Login, user.ID); err != nil {
			s.serverError(w, "github id save failed", err)
			return
		}
	}
	if err := s.store.SetAuthRequestUser(r.Context(), ar.ID, user.Login, user.ID); err != nil {
		s.serverError(w, "auth request update failed", err)
		return
	}
	ar.Login, ar.GitHubID = strings.ToLower(user.Login), user.ID

	consented, err := s.store.HasConsent(r.Context(), ar.Login, ar.ClientID)
	if err != nil {
		s.serverError(w, "consent lookup failed", err)
		return
	}
	if consented {
		s.issueCode(w, r, ar)
		return
	}
	client, err := s.store.GetClient(r.Context(), ar.ClientID)
	if err != nil {
		s.serverError(w, "client lookup failed", err)
		return
	}
	name := client.Name
	if name == "" {
		name = "Unnamed app"
	}
	s.writeHTML(w, http.StatusOK, consentTmpl, consentData{
		ClientName:   name,
		RedirectHost: redirectDisplay(ar.RedirectURI),
		Login:        user.Login,
		RequestID:    ar.ID,
	})
}

// handleConsent takes the Approve/Deny POST. The AuthRequest must exist,
// be past GitHub login, and equal this browser's cookie; anything else is an
// error page and never a code.
//
// CSRF: the cookie is SameSite=Lax, so browsers don't send it on a
// cross-site POST, and the request ID in the form is a 256-bit secret only
// this browser has seen (it's the cookie value and is only rendered on a
// no-store, unframeable page). A forged POST lacks one or both. A same-site
// attacker could send the cookie but still can't know the ID.
func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		if isTooLarge(err) {
			s.errorPage(w, http.StatusRequestEntityTooLarge, "Invalid request", "The request was too large.")
			return
		}
		s.errorPage(w, http.StatusBadRequest, "Invalid request", "The request couldn't be read.")
		return
	}
	ar, ok := s.boundRequest(w, r, r.PostForm.Get("request"))
	if !ok {
		return
	}
	if ar.Login == "" {
		s.badLink(w)
		return
	}
	switch r.PostForm.Get("action") {
	case "deny":
		s.abandon(w, r, ar)
		s.redirectError(w, ar.RedirectURI, ar.ClientState, "access_denied", "")
	case "approve":
		if !s.cfg.IsAllowed(ar.Login) {
			s.abandon(w, r, ar)
			s.notAllowed(w, ar.Login)
			return
		}
		if err := s.store.SaveConsent(r.Context(), ar.Login, ar.ClientID); err != nil {
			s.serverError(w, "consent save failed", err)
			return
		}
		s.issueCode(w, r, ar)
	default:
		s.errorPage(w, http.StatusBadRequest, "Invalid request", "Choose Approve or Deny.")
	}
}

// issueCode finishes an authorization: it stores the code's hash, drops the
// AuthRequest and cookie, and redirects to the client with code, state and
// iss (RFC 9207).
func (s *Server) issueCode(w http.ResponseWriter, r *http.Request, ar *store.AuthRequest) {
	// Delete first so the AuthRequest can't be reused if saving fails.
	if err := s.store.DeleteAuthRequest(r.Context(), ar.ID); err != nil {
		s.serverError(w, "auth request delete failed", err)
		return
	}
	code := newSecret()
	err := s.store.SaveCode(r.Context(), store.Code{
		Hash:          hashSecret(code),
		ClientID:      ar.ClientID,
		RedirectURI:   ar.RedirectURI,
		CodeChallenge: ar.CodeChallenge,
		Resource:      ar.Resource,
		Scope:         ar.Scope,
		Login:         ar.Login,
		ExpiresAt:     s.now().Add(codeTTL),
	})
	if err != nil {
		s.serverError(w, "code save failed", err)
		return
	}
	s.clearAuthCookie(w)
	s.log.Info("oauth authorization code issued", "client_id", ar.ClientID, "login", ar.Login)
	v := url.Values{}
	v.Set("code", code)
	if ar.ClientState != "" {
		v.Set("state", ar.ClientState)
	}
	v.Set("iss", s.cfg.PublicURL)
	s.redirect(w, appendQuery(ar.RedirectURI, v))
}

// boundRequest loads the AuthRequest id and checks it equals this browser's
// cookie. On failure it has written a 400 page.
func (s *Server) boundRequest(w http.ResponseWriter, r *http.Request, id string) (*store.AuthRequest, bool) {
	ck, err := r.Cookie(authCookie)
	if id == "" || err != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(id)) != 1 {
		s.badLink(w)
		return nil, false
	}
	ar, err := s.store.GetAuthRequest(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.badLink(w)
		return nil, false
	}
	if err != nil {
		s.serverError(w, "auth request lookup failed", err)
		return nil, false
	}
	return ar, true
}

// abandon ends a flow that won't issue a code.
func (s *Server) abandon(w http.ResponseWriter, r *http.Request, ar *store.AuthRequest) {
	if err := s.store.DeleteAuthRequest(r.Context(), ar.ID); err != nil {
		s.log.Error("oauth: auth request delete failed", "err", err)
	}
	s.clearAuthCookie(w)
}

func (s *Server) badLink(w http.ResponseWriter) {
	s.errorPage(w, http.StatusBadRequest, "Login link expired",
		"This sign-in has expired, was already used, or was opened in a different browser. Start again from your app.")
}

// notAllowed is the only 403 page. Allowlist misses and GitHub ID
// mismatches look the same; the reason is in the server log.
func (s *Server) notAllowed(w http.ResponseWriter, login string) {
	s.errorPage(w, http.StatusForbidden, "Access denied", "GitHub account "+login+" isn't allowed on this gateway.")
}

func (s *Server) serverError(w http.ResponseWriter, what string, err error) {
	s.log.Error("oauth: "+what, "err", err)
	s.errorPage(w, http.StatusInternalServerError, "Server error", "Something went wrong. Try again.")
}

// redirectError sends an authorization error to a trusted redirect URI.
func (s *Server) redirectError(w http.ResponseWriter, redirect, state, code, desc string) {
	v := url.Values{}
	v.Set("error", code)
	if desc != "" {
		v.Set("error_description", desc)
	}
	if state != "" {
		v.Set("state", state)
	}
	v.Set("iss", s.cfg.PublicURL)
	s.redirect(w, appendQuery(redirect, v))
}

func (s *Server) redirect(w http.ResponseWriter, loc string) {
	h := w.Header()
	h.Set("Location", loc)
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusFound)
}

// appendQuery adds v to uri, keeping its existing query as is. Registered
// URIs never have a fragment.
func appendQuery(uri string, v url.Values) string {
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
		if strings.HasSuffix(uri, "?") || strings.HasSuffix(uri, "&") {
			sep = ""
		}
	}
	return uri + sep + v.Encode()
}

// redirectDisplay is what the consent page shows as the destination: the
// host for https, scheme and host for others (loopback, app schemes).
func redirectDisplay(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	switch {
	case u.Scheme == "https":
		return u.Host
	case u.Host != "":
		return u.Scheme + "://" + u.Host
	default:
		return u.Scheme + ": (app on this device)"
	}
}

func (s *Server) secureCookies() bool {
	u, err := url.Parse(s.cfg.PublicURL)
	return err == nil && u.Scheme == "https"
}

func (s *Server) setAuthCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookie,
		Value:    value,
		Path:     authCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearAuthCookie(w http.ResponseWriter) {
	s.setAuthCookie(w, "", -1)
}
