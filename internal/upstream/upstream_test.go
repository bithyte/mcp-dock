package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/testutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

type harness struct {
	u       *Upstream
	logs    *syncBuffer
	runDone chan struct{}
	stop    context.CancelFunc // cancels Run's ctx

	mu      sync.Mutex
	changes []bool // per OnToolsChanged call: was Tools() non-nil at that moment
}

func (h *harness) changeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.changes)
}

func (h *harness) sawDownAfter(i int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, up := range h.changes[min(i, len(h.changes)):] {
		if !up {
			return true
		}
	}
	return false
}

func (h *harness) hasTool(name string) bool {
	return slices.ContainsFunc(h.u.Tools(), func(t *mcp.Tool) bool { return t.Name == name })
}

func (h *harness) waitUp(t *testing.T) {
	t.Helper()
	eventually(t, 10*time.Second, "upstream up", func() bool { return h.u.Tools() != nil })
}

// start launches an upstream supervisor for the fake. mod may adjust options.
func start(t *testing.T, mod func(*Options)) *harness {
	t.Helper()
	bin := testutil.BuildFakeUpstream(t)
	h := &harness{logs: &syncBuffer{}, runDone: make(chan struct{})}
	opts := Options{
		Name:       "fake",
		Command:    bin,
		Env:        []string{"PATH=" + os.Getenv("PATH")},
		WorkDir:    filepath.Join(t.TempDir(), "upstreams", "fake", "cwd"),
		Logger:     slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		MinBackoff: 50 * time.Millisecond,
		MaxBackoff: 200 * time.Millisecond,
		OnToolsChanged: func() {
			up := h.u.Tools() != nil
			h.mu.Lock()
			h.changes = append(h.changes, up)
			h.mu.Unlock()
		},
	}
	if mod != nil {
		mod(&opts)
	}
	h.u = New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	go func() {
		defer close(h.runDone)
		h.u.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.runDone:
		case <-time.After(15 * time.Second):
			t.Error("Run did not return after ctx cancel")
		}
	})
	return h
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("want 1 content item, got %+v", res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want TextContent, got %T", res.Content[0])
	}
	return tc.Text
}

