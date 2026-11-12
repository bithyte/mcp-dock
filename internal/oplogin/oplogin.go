// Package oplogin proves that a user controls an Open Payments wallet
// address, using a subject-only GNAP grant with redirect interaction. It never
// requests an access token and never moves money.
package oplogin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	openpayments "github.com/interledger/open-payments-go"
	as "github.com/interledger/open-payments-go/generated/authserver"
)

var (
	// ErrBadHash means the interaction finish hash did not verify.
	ErrBadHash = errors.New("open payments: interaction hash mismatch")
	// ErrSubjectMismatch means the finalized grant is not for the wallet the
	// login started with.
	ErrSubjectMismatch = errors.New("open payments: grant subject does not match wallet")
	// ErrNotPending means the auth server did not answer the grant request
	// with a pending interactive grant.
	ErrNotPending = errors.New("open payments: grant is not pending interaction")
)

// Config configures a Client. WalletAddress, KeyID and PrivateKey come from
// config.OpenPaymentsConfig.
type Config struct {
	WalletAddress, KeyID, PrivateKey string
	// HTTP is the base client. nil means a default with a 15s timeout.
	// Redirects are disabled and bodies capped either way.
	HTTP   *http.Client
	Logger *slog.Logger
}

// Wallet holds the raw id and authServer from a wallet address document.
type Wallet struct{ ID, AuthServer string }

// Pending is the state of a started login that Finish needs. It holds
// secrets (nonces, continue token) and must stay server side.
type Pending struct {
	WalletID, AuthServer, ClientNonce, FinishNonce, ContinueURI, ContinueToken, RedirectURL string
}

// Client runs the wallet login flow.
type Client struct {
	op  *openpayments.AuthenticatedClient
	log *slog.Logger
}

// New builds a Client that identifies itself as cfg.WalletAddress and signs
// requests with cfg.PrivateKey.
func New(cfg Config) (*Client, error) {
	if cfg.WalletAddress == "" || cfg.KeyID == "" || cfg.PrivateKey == "" {
		return nil, errors.New("open payments: wallet address, key id and private key are required")
	}
	op, err := openpayments.NewAuthenticatedClient(cfg.WalletAddress, cfg.PrivateKey, cfg.KeyID,
		openpayments.WithHTTPClientAuthed(hardened(cfg.HTTP)))
	if err != nil {
		// The SDK error may describe the key input; keep it out.
		return nil, errors.New("open payments: invalid client wallet address or private key")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{op: op, log: log}, nil
}

// Wallet fetches the wallet address document at normalizedURL. Both id and
// an https authServer are required.
func (c *Client) Wallet(ctx context.Context, normalizedURL string) (Wallet, error) {
	if !isHTTPS(normalizedURL) {
		return Wallet{}, errors.New("open payments: wallet address must be https")
	}
	doc, err := c.op.WalletAddress.Get(ctx, openpayments.WalletAddressGetParams{URL: normalizedURL})
	if err != nil {
		return Wallet{}, sanitize(err)
	}
	if doc.Id == nil || *doc.Id == "" {
		return Wallet{}, errors.New("open payments: wallet document has no id")
	}
	if doc.AuthServer == nil || !isHTTPS(*doc.AuthServer) {
		return Wallet{}, errors.New("open payments: wallet document has no https authServer")
	}
	return Wallet{ID: *doc.Id, AuthServer: *doc.AuthServer}, nil
}

// Start requests a subject-only grant for w with redirect interaction that
// finishes at finishURI.
func (c *Client) Start(ctx context.Context, w Wallet, finishURI string) (Pending, error) {
	nonce, err := newNonce()
	if err != nil {
		return Pending{}, err
	}
	body := as.GrantRequestWithSubject{
		Subject: as.Subject{SubIds: []struct {
			Format as.SubjectSubIdsFormat `json:"format"`
			Id     string                 `json:"id"`
		}{{Format: as.Uri, Id: w.ID}}},
		Interact: as.InteractRequest{
			Start: []as.InteractRequestStart{as.InteractRequestStartRedirect},
			Finish: &struct {
				Method as.InteractRequestFinishMethod `json:"method"`
				Nonce  string                         `json:"nonce"`
				Uri    string                         `json:"uri"`
			}{Method: as.InteractRequestFinishMethodRedirect, Nonce: nonce, Uri: finishURI},
		},
	}
	var req as.GrantRequest
	if err := req.FromGrantRequestWithSubject(body); err != nil {
		return Pending{}, errors.New("open payments: building grant request failed")
	}
	g, err := c.op.Grant.Request(ctx, openpayments.GrantRequestParams{URL: w.AuthServer, RequestBody: req})
	if err != nil {
		return Pending{}, sanitize(err)
	}
	if g.Interact == nil || g.Interact.Finish == "" || !isHTTPS(g.Interact.Redirect) ||
		!isHTTPS(g.Continue.Uri) || g.Continue.AccessToken.Value == "" {
		return Pending{}, ErrNotPending
	}
	return Pending{
		WalletID:      w.ID,
		AuthServer:    w.AuthServer,
		ClientNonce:   nonce,
		FinishNonce:   g.Interact.Finish,
		ContinueURI:   g.Continue.Uri,
		ContinueToken: g.Continue.AccessToken.Value,
		RedirectURL:   g.Interact.Redirect,
	}, nil
}

// Finish verifies the interaction hash, continues the grant, checks that its
// subject is p.WalletID, and then cancels the grant. A failed cancel is
// logged and otherwise ignored.
func (c *Client) Finish(ctx context.Context, p Pending, interactRef, hash string) error {
	if !VerifyHash(p.ClientNonce, p.FinishNonce, interactRef, p.AuthServer, hash) {
		return ErrBadHash
	}
	g, err := c.op.Grant.Continue(ctx, openpayments.GrantContinueParams{
		URL: p.ContinueURI, AccessToken: p.ContinueToken, InteractRef: interactRef,
	})
	if err != nil {
		return sanitize(err)
	}
	match := false
	if g.Subject != nil {
		for _, s := range g.Subject.SubIds {
			if s.Format == as.Uri && s.Id == p.WalletID {
				match = true
				break
			}
		}
	}
	// Nothing is needed from the grant after this, so release it either way.
	if err := c.op.Grant.Cancel(ctx, openpayments.GrantCancelParams{
		URL: g.Continue.Uri, AccessToken: g.Continue.AccessToken.Value,
	}); err != nil {
		c.log.Warn("open payments grant cancel failed", "err", sanitize(err))
	}
	if !match {
		return ErrSubjectMismatch
	}
	return nil
}

func newNonce() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("open payments: nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
