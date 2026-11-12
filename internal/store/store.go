// Package store persists OAuth state in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
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
	expires_at INTEGER NOT NULL,
	pending TEXT NOT NULL DEFAULT ''
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
	redirect_uri TEXT NOT NULL,
	PRIMARY KEY (login, client_id, redirect_uri)
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
	if err := migratePendingColumn(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateBareLogins(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migratePendingColumn adds auth_requests.pending to databases created before
// it existed. Fresh databases already have it from the schema.
func migratePendingColumn(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('auth_requests') WHERE name='pending'`).Scan(&n); err != nil {
		return fmt.Errorf("migrate pending column: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE auth_requests ADD COLUMN pending TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrate pending column: %w", err)
	}
	return nil
}

// migrateBareLogins rewrites logins stored before ids were provider
// prefixed ("Cozmin") to "github:cozmin". Rows that already contain a colon
// and the empty login of a not yet signed-in auth request are left alone, so
// it is safe to run on every Open.
func migrateBareLogins(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate logins: %w", err)
	}
	defer tx.Rollback()
	for _, t := range []string{"auth_requests", "codes", "tokens", "consents", "users"} {
		q := "UPDATE " + t + " SET login = 'github:' || lower(login) WHERE login <> '' AND instr(login, ':') = 0"
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrate logins in %s: %w", t, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate logins: %w", err)
	}
	return nil
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

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