func call(t *testing.T, u *Upstream, tool, args string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := u.Call(ctx, tool, json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("Call(%s): %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("Call(%s) returned tool error: %+v", tool, res.Content)
	}
	return textOf(t, res)
}

func TestStartListsTools(t *testing.T) {
	h := start(t, nil)
	if h.u.Name() != "fake" {
		t.Fatalf("Name() = %q", h.u.Name())
	}
	h.waitUp(t)
	for _, name := range []string{"echo", "sleep", "crash", "getenv", "cwd", "notify", "add_tool"} {
		if !h.hasTool(name) {
			t.Errorf("tool %q missing", name)
		}
	}
	eventually(t, 5*time.Second, "OnToolsChanged called", func() bool { return h.changeCount() >= 1 })
}

func TestCallEcho(t *testing.T) {
	h := start(t, nil)
	h.waitUp(t)
	if got := call(t, h.u, "echo", `{"text":"hi"}`); got != "hi" {
		t.Fatalf("echo = %q, want hi", got)
	}
}

func TestMinimalEnvAndCwd(t *testing.T) {
	t.Setenv("SECRET", "x")
	h := start(t, func(o *Options) {
		o.Env = []string{"PATH=" + os.Getenv("PATH"), "FOO=bar"}
	})
	h.waitUp(t)
	if got := call(t, h.u, "getenv", `{"name":"FOO"}`); got != "bar" {
		t.Errorf("FOO = %q, want bar", got)
	}
	if got := call(t, h.u, "getenv", `{"name":"SECRET"}`); got != "<unset>" {
		t.Errorf("SECRET = %q, want <unset>", got)
	}
	want, err := filepath.EvalSymlinks(h.u.opts.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(call(t, h.u, "cwd", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

func TestProgressForwarded(t *testing.T) {
	h := start(t, nil)
	h.waitUp(t)
	var n atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.u.Call(ctx, "sleep", json.RawMessage(`{"ms":300}`), func(p *mcp.ProgressNotificationParams) {
		n.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, res); got != "done" {
		t.Fatalf("sleep = %q, want done", got)
	}
	if n.Load() < 2 {
		t.Fatalf("onProgress called %d times, want >= 2", n.Load())
	}
}

func TestCancelPropagates(t *testing.T) {
	cancelFile := filepath.Join(t.TempDir(), "cancel")
	h := start(t, func(o *Options) {
		o.Env = append(o.Env, "FAKE_CANCEL_FILE="+cancelFile)
	})
	h.waitUp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	begin := time.Now()
	_, err := h.u.Call(ctx, "sleep", json.RawMessage(`{"ms":5000}`), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("Call took %v after cancel", d)
	}
	eventually(t, 5*time.Second, "cancel file written", func() bool {
		b, err := os.ReadFile(cancelFile)
		return err == nil && strings.TrimSpace(string(b)) == "cancelled"
	})
}

func TestCallsNotSerialized(t *testing.T) {
	h := start(t, nil)
	h.waitUp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inFlight := make(chan struct{})
	var once sync.Once
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		_, _ = h.u.Call(ctx, "sleep", json.RawMessage(`{"ms":10000}`), func(*mcp.ProgressNotificationParams) {
			once.Do(func() { close(inFlight) })
		})
	}()
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("slow call never reported progress")
	}
	// The slow call is still running; a second call must not wait for it.
	if got := call(t, h.u, "echo", `{"text":"parallel"}`); got != "parallel" {
		t.Fatalf("echo = %q", got)
	}
	select {
	case <-slowDone:
		t.Fatal("slow call finished early; test proves nothing")
	default:
	}
	cancel()
	<-slowDone
}

func TestShutdownWithInFlightCall(t *testing.T) {
	h := start(t, nil)
	h.waitUp(t)
	inFlight := make(chan struct{})
	var once sync.Once
	callErr := make(chan error, 1)
	go func() {
		// Background ctx: nothing but shutdown can end this call.
		_, err := h.u.Call(context.Background(), "sleep", json.RawMessage(`{"ms":60000}`), func(*mcp.ProgressNotificationParams) {
			once.Do(func() { close(inFlight) })
		})
		callErr <- err
	}()
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("call never reported progress")
	}
	h.stop()
	select {
	case <-h.runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Run blocked on shutdown by an in-flight call")
	}
	select {
	case err := <-callErr:
		if err == nil {
			t.Fatal("in-flight call returned nil error after shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call never returned")
	}
}

func TestCrashRestarts(t *testing.T) {
	h := start(t, nil)
	h.waitUp(t)
	before := h.changeCount()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.u.Call(ctx, "crash", json.RawMessage(`{}`), nil); err == nil {
		t.Fatal("crash call returned no error")
	}
	eventually(t, 10*time.Second, "tools nil after crash", func() bool { return h.sawDownAfter(before) })
	h.waitUp(t)
	if got := call(t, h.u, "echo", `{"text":"back"}`); got != "back" {
		t.Fatalf("echo after restart = %q", got)
	}
}

// failures counts "upstream start failed" style warnings for the fake upstream.
func failures(t *testing.T, h *harness) int {
	n := 0
	for _, r := range h.logs.records(t) {
		if r["upstream"] == h.u.Name() && r["level"] == "WARN" {
			n++
		}
	}
	return n
}

func TestMissingCommandRetries(t *testing.T) {
	h := start(t, func(o *Options) { o.Command = "/nonexistent" })
	eventually(t, 10*time.Second, "at least 3 failed attempts logged", func() bool { return failures(t, h) >= 3 })
	select {
	case <-h.runDone:
		t.Fatal("Run returned early")
	default:
	}
	if h.u.Tools() != nil {
		t.Fatal("Tools() non-nil for missing command")
	}
	_, err := h.u.Call(context.Background(), "echo", json.RawMessage(`{"text":"x"}`), nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestFailingStartThenRecovers(t *testing.T) {
	failFile := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(failFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := start(t, func(o *Options) {
		o.Env = append(o.Env, "FAKE_FAIL_START="+failFile)
	})
	eventually(t, 10*time.Second, "at least 2 failed starts logged", func() bool { return failures(t, h) >= 2 })
	if h.u.Tools() != nil {
		t.Fatal("Tools() non-nil while start fails")
	}
	if err := os.Remove(failFile); err != nil {
		t.Fatal(err)
	}
	h.waitUp(t)
	if got := call(t, h.u, "echo", `{"text":"ok"}`); got != "ok" {
		t.Fatalf("echo = %q", got)
	}
}

func TestListChanged(t *testing.T) {
	h := start(t, nil)
	h.waitUp(t)
	eventually(t, 5*time.Second, "initial OnToolsChanged", func() bool { return h.changeCount() >= 1 })
	before := h.changeCount()
	if h.hasTool("late") {
		t.Fatal("late present before add_tool")
	}
	call(t, h.u, "add_tool", `{}`)
	eventually(t, 10*time.Second, "late tool listed", func() bool { return h.hasTool("late") })
	eventually(t, 5*time.Second, "OnToolsChanged fired again", func() bool { return h.changeCount() > before })
}

func TestStderrLogged(t *testing.T) {
	h := start(t, nil)
	eventually(t, 10*time.Second, "stderr line logged", func() bool {
		for _, r := range h.logs.records(t) {
			if r["upstream"] != "fake" {
				continue
			}
			for _, v := range r {
				if s, ok := v.(string); ok && s == "hello stderr" {
					return true
				}
			}
		}
		return false
	})
}

func TestNextBackoff(t *testing.T) {
	const lo, hi = time.Second, 60 * time.Second
	cases := []struct {
		cur, up, want time.Duration
	}{
		{lo, 0, 2 * time.Second},
		{8 * time.Second, time.Second, 16 * time.Second},
		{32 * time.Second, 0, hi},
		{hi, 0, hi},
		{hi, 61 * time.Second, lo}, // stayed up long enough: reset
	}
	for _, c := range cases {
		wait, next := nextBackoff(c.cur, c.up, lo, hi)
		if c.up >= stableUptime {
			if wait != lo || next != 2*lo {
				t.Errorf("nextBackoff(%v, up %v) = %v,%v; want %v,%v", c.cur, c.up, wait, next, lo, 2*lo)
			}
			continue
		}
		if wait != c.cur || next != c.want {
			t.Errorf("nextBackoff(%v, up %v) = %v,%v; want %v,%v", c.cur, c.up, wait, next, c.cur, c.want)
		}
	}
}
