package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type Client struct {
	ID, Name     string
	RedirectURIs []string
	AuthMethod   string // "none" | "client_secret_post" | "client_secret_basic"
	SecretHash   string // empty for "none"
	CreatedAt    time.Time
}

func (s *Store) CreateClient(ctx context.Context, c Client) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO clients (id, name, redirect_uris, auth_method, secret_hash, created_at) VALUES (?,?,?,?,?,?)`,
		c.ID, c.Name, string(uris), c.AuthMethod, c.SecretHash, ts(c.CreatedAt))
	return err
}

func (s *Store) GetClient(ctx context.Context, id string) (*Client, error) {
	var c Client
	var uris string
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, redirect_uris, auth_method, secret_hash, created_at FROM clients WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &uris, &c.AuthMethod, &c.SecretHash, &created)
	if err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, err
	}
	c.CreatedAt = fromTS(created)
	return &c, nil
}

type AuthRequest struct {
	ID, ClientID, RedirectURI, ClientState, CodeChallenge, Resource, Scope string
	Login                                                                  string // set after GitHub callback
	GitHubID                                                               int64
	ExpiresAt                                                              time.Time
}

func (s *Store) SaveAuthRequest(ctx context.Context, r AuthRequest) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO auth_requests (id, client_id, redirect_uri, client_state, code_challenge, resource, scope, login, github_id, expires_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.ClientID, r.RedirectURI, r.ClientState, r.CodeChallenge, r.Resource, r.Scope, r.Login, r.GitHubID, ts(r.ExpiresAt))
	return err
}

// GetAuthRequest returns ErrNotFound if missing or expired.
func (s *Store) GetAuthRequest(ctx context.Context, id string) (*AuthRequest, error) {
	var r AuthRequest
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, client_id, redirect_uri, client_state, code_challenge, resource, scope, login, github_id, expires_at
		 FROM auth_requests WHERE id=? AND expires_at>?`, id, ts(time.Now())).
		Scan(&r.ID, &r.ClientID, &r.RedirectURI, &r.ClientState, &r.CodeChallenge, &r.Resource, &r.Scope, &r.Login, &r.GitHubID, &exp)
	if err != nil {
		return nil, notFound(err)
	}
	r.ExpiresAt = fromTS(exp)
	return &r, nil
}

