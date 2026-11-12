package runner_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/runner"
	"github.com/bithyte/mcp-dock/internal/testutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerRT adds fixed headers to every request.
type headerRT struct{ hdr map[string]string }

func (h headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.hdr {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// postWith sends a raw POST with the given extra headers and returns the
// response with its body drained.
func postWith(t *testing.T, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestBearerRequired(t *testing.T) {
	url, r, _ := start(t, func(o *runner.Options) { o.BearerToken = "s3cret" })
	for _, auth := range []string{"", "Bearer wrong", "Bearer s3cret-longer", "Basic s3cret", "Bearer", "Bearer "} {
		hdr := map[string]string{}
		if auth != "" {
			hdr["Authorization"] = auth
		}
		resp := postWith(t, url, initBody, hdr)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", auth, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("Authorization %q: WWW-Authenticate %q, want Bearer", auth, got)
		}
	}
	// GET and DELETE are checked too, before any session lookup.
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		resp := do(t, m, url, map[string]string{"Mcp-Session-Id": "nope"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token: status %d, want 401", m, resp.StatusCode)
		}
	}
	if n := r.Sessions(); n != 0 {
		t.Fatalf("Sessions() = %d after rejects, want 0", n)
	}

	resp := postWith(t, url, initBody, map[string]string{"Authorization": "bearer s3cret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lowercase scheme: status %d, want 200", resp.StatusCode)
	}

	hc := &http.Client{Transport: headerRT{map[string]string{"Authorization": "Bearer s3cret"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatalf("authorized connect: %v", err)
	}
	defer cs.Close()
	if got := callText(t, cs, "echo", map[string]any{"text": "hi"}); got != "hi" {
		t.Fatalf("echo = %q", got)
	}

	hz := do(t, http.MethodGet, strings.TrimSuffix(url, "/mcp")+"/healthz", nil)
	if hz.StatusCode != http.StatusOK {
		t.Fatalf("/healthz without token: %d, want 200", hz.StatusCode)
	}
}

func TestNoBearerTrustsNetwork(t *testing.T) {
	url, _, _ := start(t, nil)
	cs := connect(t, url, nil)
	if got := callText(t, cs, "echo", map[string]any{"text": "hi"}); got != "hi" {
		t.Fatalf("echo = %q", got)
	}
}

func TestSessionCap(t *testing.T) {
	url, r, _ := start(t, func(o *runner.Options) { o.MaxSessions = 2 })
	a := connect(t, url, nil)
	connect(t, url, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	if cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil); err == nil {
		cs.Close()
		t.Fatal("third connect succeeded")
	}
	resp := postWith(t, url, initBody, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("raw initialize over cap: %d, want 503", resp.StatusCode)
	}
	if n := r.Sessions(); n != 2 {
		t.Fatalf("Sessions() = %d, want 2", n)
	}

	a.Close()
	eventually(t, 5*time.Second, "sessions at 1", func() bool { return r.Sessions() == 1 })
	connect(t, url, nil)
	if n := r.Sessions(); n != 2 {
		t.Fatalf("Sessions() = %d, want 2", n)
	}
}

func TestSessionCapConcurrent(t *testing.T) {
	url, r, _ := start(t, func(o *runner.Options) { o.MaxSessions = 2 })
	const n = 10
	var accepted, refused atomic.Int32
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusServiceUnavailable {
				refused.Add(1)
			} else {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 2 || refused.Load() != n-2 {
		t.Fatalf("accepted %d, refused %d, want 2 and %d", accepted.Load(), refused.Load(), n-2)
	}
	if got := r.Sessions(); got != 2 {
		t.Fatalf("Sessions() = %d, want 2", got)
	}
}

func TestIdleTimeout(t *testing.T) {
	url, r, logs := start(t, func(o *runner.Options) { o.IdleTimeout = 200 * time.Millisecond })

	// A client holding the standalone GET stream stays alive.
	cs := connect(t, url, nil)
	// A raw initialize followed by nothing goes idle.
	postWith(t, url, initBody, nil)
	eventually(t, 2*time.Second, "raw session to go idle", func() bool { return r.Sessions() == 1 })
	eventually(t, 5*time.Second, "idle session ended log", func() bool {
		return logs.hasRecord(t, map[string]any{"msg": "session ended", "reason": "idle"})
	})
	time.Sleep(time.Second)
	if n := r.Sessions(); n != 1 {
		t.Fatalf("Sessions() = %d after 1s, want 1 (client holds GET stream)", n)
	}
	if got := callText(t, cs, "echo", map[string]any{"text": "still here"}); got != "still here" {
		t.Fatalf("echo = %q", got)
	}
}

func TestInFlightNotIdle(t *testing.T) {
	url, r, _ := start(t, func(o *runner.Options) { o.IdleTimeout = 200 * time.Millisecond })
	// No standalone GET stream, so only the POST is in flight.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if got := callText(t, cs, "sleep", map[string]any{"ms": 800}); got != "done" {
		t.Fatalf("sleep = %q, want done", got)
	}
	if n := r.Sessions(); n != 1 {
		t.Fatalf("Sessions() = %d, want 1", n)
	}
}

func TestBodyLimit(t *testing.T) {
	url, r, _ := start(t, func(o *runner.Options) { o.MaxBodyBytes = 1024 })
	big := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"pad":"` + strings.Repeat("x", 2048) + `"}}`
	resp := postWith(t, url, big, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("2 KiB initialize: %d, want 413", resp.StatusCode)
	}
	if n := r.Sessions(); n != 0 {
		t.Fatalf("Sessions() = %d, want 0", n)
	}
}

// TestCloseDuringSpawn races Close against initializes that are starting
// children. Every child that ever started must be gone once Close returns.
func TestCloseDuringSpawn(t *testing.T) {
	bin := testutil.BuildFakeUpstream(t)
	pidFile := filepath.Join(t.TempDir(), "pids")
	url, r, _ := start(t, func(o *runner.Options) {
		o.Command = "sh"
		o.Args = []string{"-c", `echo $$ >> "$0"; exec "$1"`, pidFile, bin}
		o.MaxSessions = 16
	})
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			resp, err := http.DefaultClient.Do(req.WithContext(ctx))
			if err != nil {
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	time.Sleep(5 * time.Millisecond)
	r.Close()
	if n := r.Sessions(); n != 0 {
		t.Fatalf("Sessions() after Close = %d, want 0", n)
	}
	wg.Wait()
	if n := r.Sessions(); n != 0 {
		t.Fatalf("Sessions() after requests drained = %d, want 0", n)
	}
	data, _ := os.ReadFile(pidFile)
	for _, f := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(pid, 0); err == nil {
			t.Errorf("child %d still alive after Close", pid)
		}
	}
}

// spawnMarked wraps the child command so each spawn appends a line to the
// returned file.
func spawnMarked(t *testing.T, o *runner.Options) string {
	mark := filepath.Join(t.TempDir(), "spawns")
	o.Args = append([]string{"-c", `echo x >> "$SPAWN_MARK"; exec "$0" "$@"`, o.Command}, o.Args...)
	o.Command = "/bin/sh"
	o.Env = append(o.Env, "SPAWN_MARK="+mark)
	return mark
}

func spawns(t *testing.T, mark string) int {
	data, err := os.ReadFile(mark)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// Browsers always send Origin and the Dock never does, so any /mcp request
// with one is refused before anything else, token or not (DNS rebinding).
func TestOriginRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		auth  string
	}{
		{"no token configured", "", ""},
		{"valid bearer", "s3cret", "Bearer s3cret"},
		{"wrong bearer", "s3cret", "Bearer wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mark string
			url, r, _ := start(t, func(o *runner.Options) {
				o.BearerToken = tc.token
				mark = spawnMarked(t, o)
			})
			hdr := map[string]string{"Origin": "http://evil.example"}
			if tc.auth != "" {
				hdr["Authorization"] = tc.auth
			}
			if resp := postWith(t, url, initBody, hdr); resp.StatusCode != http.StatusForbidden {
				t.Fatalf("initialize with Origin: status %d, want 403", resp.StatusCode)
			}
			for _, m := range []string{http.MethodGet, http.MethodDelete} {
				h := map[string]string{"Mcp-Session-Id": "nope", "Origin": "null"}
				if tc.auth != "" {
					h["Authorization"] = tc.auth
				}
				if resp := do(t, m, url, h); resp.StatusCode != http.StatusForbidden {
					t.Errorf("%s with Origin: status %d, want 403", m, resp.StatusCode)
				}
			}
			if n := r.Sessions(); n != 0 {
				t.Fatalf("Sessions() = %d after rejects, want 0", n)
			}
			if n := spawns(t, mark); n != 0 {
				t.Fatalf("%d children spawned for rejected requests", n)
			}

			// Without Origin the same request works.
			delete(hdr, "Origin")
			if tc.auth == "Bearer wrong" {
				hdr["Authorization"] = "Bearer s3cret"
			}
			if resp := postWith(t, url, initBody, hdr); resp.StatusCode != http.StatusOK {
				t.Fatalf("initialize without Origin: status %d, want 200", resp.StatusCode)
			}
			if n := spawns(t, mark); n != 1 {
				t.Fatalf("%d children spawned, want 1", n)
			}
			hz := do(t, http.MethodGet, strings.TrimSuffix(url, "/mcp")+"/healthz", map[string]string{"Origin": "http://evil.example"})
			if hz.StatusCode != http.StatusOK {
				t.Fatalf("/healthz with Origin: %d, want 200", hz.StatusCode)
			}
		})
	}
}
