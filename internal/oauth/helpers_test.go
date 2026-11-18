package oauth

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/github"
	"github.com/bithyte/mcp-dock/internal/oidclogin"
	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

const (
	testGitHubClientID     = "gh-cid"
	testGitHubClientSecret = "gh-secret"
)

// newTestServer starts an httptest server serving the oauth routes, with
// public_url set to its own URL and allowlist ["alice"]. Logs are discarded.
func newTestServer(t *testing.T) (*Server, *httptest.Server, *testutil.FakeGitHub) {
	t.Helper()
	return newTestServerLog(t, io.Discard)
}

// logBuffer is a goroutine-safe log sink; handlers write from server
// goroutines while the test reads.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestServerLogs is newTestServer with JSON logs captured.
func newTestServerLogs(t *testing.T) (*Server, *httptest.Server, *testutil.FakeGitHub, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	srv, ts, fake := newTestServerLog(t, logs)
	return srv, ts, fake, logs
}

func newTestServerLog(t *testing.T, logOut io.Writer) (*Server, *httptest.Server, *testutil.FakeGitHub) {
	t.Helper()
	fake := testutil.NewFakeGitHub(t, testGitHubClientID, testGitHubClientSecret)
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "mcp-dock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		PublicURL:    ts.URL,
		DataDir:      t.TempDir(),
		Providers:    config.ProvidersConfig{GitHub: &config.GitHubConfig{ClientID: testGitHubClientID, ClientSecret: testGitHubClientSecret}},
		AllowedUsers: config.AllowedUsers{GitHub: []string{"alice"}},
	}
	srv := New(Options{
		Config: cfg,
		Store:  st,
		GitHub: &github.Client{
			ClientID:     testGitHubClientID,
			ClientSecret: testGitHubClientSecret,
			WebBaseURL:   fake.URL,
			APIBaseURL:   fake.URL,
		},
		Logger: slog.New(slog.NewJSONHandler(logOut, nil)),
	})
	srv.Routes(mux)
	return srv, ts, fake
}

// postJSON POSTs body (marshalled unless already []byte) and decodes the JSON response.
func postJSON(t *testing.T, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, ok := body.([]byte)
	if !ok {
		var err error
		if b, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("response not JSON (status %d): %q", resp.StatusCode, data)
		}
	}
	return resp, out
}

func getJSON(t *testing.T, url string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp, out
}

func strs(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// oidcTestOpts configures newTestServerOIDC.
type oidcTestOpts struct {
	GitHub     bool                 // also configure GitHub login (allowlist ["alice"])
	GitHubFake *testutil.FakeGitHub // the fake GitHub to use when GitHub is set; a fresh one if nil
	Providers  []config.OIDCConfig  // Issuer is replaced; empty fields get config's defaults
	Allowed    map[string][]string  // allowed_users.oidc
}

// newTestServerOIDC starts an oauth server with OIDC login, one fresh
// FakeOIDC per provider (keyed by name). Missing client credentials default
// to "<name>-cid" and "<name>-secret". JSON logs are captured.
func newTestServerOIDC(t *testing.T, opts oidcTestOpts) (*Server, *httptest.Server, map[string]*testutil.FakeOIDC, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "mcp-dock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	fakes := map[string]*testutil.FakeOIDC{}
	providers := make([]config.OIDCConfig, len(opts.Providers))
	var clients []*oidclogin.Client
	for i, p := range opts.Providers {
		if p.ClientID == "" {
			p.ClientID = p.Name + "-cid"
		}
		if p.ClientSecret == "" {
			p.ClientSecret = p.Name + "-secret"
		}
		if p.Label == "" {
			p.Label = p.Name
		}
		if p.UserClaim == "" {
			p.UserClaim = "sub"
		}
		if len(p.Scopes) == 0 {
			p.Scopes = []string{"openid"}
		}
		fake := testutil.NewFakeOIDC(t, p.ClientID, p.ClientSecret, testutil.FakeOIDCOptions{})
		p.Issuer = fake.Issuer()
		fakes[p.Name] = fake
		providers[i] = p
		clients = append(clients, oidclogin.New(oidclogin.Config{
			Name: p.Name, Issuer: p.Issuer, ClientID: p.ClientID, ClientSecret: p.ClientSecret,
			Scopes: p.Scopes, UserClaim: p.UserClaim,
		}))
	}
	cfg := &config.Config{
		PublicURL:    ts.URL,
		DataDir:      t.TempDir(),
		Providers:    config.ProvidersConfig{OIDC: providers},
		AllowedUsers: config.AllowedUsers{OIDC: opts.Allowed},
	}
	o := Options{Config: cfg, Store: st, OIDC: clients, Logger: log}
	if opts.GitHub {
		gh := opts.GitHubFake
		if gh == nil {
			gh = testutil.NewFakeGitHub(t, testGitHubClientID, testGitHubClientSecret)
		}
		cfg.Providers.GitHub = &config.GitHubConfig{ClientID: testGitHubClientID, ClientSecret: testGitHubClientSecret}
		cfg.AllowedUsers.GitHub = []string{"alice"}
		o.GitHub = &github.Client{
			ClientID:     testGitHubClientID,
			ClientSecret: testGitHubClientSecret,
			WebBaseURL:   gh.URL,
			APIBaseURL:   gh.URL,
		}
	}
	srv := New(o)
	srv.Routes(mux)
	return srv, ts, fakes, logs
}
