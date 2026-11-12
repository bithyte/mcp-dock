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
	"github.com/bithyte/mcp-dock/internal/identity"
	"github.com/bithyte/mcp-dock/internal/oplogin"
	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

const (
	testGitHubClientID     = "gh-cid"
	testGitHubClientSecret = "gh-secret"
)

// newTestServer starts an httptest server serving the oauth routes, with
// public_url set to its own URL and allowlist ["cozmin"]. Logs are discarded.
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
		AllowedUsers: config.AllowedUsers{GitHub: []string{"cozmin"}},
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

// testGatewayWallet is the gateway's own wallet address in Open Payments tests.
const testGatewayWallet = "https://gateway.example/mcp"

// newTestServerOP starts an oauth server with Open Payments login against a
// fake wallet and auth server, and GitHub login too when withGitHub is set
// (the returned FakeGitHub is nil otherwise). allowWallets are wallet names
// on the fake ("cozmin" allows fop.Wallet("cozmin")). Logs are discarded.
func newTestServerOP(t *testing.T, allowGitHub, allowWallets []string, withGitHub bool) (*Server, *httptest.Server, *testutil.FakeGitHub, *testutil.FakeOpenPayments) {
	t.Helper()
	srv, ts, gh, fop, _ := newTestServerOPLogs(t, allowGitHub, allowWallets, withGitHub)
	return srv, ts, gh, fop
}

// newTestServerOPLogs is newTestServerOP with JSON logs captured.
func newTestServerOPLogs(t *testing.T, allowGitHub, allowWallets []string, withGitHub bool) (*Server, *httptest.Server, *testutil.FakeGitHub, *testutil.FakeOpenPayments, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	pemText, pub := testutil.Ed25519PEM(t)
	fop := testutil.NewFakeOpenPayments(t, testGatewayWallet, pub)
	opc, err := oplogin.New(oplogin.Config{
		WalletAddress: testGatewayWallet,
		KeyID:         "key-1",
		PrivateKey:    pemText,
		HTTP:          fop.Client(),
		Logger:        log,
	})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "mcp-dock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	wallets := make([]string, 0, len(allowWallets))
	for _, name := range allowWallets {
		n, err := identity.NormalizeWalletAddress(fop.Wallet(name))
		if err != nil {
			t.Fatal(err)
		}
		wallets = append(wallets, n)
	}
	cfg := &config.Config{
		PublicURL: ts.URL,
		DataDir:   t.TempDir(),
		Providers: config.ProvidersConfig{OpenPayments: &config.OpenPaymentsConfig{
			WalletAddress: testGatewayWallet, KeyID: "key-1", PrivateKey: pemText,
		}},
		AllowedUsers: config.AllowedUsers{GitHub: allowGitHub, OpenPayments: wallets},
	}
	opts := Options{Config: cfg, Store: st, OpenPayments: opc, Logger: log}
	var gh *testutil.FakeGitHub
	if withGitHub {
		gh = testutil.NewFakeGitHub(t, testGitHubClientID, testGitHubClientSecret)
		cfg.Providers.GitHub = &config.GitHubConfig{ClientID: testGitHubClientID, ClientSecret: testGitHubClientSecret}
		opts.GitHub = &github.Client{
			ClientID:     testGitHubClientID,
			ClientSecret: testGitHubClientSecret,
			WebBaseURL:   gh.URL,
			APIBaseURL:   gh.URL,
		}
	}
	srv := New(opts)
	srv.Routes(mux)
	return srv, ts, gh, fop, logs
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
