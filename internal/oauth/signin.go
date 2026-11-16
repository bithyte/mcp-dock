package oauth

import (
	"net/http"

	"github.com/bithyte/mcp-dock/internal/identity"
	"github.com/bithyte/mcp-dock/internal/oidclogin"
	"github.com/bithyte/mcp-dock/internal/store"
)

// pendingLogin is the provider state stored on an auth request between
// starting an upstream login and its callback. It holds secrets (state,
// nonce, PKCE verifier) and is never logged.
type pendingLogin struct {
	Provider identity.Provider  `json:"provider"`
	OIDC     *oidclogin.Pending `json:"oidc,omitempty"`
}

// signin renders the sign-in page for the auth request id.
func (s *Server) signin(w http.ResponseWriter, clientName, id string) {
	if clientName == "" {
		clientName = "Unnamed app"
	}
	var buttons []signinButton
	if s.gh != nil {
		buttons = append(buttons, signinButton{Action: githubStartPath, Label: "Continue with GitHub"})
	}
	for _, c := range s.oidc {
		buttons = append(buttons, signinButton{Action: oidcStartPath(c.Name()), Label: "Continue with " + s.oidcLabel(c)})
	}
	s.writeHTML(w, http.StatusOK, signinTmpl, signinData{
		ClientName: clientName,
		RequestID:  id,
		Buttons:    buttons,
	})
}

// startForm parses a sign-in button POST and returns its auth request,
// which must be bound to this browser and not logged in yet. On failure it
// has written an error page.
func (s *Server) startForm(w http.ResponseWriter, r *http.Request) (*store.AuthRequest, bool) {
	if err := r.ParseForm(); err != nil {
		if isTooLarge(err) {
			s.errorPage(w, http.StatusRequestEntityTooLarge, "Invalid request", "The request was too large.")
			return nil, false
		}
		s.errorPage(w, http.StatusBadRequest, "Invalid request", "The request couldn't be read.")
		return nil, false
	}
	ar, ok := s.boundRequest(w, r, r.PostForm.Get("request"))
	if !ok {
		return nil, false
	}
	if ar.Login != "" {
		s.badLink(w)
		return nil, false
	}
	return ar, true
}

// handleGitHubStart is the sign-in page's GitHub button.
func (s *Server) handleGitHubStart(w http.ResponseWriter, r *http.Request) {
	ar, ok := s.startForm(w, r)
	if !ok {
		return
	}
	s.redirect(w, s.gh.AuthorizeURL(ar.ID, s.cfg.PublicURL+callbackPath))
}
