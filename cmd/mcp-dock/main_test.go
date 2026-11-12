package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testutil.Cleanup()
	os.Exit(code)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testGetenv(k string) string {
	switch k {
	case "GH_SECRET":
		return "s3cret-value"
	case "PATH":
		return os.Getenv("PATH")
	}
	return ""
}

// writeConfig writes a config with the given listen address and extra YAML
// and returns its path and data dir.
func writeConfig(t *testing.T, listen, extra string) (path, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	cfg := fmt.Sprintf(`public_url: http://%s
listen: %q
data_dir: %s
github:
  client_id: cid
  client_secret_env: GH_SECRET
allowed_users: [cozmin]
%s`, host, listen, dataDir, extra)
	path = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dataDir
}

func TestUsersReset(t *testing.T) {
	p, dataDir := writeConfig(t, ":8080", "")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "gateway.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetGitHubID(context.Background(), "github:cozmin", 42); err != nil {
		t.Fatal(err)
	}
	const wallet = "open_payments:https://w.example/Al"
	if err := st.SaveConsent(context.Background(), wallet, "c", "https://a.example/cb"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var out bytes.Buffer
	if err := run(context.Background(), []string{"users", "reset", "-config", p, "Cozmin"}, testGetenv, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "github:cozmin") {
		t.Errorf("stdout %q, want a confirmation naming github:cozmin", out.String())
	}

	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GitHubID(context.Background(), "github:cozmin"); err != nil || ok {
		t.Fatalf("GitHubID after reset: ok=%v err=%v, want cleared", ok, err)
	}
	st.Close()

	out.Reset()
	if err := run(context.Background(), []string{"users", "reset", "-config", p, "$w.example/Al"}, testGetenv, &out); err != nil {
		t.Fatalf("run wallet: %v", err)
	}
	if !strings.Contains(out.String(), wallet) {
		t.Errorf("stdout %q, want a confirmation naming %s", out.String(), wallet)
	}
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if has, _ := st.HasConsent(context.Background(), wallet, "c", "https://a.example/cb"); has {
		t.Error("wallet consent not cleared")
	}

	err = run(context.Background(), []string{"users", "reset", "-config", p, "http://x"}, testGetenv, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("http://x: err %v, want a usage error", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	p, dataDir := writeConfig(t, ":8080", "")
	getenv := func(k string) string {
		if k == "MCP_DOCK_CONFIG" {
			return p
		}
		return testGetenv(k)
	}
	if err := run(context.Background(), []string{"users", "reset", "octo"}, getenv, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fi, err := os.Stat(dataDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir: %v %v, want mode 0700", fi, err)
	}
}

func TestUsage(t *testing.T) {
	p, _ := writeConfig(t, ":8080", "")
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"users"},
		{"users", "bogus"},
		{"users", "reset", "-config", p},
		{"users", "reset", "-config", p, "a", "b"},
		{"serve", "extra"},
		{"serve", "-nope"},
	} {
		err := run(context.Background(), args, testGetenv, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("run(%q) = %v, want a usage error", args, err)
		}
	}
}

func TestBadConfig(t *testing.T) {
	p, _ := writeConfig(t, ":8080", "")
	noSecret := func(string) string { return "" }
	err := run(context.Background(), []string{"serve", "-config", p}, noSecret, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "GH_SECRET") {
		t.Fatalf("run = %v, want config error about GH_SECRET", err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestServe runs the real serve path with one working and one missing
// upstream, checks it serves, and checks it shuts down on ctx cancel.
func TestServe(t *testing.T) {
	bin := testutil.BuildFakeUpstream(t)
	addr := freeAddr(t)
	p, dataDir := writeConfig(t, addr, fmt.Sprintf(`upstreams:
  - name: fake
    command: %s
  - name: broken
    command: /nonexistent
`, bin))

	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve", "-config", p}, testGetenv, out) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(b) != "ok" {
				t.Fatalf("healthz: %d %q", resp.StatusCode, b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v\n%s", err, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for !strings.Contains(out.String(), `"upstream":"broken"`) || !strings.Contains(out.String(), "upstream started") {
		if time.Now().After(deadline) {
			t.Fatalf("want broken upstream failure and fake upstream started in logs:\n%s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp, err := http.Post("http://"+addr+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/mcp without token: %d, want 401", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancel")
	}

	logs := out.String()
	if !strings.Contains(logs, `"msg":"mcp-dock listening"`) {
		t.Errorf("no startup log line:\n%s", logs)
	}
	if strings.Contains(logs, "s3cret-value") {
		t.Error("client secret appears in logs")
	}
	if fi, err := os.Stat(dataDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("data dir: %v %v, want mode 0700", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "gateway.db")); err != nil {
		t.Errorf("gateway.db: %v", err)
	}
}

func TestDataDirTightened(t *testing.T) {
	p, dataDir := writeConfig(t, ":8080", "")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"users", "reset", "-config", p, "octo"}, testGetenv, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fi, err := os.Stat(dataDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir: %v %v, want mode 0700", fi, err)
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if srv.IdleTimeout != 2*time.Minute || srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Fatalf("idle %v, read header %v", srv.IdleTimeout, srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout != 0 || srv.ReadTimeout != 0 {
		t.Fatalf("write %v, read %v: long tool calls need both unset", srv.WriteTimeout, srv.ReadTimeout)
	}
}
