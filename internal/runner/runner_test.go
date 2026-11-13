package runner_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/runner"
	"github.com/bithyte/mcp-dock/internal/testutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testutil.Cleanup()
	os.Exit(code)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns the JSON log records written so far.
func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	data := slices.Clone(b.buf.Bytes())
	b.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// hasRecord reports whether some record matches every key/value in want.
func (b *syncBuffer) hasRecord(t *testing.T, want map[string]any) bool {
	t.Helper()
	for _, rec := range b.records(t) {
		ok := true
		for k, v := range want {
			if rec[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for: %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start runs a runner with a JSON log sink, applying mod to its options.
func start(t *testing.T, mod func(*runner.Options)) (string, *runner.Runner, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	url, r := testutil.StartRunner(t, testutil.RunnerOptions{Mod: func(o *runner.Options) {
		o.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		if mod != nil {
			mod(o)
		}
	}})
	return url, r, logs
}

func connect(t *testing.T, url string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, opts)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s: tool error %+v", name, res.Content)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`

// rawPost sends body to url with the headers a streamable client uses.
func rawPost(t *testing.T, url, sessionID, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func do(t *testing.T, method, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestInitializeListCall(t *testing.T) {
	url, r, _ := start(t, nil)
	cs := connect(t, url, nil)
	if names := toolNames(t, cs); !slices.Contains(names, "echo") {
		t.Fatalf("tools %v lack echo", names)
	}
	if got := callText(t, cs, "echo", map[string]any{"text": "hi"}); got != "hi" {
		t.Fatalf("echo = %q", got)
	}
	if n := r.Sessions(); n != 1 {
		t.Fatalf("Sessions() = %d, want 1", n)
	}
}

func TestChildPerSession(t *testing.T) {
	url, r, _ := start(t, nil)
	a := connect(t, url, nil)
	connect(t, url, nil)
	if n := r.Sessions(); n != 2 {
		t.Fatalf("Sessions() = %d, want 2", n)
	}
	a.Close()
	eventually(t, 5*time.Second, "one session left", func() bool { return r.Sessions() == 1 })
}

func TestChildEnvAndCwd(t *testing.T) {
	var workDir string
	url, _, _ := start(t, func(o *runner.Options) {
		o.Env = append(o.Env, "FOO=bar")
		workDir = o.WorkDir
	})
	cs := connect(t, url, nil)
	if got := callText(t, cs, "getenv", map[string]any{"name": "FOO"}); got != "bar" {
		t.Fatalf("FOO = %q", got)
	}
	if got := callText(t, cs, "getenv", map[string]any{"name": "HOME"}); got != "<unset>" {
		t.Fatalf("HOME = %q, want <unset>", got)
	}
	if got := callText(t, cs, "cwd", map[string]any{}); got != workDir {
		t.Fatalf("cwd = %q, want %q", got, workDir)
	}
}

func TestWorkDirCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	url, _, _ := start(t, func(o *runner.Options) { o.WorkDir = dir })
	cs := connect(t, url, nil)
	if got := callText(t, cs, "cwd", map[string]any{}); got != dir {
		t.Fatalf("cwd = %q, want %q", got, dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("WorkDir perm = %o, want 700", perm)
	}
}

func TestProgressRelayed(t *testing.T) {
	url, _, _ := start(t, nil)
	var mu sync.Mutex
	n := 0
	cs := connect(t, url, &mcp.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			n++
			mu.Unlock()
		},
	})
	params := &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 300}}
	params.SetProgressToken("tok")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cs.CallTool(ctx, params); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	eventually(t, 5*time.Second, "a progress notification", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return n > 0
	})
}

func TestListChangedRelayed(t *testing.T) {
	url, _, _ := start(t, nil)
	changed := make(chan struct{}, 10)
	cs := connect(t, url, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			changed <- struct{}{}
		},
	})
	callText(t, cs, "add_tool", map[string]any{})
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed notification")
	}
	if names := toolNames(t, cs); !slices.Contains(names, "late") {
		t.Fatalf("tools %v lack late", names)
	}
}

func TestCancelRelayed(t *testing.T) {
	cancelFile := filepath.Join(t.TempDir(), "cancel")
	url, _, _ := start(t, func(o *runner.Options) {
		o.Env = append(o.Env, "FAKE_CANCEL_FILE="+cancelFile)
	})
	progress := make(chan struct{}, 1)
	cs := connect(t, url, &mcp.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) {
			select {
			case progress <- struct{}{}:
			default:
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	params := &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 10000}}
	params.SetProgressToken("tok")
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, params)
		done <- err
	}()
	// A progress notification means the child is inside the call.
	select {
	case <-progress:
	case <-time.After(5 * time.Second):
		t.Fatal("no progress notification before cancel")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled call returned no error")
	}
	eventually(t, 5*time.Second, "cancel file written", func() bool {
		b, err := os.ReadFile(cancelFile)
		return err == nil && string(b) == "cancelled"
	})
}

