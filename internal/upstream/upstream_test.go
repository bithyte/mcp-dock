package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/runner"
	"github.com/bithyte/mcp-dock/internal/testutil"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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

// start launches an upstream supervisor for the fake behind a runner. mod
// may adjust the upstream options.
func start(t *testing.T, mod func(*Options)) *harness {
	t.Helper()
	url, _ := testutil.StartRunner(t, testutil.RunnerOptions{})
	return startURL(t, url, mod)
}

// startWith is start with runner options.
func startWith(t *testing.T, ro testutil.RunnerOptions, mod func(*Options)) *harness {
	t.Helper()
	url, _ := testutil.StartRunner(t, ro)
	return startURL(t, url, mod)
}

// childEnv returns runner options adding env vars to the fake child.
func childEnv(kv ...string) testutil.RunnerOptions {
	return testutil.RunnerOptions{Mod: func(o *runner.Options) { o.Env = append(o.Env, kv...) }}
}

// startURL launches an upstream supervisor for the given endpoint.
func startURL(t *testing.T, url string, mod func(*Options)) *harness {
	t.Helper()
	h := &harness{logs: &syncBuffer{}, runDone: make(chan struct{})}
	opts := Options{
		Name:       "fake",
		URL:        url,
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
	h := startWith(t, childEnv("FAKE_CANCEL_FILE="+cancelFile), nil)
	h.waitUp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	begin := time.Now()
	_, err := h.u.Call(ctx, "sleep", json.RawMessage(`{"ms":5000}`), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled call err = %v, must not be ErrUnavailable", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("Call took %v after cancel", d)
	}
	eventually(t, 5*time.Second, "cancel file written", func() bool {
		b, err := os.ReadFile(cancelFile)
		return err == nil && strings.TrimSpace(string(b)) == "cancelled"
	})
}

func TestRPCErrorNotUnavailable(t *testing.T) {
	h := start(t, nil)
	h.waitUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Unknown tool: the upstream answers with a JSON-RPC error and stays up.
	_, err := h.u.Call(ctx, "no_such_tool", json.RawMessage(`{}`), nil)
	if err == nil {
		t.Fatal("unknown tool returned no error")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("JSON-RPC error mapped to ErrUnavailable: %v", err)
	}
	if got := call(t, h.u, "echo", `{"text":"still up"}`); got != "still up" {
		t.Fatalf("echo = %q", got)
	}
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
	// The runner ends the POST stream without a response when the child
	// dies, so the call fails with a transport error (the SDK's "request
	// terminated without response"), not an answer from the upstream. It
	// isn't mapped to ErrUnavailable: the session hasn't been seen to end
	// yet, and the call may have had effects, so it's not retried either.
	res, err := h.u.Call(ctx, "crash", json.RawMessage(`{}`), nil)
	if err == nil {
		t.Fatalf("crash call returned %+v, want an error", res)
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		t.Fatalf("crash call err = %v, want a transport failure, not a JSON-RPC error", err)
	}
	// The runner ends the session, so the GET stream's reconnect gets a 404
	// and the session ends without any further call.
	eventually(t, 10*time.Second, "tools nil after crash", func() bool { return h.sawDownAfter(before) })
	h.waitUp(t)
	if got := call(t, h.u, "echo", `{"text":"back"}`); got != "back" {
		t.Fatalf("echo after restart = %q", got)
	}
}

// startFailures counts "upstream start failed" warnings for the fake upstream.
func startFailures(t *testing.T, h *harness) int {
	n := 0
	for _, r := range h.logs.records(t) {
		if r["upstream"] == h.u.Name() && r["msg"] == "upstream start failed" {
			n++
		}
	}
	return n
}

func TestUnreachableRetries(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	h := startURL(t, "http://"+addr+"/mcp", nil)
	eventually(t, 10*time.Second, "at least 2 failed attempts logged", func() bool { return startFailures(t, h) >= 2 })
	select {
	case <-h.runDone:
		t.Fatal("Run returned early")
	default:
	}
	if h.u.Tools() != nil {
		t.Fatal("Tools() non-nil for unreachable upstream")
	}
	_, err = h.u.Call(context.Background(), "echo", json.RawMessage(`{"text":"x"}`), nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestFailingStartThenRecovers(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	h := startWith(t, testutil.RunnerOptions{Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if failing.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}}, nil)
	eventually(t, 10*time.Second, "at least 2 failed starts logged", func() bool { return startFailures(t, h) >= 2 })
	if h.u.Tools() != nil {
		t.Fatal("Tools() non-nil while start fails")
	}
	failing.Store(false)
	h.waitUp(t)
	if got := call(t, h.u, "echo", `{"text":"ok"}`); got != "ok" {
		t.Fatalf("echo = %q", got)
	}
}

func TestBearerSent(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	record := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			auths = append(auths, r.Header.Get("Authorization"))
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
	h := startWith(t, testutil.RunnerOptions{Wrap: record}, func(o *Options) { o.BearerToken = "tok" })
	h.waitUp(t)
	if got := call(t, h.u, "echo", `{"text":"hi"}`); got != "hi" {
		t.Fatalf("echo = %q", got)
	}
	mu.Lock()
	got := slices.Clone(auths)
	mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("only %d requests recorded", len(got))
	}
	for i, a := range got {
		if a != "Bearer tok" {
			t.Fatalf("request %d Authorization = %q, want Bearer tok", i, a)
		}
	}

	// Runner requires a token the Dock doesn't have.
	ro := testutil.RunnerOptions{Mod: func(o *runner.Options) { o.BearerToken = "tok" }}
	h = startWith(t, ro, func(o *Options) { o.BearerToken = "wrong" })
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if h.u.Tools() != nil {
			t.Fatal("upstream came up with the wrong bearer token")
		}
	}
	if startFailures(t, h) == 0 {
		t.Fatal("no start failure logged with the wrong bearer token")
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

// expirer is a runner Wrap that makes the first session look gone: GETs get
// 405 (so the SDK runs without a standalone stream and can't notice a lost
// session on its own), and once expire is set every request carrying the
// first session id gets 404, before the runner sees it. It counts tools/call
// POSTs per session id.
type expirer struct {
	expire atomic.Bool

	mu    sync.Mutex
	first string         // first session id seen
	calls map[string]int // tools/call POSTs per session id
}

func newExpirer() *expirer { return &expirer{calls: map[string]int{}} }

func (x *expirer) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Mcp-Session-Id")
		isCall := false
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			isCall = bytes.Contains(body, []byte(`"tools/call"`))
		}
		x.mu.Lock()
		if x.first == "" && id != "" {
			x.first = id
		}
		if isCall {
			x.calls[id]++
		}
		old := id != "" && id == x.first
		x.mu.Unlock()
		if r.Method == http.MethodGet {
			http.Error(w, "no standalone stream", http.StatusMethodNotAllowed)
			return
		}
		if old && x.expire.Load() {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// counts returns the tools/call POST count on the first session and per
// other session id.
func (x *expirer) counts() (first int, others map[string]int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	others = map[string]int{}
	for id, n := range x.calls {
		if id == x.first {
			first = n
		} else {
			others[id] = n
		}
	}
	return first, others
}

func hasLog(t *testing.T, h *harness, msg string) bool {
	return slices.ContainsFunc(h.logs.records(t), func(r map[string]any) bool { return r["msg"] == msg })
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// With no GET stream, only the ping notices the session is gone.
func TestPingDetectsDeadSession(t *testing.T) {
	x := newExpirer()
	h := startWith(t, testutil.RunnerOptions{Wrap: x.wrap}, func(o *Options) { o.PingInterval = 100 * time.Millisecond })
	h.waitUp(t)
	before := h.changeCount()
	x.expire.Store(true)
	eventually(t, 3*time.Second, "down after expire", func() bool { return h.sawDownAfter(before) })
	eventually(t, 3*time.Second, "up again", func() bool { return h.u.Tools() != nil })
	if !hasLog(t, h, "upstream ping failed") {
		t.Fatal("no upstream ping failed log")
	}
	if first, others := x.counts(); first != 0 || len(others) != 0 {
		t.Fatalf("tool calls made: first %d, others %v", first, others)
	}
}

// A call on a session the runner no longer knows gets 404 before reaching a
// child, so it's retried once on the new session.
func TestRetryOnSessionMissing(t *testing.T) {
	x := newExpirer()
	h := startWith(t, testutil.RunnerOptions{Wrap: x.wrap}, func(o *Options) { o.PingInterval = time.Hour })
	h.waitUp(t)
	x.expire.Store(true)
	if got := call(t, h.u, "echo", `{"text":"hi"}`); got != "hi" {
		t.Fatalf("echo = %q, want hi", got)
	}
	first, others := x.counts()
	if first != 1 || len(others) != 1 {
		t.Fatalf("tools/call POSTs: first session %d, others %v; want 1 and one other with 1", first, others)
	}
	for id, n := range others {
		if n != 1 {
			t.Fatalf("session %s got %d tools/call POSTs, want 1", id, n)
		}
	}
}

// A call that reached the child is never retried, even though the child
// died with it.
func TestNoRetryAfterSend(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	h := startWith(t, childEnv("FAKE_CALL_LOG="+log), nil)
	h.waitUp(t)
	before := h.changeCount()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if res, err := h.u.Call(ctx, "crash", json.RawMessage(`{}`), nil); err == nil {
		t.Fatalf("crash call returned %+v, want an error", res)
	}
	eventually(t, 10*time.Second, "down after crash", func() bool { return h.sawDownAfter(before) })
	h.waitUp(t)
	n := 0
	for _, l := range readLines(t, log) {
		if l == "crash" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("crash ran %d times, want 1 (log %v)", n, readLines(t, log))
	}
}

// A call already in flight when another request finds the session gone was
// sent, so it must not be retried. jsonrpc2 may retire such a call with the
// conn's session-missing error; with go-sdk v1.8.0 it's instead ended by the
// teardown (context canceled), so this passes even without Call's
// reached check. It's here so a change in either doesn't go unnoticed.
func TestNoRetryInFlightOnSessionMissing(t *testing.T) {
	x := newExpirer()
	log := filepath.Join(t.TempDir(), "calls")
	ro := childEnv("FAKE_CALL_LOG=" + log)
	ro.Wrap = x.wrap
	h := startWith(t, ro, func(o *Options) { o.PingInterval = time.Hour })
	h.waitUp(t)
	slowErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := h.u.Call(ctx, "sleep", json.RawMessage(`{"ms":1500}`), nil)
		slowErr <- err
	}()
	// No GET stream, so no progress either (the runner relays child
	// notifications on it); the call log shows the child got the call.
	eventually(t, 10*time.Second, "sleep reached the child", func() bool {
		return slices.Contains(readLines(t, log), "sleep")
	})
	x.expire.Store(true)
	if got := call(t, h.u, "echo", `{"text":"hi"}`); got != "hi" {
		t.Fatalf("echo = %q, want hi", got)
	}
	select {
	case err := <-slowErr:
		if err == nil {
			t.Fatal("in-flight sleep succeeded after its session was lost")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight sleep never returned")
	}
	// Give a wrongly retried sleep time to show up in the log.
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, l := range readLines(t, log) {
		if l == "sleep" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("sleep ran %d times, want 1 (log %v)", n, readLines(t, log))
	}
}
