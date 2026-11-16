package oauth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bithyte/mcp-dock/internal/identity"
	"github.com/bithyte/mcp-dock/internal/oidclogin"
	"github.com/bithyte/mcp-dock/internal/store"
)

// oidcStartPath is the sign-in button route for OIDC provider name.
func oidcStartPath(name string) string { return "/oauth/oidc/" + name + "/start" }

// oidcCallbackPath is the redirect URI registered at OIDC provider name.
func oidcCallbackPath(name string) string { return "/oauth/oidc/" + name + "/callback" }

func (s *Server) oidcLabel(c *oidclogin.Client) string {
	return s.cfg.ProviderLabel(identity.OIDCProvider(c.Name()))
}

// oidcReason is what gets logged for an oidclogin error: its full text.
// oidclogin builds that text so it's safe to log: library output is clipped,
// and token endpoint response bodies are never included. It's what tells an
// operator about an issuer mismatch, a wrong client_id or a bad secret. It
// never goes on a page.
func oidcReason(err error) string { return err.Error() }

// handleOIDCStart is the sign-in page's button for one OIDC provider.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request, c *oidclogin.Client) {
	ar, ok := s.startForm(w, r)
	if !ok {
		return
	}
	s.oidcBegin(w, r, ar, c)
}

// oidcBegin starts a login at c for ar: it stores fresh state, nonce and
// PKCE verifier on the auth request and redirects to the IdP. An IdP that
// can't be reached leaves the auth request alone so the user can retry.
func (s *Server) oidcBegin(w http.ResponseWriter, r *http.Request, ar *store.AuthRequest, c *oidclogin.Client) {
	name := c.Name()
	authURL, p, err := c.Start(r.Context(), s.cfg.PublicURL+oidcCallbackPath(name))
	if err != nil {
		s.log.Warn("oidc login start failed", "provider", name, "reason", oidcReason(err))
		label := s.oidcLabel(c)
		s.errorPage(w, http.StatusBadGateway, label+" unavailable", "Couldn't reach "+label+". Try again.")
		return
	}
	blob, err := json.Marshal(pendingLogin{Provider: identity.OIDCProvider(name), OIDC: &p})
	if err != nil {
		s.serverError(w, "pending state encode failed", errors.New("json marshal failed"))
		return
	}
	// A resubmitted button replaces the earlier attempt's state, so only
	// the newest attempt can finish.
	err = s.store.SetAuthRequestPending(r.Context(), ar.ID, string(blob), s.now())
	if errors.Is(err, store.ErrNotFound) {
		s.badLink(w)
		return
	}
	if err != nil {
		s.serverError(w, "pending state save failed", err)
		return
	}
	s.log.Info("oidc login started", "provider", name)
	s.redirect(w, authURL)
}

// handleOIDCCallback is where the IdP sends the browser back. The auth
// request comes from the cookie alone; the IdP state is a separate secret
// in the pending blob. A missing or unknown cookie touches nothing, so a
// link opened in another browser doesn't spoil the real attempt. Once the
// pending state is claimed every failure ends the request: it's single use.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request, c *oidclogin.Client) {
	name := c.Name()
	ck, err := r.Cookie(authCookie)
	if err != nil {
		s.badLink(w)
		return
	}
	ar, ok := s.boundRequest(w, r, ck.Value)
	if !ok {
		return
	}
	raw, err := s.store.ClaimAuthRequestPending(r.Context(), ar.ID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		// Logged in already (a replay), or no login was started.
		s.badLink(w)
		return
	}
	if err != nil {
		s.serverError(w, "pending state claim failed", err)
		return
	}
	var pl pendingLogin
	if json.Unmarshal([]byte(raw), &pl) != nil || pl.Provider != identity.OIDCProvider(name) || pl.OIDC == nil {
		// Started at another provider (or not an OIDC login at all). Never
		// finish it here. Don't log the blob or the decode error; both may
		// quote secrets.
		s.log.Warn("oidc callback doesn't match the started login", "provider", name)
		s.abandon(w, r, ar)
		s.serverError(w, "pending state unreadable", errors.New("not a login at this oidc provider"))
		return
	}
	p := *pl.OIDC

	q := r.URL.Query()
	if p.State == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(p.State)) != 1 {
		s.log.Warn("oidc callback state mismatch", "provider", name)
		s.abandon(w, r, ar)
		s.badLink(w)
		return
	}
	if e := q.Get("error"); e != "" {
		code := "server_error"
		if e == "access_denied" {
			code = "access_denied"
		}
		s.log.Info("oidc login not completed", "provider", name, "error", code)
		s.abandon(w, r, ar)
		s.redirectError(w, ar.RedirectURI, ar.ClientState, code, "")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.log.Warn("oidc callback without a code", "provider", name)
		s.abandon(w, r, ar)
		s.badLink(w)
		return
	}

	value, err := c.Finish(r.Context(), p, code, s.cfg.PublicURL+oidcCallbackPath(name))
	if err != nil {
		s.log.Warn("oidc login failed", "provider", name, "reason", oidcReason(err))
		s.abandon(w, r, ar)
		label := s.oidcLabel(c)
		if errors.Is(err, oidclogin.ErrClaim) {
			s.errorPage(w, http.StatusForbidden, "Access denied", label+" didn't provide a verified account id.")
			return
		}
		s.errorPage(w, http.StatusBadGateway, label+" login failed", "Couldn't complete the "+label+" login. Try again.")
		return
	}

	uid := identity.OIDCUser(name, value)
	if !s.cfg.IsAllowed(uid) {
		s.log.Warn("oidc login not on allowlist", "provider", name, "user", uid)
		s.abandon(w, r, ar)
		s.notAllowed(w, uid)
		return
	}
	s.log.Info("oidc login verified", "provider", name, "user", uid)
	s.afterLogin(w, r, ar, uid, value, 0)
}