func TestChildExitEndsSession(t *testing.T) {
	url, r, logs := start(t, nil)
	cs := connect(t, url, nil)
	sid := cs.ID()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "crash", Arguments: map[string]any{}})
	if err == nil && !res.IsError {
		t.Fatal("crash call succeeded")
	}
	eventually(t, 5*time.Second, "sessions at 0", func() bool { return r.Sessions() == 0 })
	resp := rawPost(t, url, sid, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST on dead session: status %d, want 404", resp.StatusCode)
	}
	eventually(t, 5*time.Second, "session ended log", func() bool {
		return logs.hasRecord(t, map[string]any{"msg": "session ended", "reason": "child_exit", "session": sid})
	})
}

func TestDeleteEndsSession(t *testing.T) {
	url, r, logs := start(t, nil)
	cs := connect(t, url, nil)
	sid := cs.ID()
	if err := cs.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	eventually(t, 5*time.Second, "sessions at 0", func() bool { return r.Sessions() == 0 })
	eventually(t, 5*time.Second, "session ended log", func() bool {
		return logs.hasRecord(t, map[string]any{"msg": "session ended", "reason": "delete", "session": sid})
	})
}

func TestStderrLogged(t *testing.T) {
	url, _, logs := start(t, nil)
	cs := connect(t, url, nil)
	eventually(t, 5*time.Second, "stderr line logged", func() bool {
		return logs.hasRecord(t, map[string]any{"line": "hello stderr", "session": cs.ID()})
	})
}

func TestSessionStartedLogged(t *testing.T) {
	url, _, logs := start(t, nil)
	cs := connect(t, url, nil)
	if !logs.hasRecord(t, map[string]any{"msg": "session started", "session": cs.ID(), "sessions": float64(1)}) {
		t.Fatal("no session started log")
	}
}

func TestNonInitializeWithoutSession(t *testing.T) {
	url, r, _ := start(t, nil)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`[` + initBody + `]`,
		`not json`,
	} {
		resp := rawPost(t, url, "", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, resp.StatusCode)
		}
	}
	if n := r.Sessions(); n != 0 {
		t.Fatalf("Sessions() = %d, want 0", n)
	}
}

func TestChildStartFailure(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		url, r, _ := start(t, func(o *runner.Options) { o.Command = "/nonexistent" })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		if cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil); err == nil {
			cs.Close()
			t.Fatal("connect succeeded")
		}
		resp := rawPost(t, url, "", initBody)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("raw initialize: status %d, want 502", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "upstream failed to start") {
			t.Fatalf("body %q", body)
		}
		eventually(t, 5*time.Second, "sessions at 0", func() bool { return r.Sessions() == 0 })
	})
	t.Run("child exits at start", func(t *testing.T) {
		failFile := filepath.Join(t.TempDir(), "fail")
		if err := os.WriteFile(failFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		url, r, _ := start(t, func(o *runner.Options) {
			o.Env = append(o.Env, "FAKE_FAIL_START="+failFile)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		began := time.Now()
		if cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil); err == nil {
			cs.Close()
			t.Fatal("connect succeeded")
		} else if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("connect hung until deadline: %v", err)
		}
		if d := time.Since(began); d > 5*time.Second {
			t.Fatalf("connect took %v to fail", d)
		}
		eventually(t, 5*time.Second, "sessions at 0", func() bool { return r.Sessions() == 0 })
	})
}

func TestRouting(t *testing.T) {
	url, _, _ := start(t, nil)
	base := strings.TrimSuffix(url, "/mcp")

	resp := do(t, http.MethodGet, base+"/healthz", nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("healthz: %d %q", resp.StatusCode, body)
	}
	if resp := do(t, http.MethodGet, url, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET without session: %d, want 400", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, url, map[string]string{"Mcp-Session-Id": "nope"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET unknown session: %d, want 404", resp.StatusCode)
	}
	if resp := do(t, http.MethodDelete, url, map[string]string{"Mcp-Session-Id": "nope"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE unknown session: %d, want 404", resp.StatusCode)
	}
	if resp := do(t, http.MethodDelete, url, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE without session: %d, want 400", resp.StatusCode)
	}
	if resp := rawPost(t, url, "nope", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST unknown session: %d, want 404", resp.StatusCode)
	}
	resp = do(t, http.MethodPut, url, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, POST, DELETE" {
		t.Errorf("PUT: %d Allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}

	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("POST text/plain: %d, want 415", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST without event-stream Accept: %d, want 400", resp.StatusCode)
	}
}

func TestBodyTooLarge(t *testing.T) {
	url, r, _ := start(t, func(o *runner.Options) { o.MaxBodyBytes = 64 })
	resp := rawPost(t, url, "", initBody)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized initialize: %d, want 413", resp.StatusCode)
	}
	if n := r.Sessions(); n != 0 {
		t.Fatalf("Sessions() = %d, want 0", n)
	}
}

func TestCloseRefusesNew(t *testing.T) {
	url, r, _ := start(t, nil)
	connect(t, url, nil)
	r.Close()
	if n := r.Sessions(); n != 0 {
		t.Fatalf("Sessions() after Close = %d, want 0", n)
	}
	resp := rawPost(t, url, "", initBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("initialize after Close: %d, want 503", resp.StatusCode)
	}
	if n := r.Sessions(); n != 0 {
		t.Fatalf("Sessions() = %d, want 0", n)
	}
}