// SetAuthRequestUser records who logged in. Only the first login on a
// request sticks; later calls, like a missing request, get ErrNotFound.
func (s *Store) SetAuthRequestUser(ctx context.Context, id, userID string, githubID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE auth_requests SET login=?, github_id=? WHERE id=? AND login=''`, userID, githubID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAuthRequestPending stores an opaque provider blob (it may hold a
// continue token, so callers must never log it) on an unexpired auth request
// that has no login yet at now, replacing any earlier blob. ErrNotFound if no row
// matched.
func (s *Store) SetAuthRequestPending(ctx context.Context, id, pending string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE auth_requests SET pending=? WHERE id=? AND login='' AND expires_at>?`,
		pending, id, ts(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimAuthRequestPending atomically clears and returns the pending blob.
// Only one caller can win; the rest get ErrNotFound. RETURNING reports the
// post-update row, so a no-op UPDATE first takes the write lock and reads the
// current blob, then the second UPDATE clears it, all in one transaction.
func (s *Store) ClaimAuthRequestPending(ctx context.Context, id string, now time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var pending string
	err = tx.QueryRowContext(ctx,
		`UPDATE auth_requests SET pending=pending WHERE id=? AND pending<>'' AND login='' AND expires_at>? RETURNING pending`,
		id, ts(now)).Scan(&pending)
	if err != nil {
		return "", notFound(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_requests SET pending='' WHERE id=?`, id); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return pending, nil
}

// ConsumeAuthRequest atomically deletes and returns an unexpired auth
// request. Only one caller can win; the rest get ErrNotFound.
func (s *Store) ConsumeAuthRequest(ctx context.Context, id string, now time.Time) (*AuthRequest, error) {
	var r AuthRequest
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM auth_requests WHERE id=? AND expires_at>?
		 RETURNING id, client_id, redirect_uri, client_state, code_challenge, resource, scope, login, github_id, expires_at`,
		id, ts(now)).
		Scan(&r.ID, &r.ClientID, &r.RedirectURI, &r.ClientState, &r.CodeChallenge, &r.Resource, &r.Scope, &r.Login, &r.GitHubID, &exp)
	if err != nil {
		return nil, notFound(err)
	}
	r.ExpiresAt = fromTS(exp)
	return &r, nil
}

func (s *Store) DeleteAuthRequest(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_requests WHERE id=?`, id)
	return err
}

type Code struct {
	Hash, ClientID, RedirectURI, CodeChallenge, Resource, Scope, Login string
	ExpiresAt                                                          time.Time
}

func (s *Store) SaveCode(ctx context.Context, c Code) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO codes (hash, client_id, redirect_uri, code_challenge, resource, scope, login, expires_at) VALUES (?,?,?,?,?,?,?,?)`,
		c.Hash, c.ClientID, c.RedirectURI, c.CodeChallenge, c.Resource, c.Scope, c.Login, ts(c.ExpiresAt))
	return err
}

// ConsumeCode atomically deletes and returns the code; ErrNotFound if used or expired.
func (s *Store) ConsumeCode(ctx context.Context, hash string, now time.Time) (*Code, error) {
	var c Code
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM codes WHERE hash=? AND expires_at>? RETURNING hash, client_id, redirect_uri, code_challenge, resource, scope, login, expires_at`,
		hash, ts(now)).
		Scan(&c.Hash, &c.ClientID, &c.RedirectURI, &c.CodeChallenge, &c.Resource, &c.Scope, &c.Login, &exp)
	if err != nil {
		return nil, notFound(err)
	}
	c.ExpiresAt = fromTS(exp)
	return &c, nil
}

type Token struct {
	Hash, Kind, ClientID, Login, Resource, Scope string // Kind: "access" | "refresh"
	ExpiresAt                                    time.Time
}

func (s *Store) SaveToken(ctx context.Context, t Token) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (hash, kind, client_id, login, resource, scope, expires_at) VALUES (?,?,?,?,?,?,?)`,
		t.Hash, t.Kind, t.ClientID, t.Login, t.Resource, t.Scope, ts(t.ExpiresAt))
	return err
}

func scanToken(row *sql.Row) (*Token, error) {
	var t Token
	var exp int64
	if err := row.Scan(&t.Hash, &t.Kind, &t.ClientID, &t.Login, &t.Resource, &t.Scope, &exp); err != nil {
		return nil, notFound(err)
	}
	t.ExpiresAt = fromTS(exp)
	return &t, nil
}

func (s *Store) GetToken(ctx context.Context, hash string, now time.Time) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx,
		`SELECT hash, kind, client_id, login, resource, scope, expires_at FROM tokens WHERE hash=? AND expires_at>?`,
		hash, ts(now)))
}

// ConsumeRefreshToken atomically deletes and returns a refresh token.
func (s *Store) ConsumeRefreshToken(ctx context.Context, hash string, now time.Time) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx,
		`DELETE FROM tokens WHERE hash=? AND kind='refresh' AND expires_at>? RETURNING hash, kind, client_id, login, resource, scope, expires_at`,
		hash, ts(now)))
}

// HasConsent reports whether userID approved clientID for redirectURI.
// redirectURI is compared as an exact string; callers pass a canonical key
// (the oauth package drops loopback ports).
func (s *Store) HasConsent(ctx context.Context, userID, clientID, redirectURI string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM consents WHERE login=? AND client_id=? AND redirect_uri=?`,
		userID, clientID, redirectURI).Scan(&n)
	return n > 0, err
}

func (s *Store) SaveConsent(ctx context.Context, userID, clientID, redirectURI string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO consents (login, client_id, redirect_uri) VALUES (?,?,?)`,
		userID, clientID, redirectURI)
	return err
}

func (s *Store) GitHubID(ctx context.Context, userID string) (id int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT github_id FROM users WHERE login=?`, userID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (s *Store) SetGitHubID(ctx context.Context, userID string, id int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (login, github_id) VALUES (?,?) ON CONFLICT(login) DO UPDATE SET github_id=excluded.github_id`, userID, id)
	return err
}

// ResetUser clears the GitHub ID, consents and tokens for userID.
func (s *Store) ResetUser(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM users WHERE login=?`,
		`DELETE FROM consents WHERE login=?`,
		`DELETE FROM tokens WHERE login=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
