package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseConfig(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		cfg, err := parseConfig([]string{"-listen", ":9100", "--", "cmd"}, envOf(map[string]string{"MCP_RUNNER_LISTEN": ":9000"}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.listen != ":9100" {
			t.Fatalf("listen = %q, want :9100", cfg.listen)
		}
	})
	t.Run("env wins over default", func(t *testing.T) {
		cfg, err := parseConfig([]string{"cmd"}, envOf(map[string]string{
			"MCP_RUNNER_LISTEN":       ":9000",
			"MCP_RUNNER_MAX_SESSIONS": "7",
			"MCP_RUNNER_IDLE_TIMEOUT": "90s",
			"MCP_RUNNER_WORKDIR":      "/tmp/wd",
			"MCP_RUNNER_BEARER_TOKEN": "tok",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.listen != ":9000" || cfg.maxSessions != 7 || cfg.idleTimeout != 90*time.Second ||
			cfg.workDir != "/tmp/wd" || cfg.bearerToken != "tok" {
			t.Fatalf("cfg = %+v", cfg)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		cfg, err := parseConfig([]string{"cmd"}, envOf(nil))
		if err != nil {
			t.Fatal(err)
		}
		wd, _ := os.Getwd()
		if cfg.listen != ":8080" || cfg.maxSessions != 4 || cfg.idleTimeout != 10*time.Minute ||
			cfg.workDir != wd || cfg.bearerToken != "" || cfg.showVersion {
			t.Fatalf("cfg = %+v (wd %q)", cfg, wd)
		}
	})
	t.Run("flags override numeric env", func(t *testing.T) {
		cfg, err := parseConfig([]string{"-max-sessions", "2", "-idle-timeout", "1m", "-workdir", "/x", "cmd"},
			envOf(map[string]string{"MCP_RUNNER_MAX_SESSIONS": "9", "MCP_RUNNER_IDLE_TIMEOUT": "5m", "MCP_RUNNER_WORKDIR": "/y"}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.maxSessions != 2 || cfg.idleTimeout != time.Minute || cfg.workDir != "/x" {
			t.Fatalf("cfg = %+v", cfg)
		}
	})
	t.Run("bad values", func(t *testing.T) {
		for _, args := range [][]string{
			{"-max-sessions", "abc", "cmd"},
			{"-max-sessions", "0", "cmd"},
			{"-idle-timeout", "5x", "cmd"},
			{"-idle-timeout", "-1s", "cmd"},
			{"-nope", "cmd"},
		} {
			if _, err := parseConfig(args, envOf(nil)); err == nil {
				t.Errorf("parseConfig(%v) succeeded, want error", args)
			}
		}
		if _, err := parseConfig([]string{"cmd"}, envOf(map[string]string{"MCP_RUNNER_MAX_SESSIONS": "abc"})); err == nil {
			t.Error("bad MCP_RUNNER_MAX_SESSIONS env accepted")
		}
		if _, err := parseConfig([]string{"cmd"}, envOf(map[string]string{"MCP_RUNNER_IDLE_TIMEOUT": "5x"})); err == nil {
			t.Error("bad MCP_RUNNER_IDLE_TIMEOUT env accepted")
		}
	})
	t.Run("command and args preserved", func(t *testing.T) {
		cfg, err := parseConfig([]string{"-listen", ":1", "--", "npx", "-y", "pkg", "--flag"}, envOf(nil))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.command != "npx" || !reflect.DeepEqual(cfg.args, []string{"-y", "pkg", "--flag"}) {
			t.Fatalf("command %q args %q", cfg.command, cfg.args)
		}
		cfg, err = parseConfig([]string{"npx", "-y", "pkg"}, envOf(nil))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.command != "npx" || !reflect.DeepEqual(cfg.args, []string{"-y", "pkg"}) {
			t.Fatalf("command %q args %q", cfg.command, cfg.args)
		}
	})
}

func TestUsageNoCommand(t *testing.T) {
	var out syncBuffer
	err := run(context.Background(), nil, envOf(nil), nil, &out)
	if err == nil || !strings.Contains(err.Error(), "usage: mcp-runner") {
		t.Fatalf("err = %v", err)
	}
	var ue *usageErr
	if !errors.As(err, &ue) {
		t.Fatalf("err %T is not a usage error", err)
	}
}

func TestVersion(t *testing.T) {
	var out syncBuffer
	if err := run(context.Background(), []string{"-version"}, envOf(nil), nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "dev" {
		t.Fatalf("version output %q", got)
	}
}

func TestMissingCommandFailsFast(t *testing.T) {
	var out syncBuffer
	err := run(context.Background(), []string{"definitely-not-on-path-xyz"}, envOf(nil), nil, &out)
	if err == nil || !strings.Contains(err.Error(), "definitely-not-on-path-xyz") {
		t.Fatalf("err = %v", err)
	}
}

// The child runs in the workdir, not the runner's cwd, so a relative
// command must be made absolute against the runner's cwd.
func TestResolveCommandRelative(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got, err := resolveCommand("./server")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(dir, "server"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("resolveCommand(./server) = %q, want an absolute path", got)
	}
	if g, _ := filepath.EvalSymlinks(got); g != want {
		t.Fatalf("resolveCommand(./server) = %q, want %q", got, want)
	}
	if _, err := resolveCommand("./missing"); err == nil {
		t.Fatal("resolveCommand(./missing) succeeded")
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

func TestServeAndShutdown(t *testing.T) {
	bin := testutil.BuildFakeUpstream(t)
	addr := freeAddr(t)
	const token = "s3cret-bearer"
	env := map[string]string{"MCP_RUNNER_BEARER_TOKEN": token}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-listen", addr, "-workdir", t.TempDir(), "--", bin},
			envOf(env), []string{"PATH=" + os.Getenv("PATH")}, &out)
	}()

	url := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(url + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v\nlogs: %s", err, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// No token: rejected.
	resp, err := http.Post(url+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}

	hc := &http.Client{Transport: &bearerTransport{token: token}}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("echo returned error: %+v", res)
	}

	// A long call in flight when shutdown starts.
	go cs.CallTool(ctx, &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 60000}})
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(shutdownTimeout + 2*time.Second):
		t.Fatal("run did not return after cancel")
	}
	t.Logf("shutdown took %s", time.Since(start))

	logs := out.String()
	if !strings.Contains(logs, "mcp-runner listening") {
		t.Errorf("no startup log: %s", logs)
	}
	if strings.Contains(logs, token) {
		t.Errorf("bearer token leaked into logs: %s", logs)
	}
	if !strings.Contains(logs, `"auth":true`) {
		t.Errorf("startup log lacks auth=true: %s", logs)
	}
}

func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("Read/Write timeouts must be unset, got %s/%s", srv.ReadTimeout, srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 2*time.Minute {
		t.Errorf("header %s idle %s", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
}

type bearerTransport struct{ token string }

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
