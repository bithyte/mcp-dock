// Package store persists OAuth state in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS clients (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	redirect_uris TEXT NOT NULL,
	auth_method TEXT NOT NULL,
	secret_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS auth_requests (
	id TEXT PRIMARY KEY,
	client_id TEXT NOT NULL,
	redirect_uri TEXT NOT NULL,
	client_state TEXT NOT NULL,
	code_challenge TEXT NOT NULL,
	resource TEXT NOT NULL,
	scope TEXT NOT NULL,
	login TEXT NOT NULL,
	github_id INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS codes (
	hash TEXT PRIMARY KEY,
	client_id TEXT NOT NULL,
	redirect_uri TEXT NOT NULL,
	code_challenge TEXT NOT NULL,
	resource TEXT NOT NULL,
	scope TEXT NOT NULL,
	login TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	hash TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	client_id TEXT NOT NULL,
	login TEXT NOT NULL,
	resource TEXT NOT NULL,
	scope TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tokens_login ON tokens(login);
CREATE TABLE IF NOT EXISTS consents (
	login TEXT NOT NULL,
	client_id TEXT NOT NULL,
	PRIMARY KEY (login, client_id)
);
CREATE TABLE IF NOT EXISTS users (
	login TEXT PRIMARY KEY,
	github_id INTEGER NOT NULL
);
`

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path, in WAL mode with a busy timeout.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DeleteExpired removes expired auth requests, codes and tokens.
func (s *Store) DeleteExpired(ctx context.Context, now time.Time) error {
	for _, t := range []string{"auth_requests", "codes", "tokens"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+t+" WHERE expires_at <= ?", ts(now)); err != nil {
			return fmt.Errorf("delete expired %s: %w", t, err)
		}
	}
	return nil
}

func ts(t time.Time) int64 { return t.UnixNano() }

func fromTS(n int64) time.Time { return time.Unix(0, n).UTC() }

func norm(login string) string { return strings.ToLower(login) }

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
