package oauth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bithyte/mcp-dock/internal/identity"
	"github.com/bithyte/mcp-dock/internal/oplogin"
	"github.com/bithyte/mcp-dock/internal/store"
)

const (
	badWalletInput    = "Enter a wallet address like $wallet.example/alice."
	walletUnreachable = "Couldn't reach that wallet address. Check it and try again."
)

// pendingLogin is the provider state stored on an auth request between
// starting an upstream login and its callback. It holds secrets (nonces,
// continue token) and is never logged.
type pendingLogin struct {
	Provider     identity.Provider `json:"provider"`
	OpenPayments *oplogin.Pending  `json:"open_payments,omitempty"`
}

// signin renders the sign-in page for the auth request id. wallet is the
// value to put back in the wallet field and errMsg an inline error, both
// empty on first render.
func (s *Server) signin(w http.ResponseWriter, status int, clientName, id, wallet, errMsg string) {
	if clientName == "" {
		clientName = "Unnamed app"
	}
	s.writeHTML(w, status, signinTmpl, signinData{
		ClientName:   clientName,
		RequestID:    id,
		Wallet:       wallet,
		Error:        errMsg,
		GitHub:       s.gh != nil,
		OpenPayments: s.op != nil,
	})
}

// resignin re-renders the sign-in page for ar after a bad wallet attempt.
// The auth request stays alive so the user can retry.
func (s *Server) resignin(w http.ResponseWriter, r *http.Request, ar *store.AuthRequest, status int, wallet, errMsg string) {
	client, err := s.store.GetClient(r.Context(), ar.ClientID)
	if err != nil {
		s.serverError(w, "client lookup failed", err)
		return
	}
	s.signin(w, status, client.Name, ar.ID, wallet, errMsg)
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

// handleOPStart is the sign-in page's wallet form. The typed address must be
// on the allowlist before anything is fetched from it, and the id its wallet
// document reports must be too. Bad input or an unreachable wallet
// re-renders the page; an address that isn't allowed ends the request.
func (s *Server) handleOPStart(w http.ResponseWriter, r *http.Request) {
	ar, ok := s.startForm(w, r)
	if !ok {
		return
	}
	typed := r.PostForm.Get("wallet")
	addr, err := identity.NormalizeWalletAddress(typed)
	if err != nil {
		s.resignin(w, r, ar, http.StatusBadRequest, typed, badWalletInput)
		return
	}
	uid := identity.WalletUser(addr)
	if !s.cfg.IsAllowed(uid) {
		s.log.Warn("open payments wallet not on allowlist", "wallet", addr)
		s.abandon(w, r, ar)
		s.notAllowed(w, uid)
		return
	}

	wallet, err := s.op.Wallet(r.Context(), addr)
	if err != nil {
		s.log.Warn("open payments wallet fetch failed", "wallet", addr, "err", err)
		s.resignin(w, r, ar, http.StatusBadGateway, typed, walletUnreachable)
		return
	}
	idAddr, err := identity.NormalizeWalletAddress(wallet.ID)
	if err != nil || !s.cfg.IsAllowed(identity.WalletUser(idAddr)) {
		s.log.Warn("open payments wallet id not on allowlist", "wallet", addr, "wallet_id", wallet.ID)
		s.abandon(w, r, ar)
		if err != nil {
			s.notAllowed(w, uid)
		} else {
			s.notAllowed(w, identity.WalletUser(idAddr))
		}
		return
	}

	p, err := s.op.Start(r.Context(), wallet, s.cfg.PublicURL+opCallbackPath)
	if err != nil {
		s.log.Warn("open payments grant request failed", "wallet", addr, "err", err)
		s.resignin(w, r, ar, http.StatusBadGateway, typed, walletUnreachable)
		return
	}
	blob, err := json.Marshal(pendingLogin{Provider: identity.OpenPayments, OpenPayments: &p})
	if err != nil {
		s.serverError(w, "pending state encode failed", errors.New("json marshal failed"))
		return
	}
	// A resubmitted form replaces the earlier attempt's state, so only the
	// newest attempt can finish.
	err = s.store.SetAuthRequestPending(r.Context(), ar.ID, string(blob))
	if errors.Is(err, store.ErrNotFound) {
		s.badLink(w)
		return
	}
	if err != nil {
		s.serverError(w, "pending state save failed", err)
		return
	}
	s.log.Info("open payments login started", "wallet", addr)
	s.redirect(w, p.RedirectURL)
}

// handleOPCallback is where the wallet provider sends the browser back. The
// finish URI can't carry our state (it would leak the cookie secret to the
// wallet provider), so the auth request comes from the cookie alone. A
// missing or unknown cookie touches nothing, so a link opened in another
// browser doesn't spoil the real attempt. Once the pending state is claimed
// every failure ends the request: the state is single use.
func (s *Server) handleOPCallback(w http.ResponseWriter, r *http.Request) {
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
		// Logged in already (a replay), or no wallet login was started.
		s.badLink(w)
		return
	}
	if err != nil {
		s.serverError(w, "pending state claim failed", err)
		return
	}
	var pl pendingLogin
	if json.Unmarshal([]byte(raw), &pl) != nil || pl.Provider != identity.OpenPayments || pl.OpenPayments == nil {
		// Don't log the blob or the decode error; both may quote secrets.
		s.abandon(w, r, ar)
		s.serverError(w, "pending state unreadable", errors.New("not an open payments login"))
		return
	}
	p := *pl.OpenPayments
	wallet, err := identity.NormalizeWalletAddress(p.WalletID)
	if err != nil {
		s.abandon(w, r, ar)
		s.serverError(w, "pending wallet id invalid", err)
		return
	}

	q := r.URL.Query()
	if q.Has("result") {
		code := "server_error"
		if q.Get("result") == "grant_rejected" {
			code = "access_denied"
		}
		s.log.Info("open payments login not completed", "wallet", wallet, "error", code)
		s.abandon(w, r, ar)
		s.redirectError(w, ar.RedirectURI, ar.ClientState, code, "")
		return
	}
	ref, hash := q.Get("interact_ref"), q.Get("hash")
	if ref == "" || hash == "" {
		s.abandon(w, r, ar)
		s.badLink(w)
		return
	}

	if err := s.op.Finish(r.Context(), p, ref, hash); err != nil {
		s.log.Warn("open payments login failed", "wallet", wallet, "err", err)
		s.abandon(w, r, ar)
		if errors.Is(err, oplogin.ErrBadHash) || errors.Is(err, oplogin.ErrSubjectMismatch) {
			s.errorPage(w, http.StatusBadRequest, "Wallet login failed",
				"Couldn't confirm you own that wallet address. Start again from your app.")
			return
		}
		s.errorPage(w, http.StatusBadGateway, "Wallet login failed", "Couldn't complete the wallet login. Try again.")
		return
	}

	uid := identity.WalletUser(wallet)
	if !s.cfg.IsAllowed(uid) {
		s.log.Warn("open payments wallet not on allowlist", "wallet", wallet)
		s.abandon(w, r, ar)
		s.notAllowed(w, uid)
		return
	}
	s.log.Info("open payments login verified", "wallet", wallet)
	s.afterLogin(w, r, ar, uid, wallet, 0)
}
