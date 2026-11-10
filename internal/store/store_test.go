package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	if err := s.SetAuthRequestUser(ctx, "a1", "Cozmin", 7); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAuthRequest(ctx, "a1")
	if err != nil || got.Login != "cozmin" || got.GitHubID != 7 {
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
	if err := s.SetAuthRequestUser(ctx, "a1", "Cozmin", 7); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeAuthRequest(ctx, "a1", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a1" || got.ClientID != "c" || got.RedirectURI != "r" || got.ClientState != "st" ||
		got.CodeChallenge != "ch" || got.Resource != "res" || got.Scope != "sc" ||
		got.Login != "cozmin" || got.GitHubID != 7 {
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
	s.SaveCode(ctx, Code{Hash: "h", ClientID: "c", Login: "Bob", ExpiresAt: now.Add(time.Minute)})
	c, err := s.ConsumeCode(ctx, "h", now)
	if err != nil || c.Login != "bob" || c.ClientID != "c" {
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
	s.SaveToken(ctx, Token{Hash: "r", Kind: "refresh", Login: "bob", ExpiresAt: now.Add(time.Hour)})
	s.SaveToken(ctx, Token{Hash: "a", Kind: "access", Login: "bob", ExpiresAt: now.Add(time.Hour)})
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
	if err := s.SetGitHubID(ctx, "Cozmin", 1); err != nil {
		t.Fatal(err)
	}
	id, ok, err := s.GitHubID(ctx, "cozmin")
	if err != nil || !ok || id != 1 {
		t.Fatalf("%d %v %v", id, ok, err)
	}
	if _, ok, _ := s.GitHubID(ctx, "other"); ok {
		t.Fatal("unexpected ok")
	}
}

func TestResetUser(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.SetGitHubID(ctx, "Bob", 5)
	s.SaveConsent(ctx, "bob", "c", "https://a.example/cb")
	s.SaveConsent(ctx, "bob", "c", "https://b.example/cb")
	s.SaveToken(ctx, Token{Hash: "t", Kind: "access", Login: "BOB", ExpiresAt: now.Add(time.Hour)})
	if has, _ := s.HasConsent(ctx, "BOB", "c", "https://a.example/cb"); !has {
		t.Fatal("consent missing")
	}
	if err := s.ResetUser(ctx, "Bob"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GitHubID(ctx, "bob"); ok {
		t.Fatal("id remains")
	}
	for _, u := range []string{"https://a.example/cb", "https://b.example/cb"} {
		if has, _ := s.HasConsent(ctx, "bob", "c", u); has {
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
	if err := s.SaveConsent(ctx, "Bob", "c", a); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConsent(ctx, "bob", "c", a); err != nil { // idempotent
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
		if has, err := s.HasConsent(ctx, tc.login, tc.client, tc.uri); err != nil || has != tc.want {
			t.Errorf("HasConsent(%q,%q,%q) = %v, %v; want %v", tc.login, tc.client, tc.uri, has, err, tc.want)
		}
	}
	if err := s.SaveConsent(ctx, "bob", "c", b); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.HasConsent(ctx, "bob", "c", b); !has {
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
	s.SaveToken(ctx, Token{Hash: "t", Kind: "access", Login: "bob", ExpiresAt: now.Add(time.Hour)})
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
