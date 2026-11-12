package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/identity"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gw.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func TestClientRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	c := Client{ID: "c1", Name: "n", RedirectURIs: []string{"http://localhost:1/cb", "https://claude.ai/api/mcp/auth_callback"}, AuthMethod: "none", CreatedAt: time.Unix(1700000000, 0).UTC()}
	if err := s.CreateClient(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetClient(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RedirectURIs, c.RedirectURIs) || got.Name != "n" || got.AuthMethod != "none" || !got.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("mismatch: %+v", got)
	}
	if _, err := s.GetClient(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestAuthRequestFlow(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	r := AuthRequest{ID: "a1", ClientID: "c", RedirectURI: "r", ExpiresAt: now.Add(time.Minute)}
	if err := s.SaveAuthRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAuthRequestUser(ctx, "a1", identity.GitHubUser("Cozmin"), 7); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAuthRequest(ctx, "a1")
	if err != nil || got.Login != "github:cozmin" || got.GitHubID != 7 {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.DeleteAuthRequest(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAuthRequest(ctx, "a1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	s.SaveAuthRequest(ctx, AuthRequest{ID: "a2", ExpiresAt: now.Add(-time.Minute)})
	if _, err := s.GetAuthRequest(ctx, "a2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
}

func TestConsumeAuthRequestOnce(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	r := AuthRequest{ID: "a1", ClientID: "c", RedirectURI: "r", ClientState: "st", CodeChallenge: "ch",
		Resource: "res", Scope: "sc", ExpiresAt: now.Add(time.Minute)}
	if err := s.SaveAuthRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAuthRequestUser(ctx, "a1", identity.GitHubUser("Cozmin"), 7); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeAuthRequest(ctx, "a1", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a1" || got.ClientID != "c" || got.RedirectURI != "r" || got.ClientState != "st" ||
		got.CodeChallenge != "ch" || got.Resource != "res" || got.Scope != "sc" ||
		got.Login != "github:cozmin" || got.GitHubID != 7 {
		t.Fatalf("consumed %+v", got)
	}
	if _, err := s.ConsumeAuthRequest(ctx, "a1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume: %v, want ErrNotFound", err)
	}
	if _, err := s.ConsumeAuthRequest(ctx, "missing", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v, want ErrNotFound", err)
	}
	s.SaveAuthRequest(ctx, AuthRequest{ID: "a2", ExpiresAt: now.Add(-time.Minute)})
	if _, err := s.ConsumeAuthRequest(ctx, "a2", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v, want ErrNotFound", err)
	}
}

func TestCodeSingleUse(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SaveCode(ctx, Code{Hash: "h", ClientID: "c", Login: identity.GitHubUser("Bob"), ExpiresAt: now.Add(time.Minute)})
	c, err := s.ConsumeCode(ctx, "h", now)
	if err != nil || c.Login != "github:bob" || c.ClientID != "c" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := s.ConsumeCode(ctx, "h", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCodeExpired(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SaveCode(ctx, Code{Hash: "h", ExpiresAt: now.Add(-time.Second)})
	if _, err := s.ConsumeCode(ctx, "h", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRefreshSingleUse(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SaveToken(ctx, Token{Hash: "r", Kind: "refresh", Login: identity.GitHubUser("bob"), ExpiresAt: now.Add(time.Hour)})
	s.SaveToken(ctx, Token{Hash: "a", Kind: "access", Login: identity.GitHubUser("bob"), ExpiresAt: now.Add(time.Hour)})
	if _, err := s.ConsumeRefreshToken(ctx, "a", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access must not be consumable as refresh: %v", err)
	}
	if _, err := s.ConsumeRefreshToken(ctx, "r", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeRefreshToken(ctx, "r", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGetTokenExpired(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SaveToken(ctx, Token{Hash: "a", Kind: "access", ExpiresAt: now.Add(-time.Second)})
	if _, err := s.GetToken(ctx, "a", now); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestConcurrentConsumeCode(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SaveCode(ctx, Code{Hash: "h", ExpiresAt: now.Add(time.Minute)})
	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ConsumeCode(ctx, "h", now); err == nil {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("want 1 success, got %d", ok)
	}
}

func TestLoginCaseFolding(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.SetGitHubID(ctx, identity.GitHubUser("Cozmin"), 1); err != nil {
		t.Fatal(err)
	}
	id, ok, err := s.GitHubID(ctx, identity.GitHubUser("cozmin"))
	if err != nil || !ok || id != 1 {
		t.Fatalf("%d %v %v", id, ok, err)
	}
	if _, ok, _ := s.GitHubID(ctx, identity.GitHubUser("other")); ok {
		t.Fatal("unexpected ok")
	}
}

func TestResetUser(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SetGitHubID(ctx, identity.GitHubUser("Bob"), 5)
	s.SaveConsent(ctx, identity.GitHubUser("bob"), "c", "https://a.example/cb")
	s.SaveConsent(ctx, identity.GitHubUser("bob"), "c", "https://b.example/cb")
	s.SaveToken(ctx, Token{Hash: "t", Kind: "access", Login: identity.GitHubUser("BOB"), ExpiresAt: now.Add(time.Hour)})
	if has, _ := s.HasConsent(ctx, identity.GitHubUser("BOB"), "c", "https://a.example/cb"); !has {
		t.Fatal("consent missing")
	}
	if err := s.ResetUser(ctx, identity.GitHubUser("Bob")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GitHubID(ctx, identity.GitHubUser("bob")); ok {
		t.Fatal("id remains")
	}
	for _, u := range []string{"https://a.example/cb", "https://b.example/cb"} {
		if has, _ := s.HasConsent(ctx, identity.GitHubUser("bob"), "c", u); has {
			t.Fatalf("consent for %s remains", u)
		}
	}
	if _, err := s.GetToken(ctx, "t", now); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestConsentPerRedirectURI(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const a, b = "https://claude.ai/api/mcp/auth_callback", "https://evil.example/cb"
	if err := s.SaveConsent(ctx, identity.GitHubUser("Bob"), "c", a); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConsent(ctx, identity.GitHubUser("bob"), "c", a); err != nil { // idempotent
		t.Fatal(err)
	}
	for _, tc := range []struct {
		login, client, uri string
		want               bool
	}{
		{"bob", "c", a, true},
		{"BOB", "c", a, true},
		{"bob", "c", b, false},
		{"bob", "other", a, false},
		{"alice", "c", a, false},
		{"bob", "c", "", false},
	} {
		if has, err := s.HasConsent(ctx, identity.GitHubUser(tc.login), tc.client, tc.uri); err != nil || has != tc.want {
			t.Errorf("HasConsent(%q,%q,%q) = %v, %v; want %v", tc.login, tc.client, tc.uri, has, err, tc.want)
		}
	}
	if err := s.SaveConsent(ctx, identity.GitHubUser("bob"), "c", b); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.HasConsent(ctx, identity.GitHubUser("bob"), "c", b); !has {
		t.Fatal("second URI consent missing")
	}
}

func TestDeleteExpired(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SaveCode(ctx, Code{Hash: "old", ExpiresAt: now.Add(-time.Hour)})
	s.SaveToken(ctx, Token{Hash: "old", Kind: "access", ExpiresAt: now.Add(-time.Hour)})
	s.SaveToken(ctx, Token{Hash: "new", Kind: "access", ExpiresAt: now.Add(time.Hour)})
	if err := s.DeleteExpired(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetToken(ctx, "new", now); err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	s, p := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SaveToken(ctx, Token{Hash: "t", Kind: "access", Login: identity.GitHubUser("bob"), ExpiresAt: now.Add(time.Hour)})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.GetToken(ctx, "t", now); err != nil {
		t.Fatal(err)
	}
}

func TestStoreKeepsWalletCase(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const a = "https://claude.ai/api/mcp/auth_callback"
	if err := s.SaveConsent(ctx, "open_payments:https://w.example/Al", "c", a); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.HasConsent(ctx, "open_payments:https://w.example/Al", "c", a); !has {
		t.Error("exact wallet id has no consent")
	}
	if has, _ := s.HasConsent(ctx, "open_payments:https://w.example/al", "c", a); has {
		t.Error("wallet ids must be case sensitive")
	}
}

func TestMigrateBareLogins(t *testing.T) {
	s, p := openTest(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (login, github_id) VALUES ('Cozmin', 1), ('open_payments:https://w.example/Al', 2)`)
	exec(`INSERT INTO consents (login, client_id, redirect_uri) VALUES ('Cozmin', 'c', 'r')`)
	exec(`INSERT INTO tokens (hash, kind, client_id, login, resource, scope, expires_at) VALUES ('t1','access','c','cozmin','','',1), ('t2','access','c','Cozmin','','',1)`)
	exec(`INSERT INTO codes (hash, client_id, redirect_uri, code_challenge, resource, scope, login, expires_at) VALUES ('c1','c','r','','','','Cozmin',1), ('c2','c','r','','','','cozmin',1)`)
	exec(`INSERT INTO auth_requests (id, client_id, redirect_uri, client_state, code_challenge, resource, scope, login, github_id, expires_at) VALUES ('a1','c','r','','','','','',0,1)`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	check := func() {
		t.Helper()
		s2, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer s2.Close()
		for table, want := range map[string][]string{
			"users":         {"github:cozmin", "open_payments:https://w.example/Al"},
			"consents":      {"github:cozmin"},
			"tokens":        {"github:cozmin", "github:cozmin"},
			"codes":         {"github:cozmin", "github:cozmin"},
			"auth_requests": {""},
		} {
			rows, err := s2.db.QueryContext(ctx, "SELECT login FROM "+table+" ORDER BY rowid")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var l string
				if err := rows.Scan(&l); err != nil {
					t.Fatal(err)
				}
				got = append(got, l)
			}
			rows.Close()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s logins = %q, want %q", table, got, want)
			}
		}
	}
	check()
	check() // idempotent
}

func saveReq(t *testing.T, s *Store, id string, exp time.Time) {
	t.Helper()
	if err := s.SaveAuthRequest(context.Background(), AuthRequest{ID: id, ClientID: "c", RedirectURI: "r", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingClaimOnce(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	saveReq(t, s, "a", time.Now().Add(time.Hour))
	if err := s.SetAuthRequestPending(ctx, "a", `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimAuthRequestPending(ctx, "a", time.Now())
	if err != nil || got != `{"x":1}` {
		t.Fatalf("claim = %q, %v", got, err)
	}
	if _, err := s.ClaimAuthRequestPending(ctx, "a", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim err = %v, want ErrNotFound", err)
	}
}

func TestPendingClaimConcurrent(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	saveReq(t, s, "a", time.Now().Add(time.Hour))
	if err := s.SetAuthRequestPending(ctx, "a", "blob"); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := s.ClaimAuthRequestPending(ctx, "a", time.Now()); err == nil && v == "blob" {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d claims won, want 1", wins.Load())
	}
}

func TestPendingOverwrite(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	saveReq(t, s, "a", time.Now().Add(time.Hour))
	for _, v := range []string{"a", "b"} {
		if err := s.SetAuthRequestPending(ctx, "a", v); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.ClaimAuthRequestPending(ctx, "a", time.Now()); err != nil || got != "b" {
		t.Fatalf("claim = %q, %v", got, err)
	}
}

func TestPendingRejectedAfterLogin(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	saveReq(t, s, "a", time.Now().Add(time.Hour))
	if err := s.SetAuthRequestPending(ctx, "a", "p"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAuthRequestUser(ctx, "a", "github:x", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAuthRequestPending(ctx, "a", "q"); !errors.Is(err, ErrNotFound) {
		t.Errorf("set after login err = %v, want ErrNotFound", err)
	}
	if _, err := s.ClaimAuthRequestPending(ctx, "a", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("claim after login err = %v, want ErrNotFound", err)
	}
}

func TestPendingExpired(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	saveReq(t, s, "a", exp)
	if err := s.SetAuthRequestPending(ctx, "a", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAuthRequestPending(ctx, "a", exp.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Errorf("claim after expiry err = %v, want ErrNotFound", err)
	}
	saveReq(t, s, "b", time.Now().Add(-time.Second))
	if err := s.SetAuthRequestPending(ctx, "b", "p"); !errors.Is(err, ErrNotFound) {
		t.Errorf("set on expired err = %v, want ErrNotFound", err)
	}
	if err := s.SetAuthRequestPending(ctx, "missing", "p"); !errors.Is(err, ErrNotFound) {
		t.Errorf("set on missing err = %v, want ErrNotFound", err)
	}
}

func TestPendingColumnMigration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE auth_requests (
		id TEXT PRIMARY KEY, client_id TEXT NOT NULL, redirect_uri TEXT NOT NULL, client_state TEXT NOT NULL,
		code_challenge TEXT NOT NULL, resource TEXT NOT NULL, scope TEXT NOT NULL, login TEXT NOT NULL,
		github_id INTEGER NOT NULL, expires_at INTEGER NOT NULL);
		INSERT INTO auth_requests VALUES ('old','c','r','','','','','',0,` + strconv.FormatInt(time.Now().Add(time.Hour).UnixNano(), 10) + `)`)
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()
	for i := 0; i < 2; i++ { // second Open proves idempotence
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := s.SetAuthRequestPending(ctx, "old", "p"); err != nil {
			t.Fatal(err)
		}
		if got, err := s.ClaimAuthRequestPending(ctx, "old", time.Now()); err != nil || got != "p" {
			t.Fatalf("claim = %q, %v", got, err)
		}
		s.Close()
	}
}
