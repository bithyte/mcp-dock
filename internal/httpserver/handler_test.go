package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/gateway"
	"github.com/bithyte/mcp-dock/internal/github"
	"github.com/bithyte/mcp-dock/internal/oauth"
	"github.com/bithyte/mcp-dock/internal/store"
)

const testPublicURL = "https://mcp.example.com"

type fixture struct {
	cfg   *config.Config
	store *store.Store
	h     http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		PublicURL:    testPublicURL,
		DataDir:      t.TempDir(),
		Providers:    config.ProvidersConfig{GitHub: &config.GitHubConfig{ClientID: "cid", ClientSecret: "secret"}},
		AllowedUsers: config.AllowedUsers{GitHub: []string{"cozmin"}},
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	osrv := oauth.New(oauth.Options{
		Config: cfg,
		Store:  st,
		GitHub: &github.Client{ClientID: "cid", ClientSecret: "secret"},
		Logger: log,
	})
	gw := gateway.New(gateway.Options{Logger: log, Version: "test"})
	return &fixture{cfg: cfg, store: st, h: NewHandler(osrv, gw)}
}

// token stores a valid access token for login and returns it.
func (f *fixture) token(t *testing.T, login string) string {
	t.Helper()
	tok := "test-access-token-" + login
	sum := sha256.Sum256([]byte(tok))
	err := f.store.SaveToken(context.Background(), store.Token{
		Hash:      hex.EncodeToString(sum[:]),
		Kind:      "access",
		ClientID:  "client",
		Login:     login,
		Resource:  f.cfg.ResourceURL(),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func mcpRequest(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q, want 200 \"ok\"", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz: got %d, want 405", rec.Code)
	}
}

func TestMCPRequiresAuth(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, mcpRequest(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	want := `resource_metadata="` + testPublicURL + `/.well-known/oauth-protected-resource/mcp"`
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, want) {
		t.Fatalf("WWW-Authenticate %q, want it to contain %s", got, want)
	}
}

func TestMCPInvalidToken(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, mcpRequest("garbage"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "resource_metadata=") {
		t.Fatalf("WWW-Authenticate %q", got)
	}
}

func TestMCPValidToken(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, mcpRequest(f.token(t, "github:cozmin")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Mcp-Session-Id") == "" {
		t.Fatal("no Mcp-Session-Id: want a stateful session")
	}
}

// The gateway sits behind a reverse proxy that may connect over loopback
// and keep the public Host. That must work, while DNS rebinding (loopback
// connection, foreign Host) stays blocked.
func TestMCPHostCheck(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	tok := f.token(t, "github:cozmin")

	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"mcp.example.com", "", http.StatusOK},
		{"MCP.example.com", "", http.StatusOK},
		{"mcp.example.com:443", "", http.StatusOK},
		{"", "", http.StatusOK}, // loopback Host from the test server URL
		{"localhost:8080", "", http.StatusOK},
		{"evil.example", "", http.StatusForbidden},
		{"mcp.example.com:8443", "", http.StatusForbidden},
		// Bearer auth, not cookies: cross-origin browser clients are fine.
		{"mcp.example.com", "http://localhost:6274", http.StatusOK},
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(initBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+tok)
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("Host %q Origin %q: status %d, want %d", tc.host, tc.origin, resp.StatusCode, tc.want)
		}
	}
}

func TestOAuthRoutesMounted(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testPublicURL+"/mcp") {
		t.Fatalf("metadata: %d %s", rec.Code, rec.Body.String())
	}
}
