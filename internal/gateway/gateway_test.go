package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bithyte/mcp-dock/internal/upstream"
)

// fakeBackend is an in-process Backend. callFn, if set, handles calls;
// otherwise calls echo back a text result.
type fakeBackend struct {
	name string

	mu     sync.Mutex
	tools  []*mcp.Tool
	calls  []fakeCall
	callFn func(ctx context.Context, tool string, args json.RawMessage,
		onProgress func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error)
}

type fakeCall struct {
	tool string
	args json.RawMessage
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Tools() []*mcp.Tool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tools)
}

func (f *fakeBackend) setTools(ts []*mcp.Tool) {
	f.mu.Lock()
	f.tools = ts
	f.mu.Unlock()
}

func (f *fakeBackend) Call(ctx context.Context, tool string, args json.RawMessage,
	onProgress func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{tool: tool, args: slices.Clone(args)})
	fn := f.callFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, tool, args, onProgress)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok:" + tool}}}, nil
}

func (f *fakeBackend) gotCalls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func tool(name string, schema any) *mcp.Tool {
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	return &mcp.Tool{Name: name, Description: "desc " + name, InputSchema: schema}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// connect returns a client session connected to g's server.
func connect(t *testing.T, g *Gateway, copts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := g.Server().Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, copts)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		cs.Close()
		ss.Wait()
	})
	return cs
}

func listNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func twoBackends() (*fakeBackend, *fakeBackend) {
	jp := &fakeBackend{name: "jaypost", tools: []*mcp.Tool{tool("ask_user", nil), tool("notify_user", nil)}}
	ot := &fakeBackend{name: "other", tools: []*mcp.Tool{tool("echo", nil)}}
	return jp, ot
}

func TestToolsPrefixed(t *testing.T) {
	jp, ot := twoBackends()
	g := New(Options{Backends: []Backend{jp, ot}, Version: "test"})
	cs := connect(t, g, nil)
	got := listNames(t, cs)
	want := []string{"jaypost__ask_user", "jaypost__notify_user", "other__echo"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if impl := cs.InitializeResult().ServerInfo; impl.Name != "mcp-dock" || impl.Version != "test" {
		t.Errorf("server info = %+v", impl)
	}
}

func TestCallRoutes(t *testing.T) {
	jp, ot := twoBackends()
	g := New(Options{Backends: []Backend{jp, ot}})
	cs := connect(t, g, nil)
	args := json.RawMessage(`{"msg":"hi","n":1}`)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "other__echo", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError || resultText(res) != "ok:echo" {
		t.Fatalf("result = %+v (%q)", res, resultText(res))
	}
	calls := ot.gotCalls()
	if len(calls) != 1 || calls[0].tool != "echo" {
		t.Fatalf("calls = %+v", calls)
	}
	var got, want any
	if err := json.Unmarshal(calls[0].args, &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(args, &want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("args = %s, want %s", calls[0].args, args)
	}
	if len(jp.gotCalls()) != 0 {
		t.Fatalf("jaypost called: %+v", jp.gotCalls())
	}
}

func TestUnknownTool(t *testing.T) {
	jp, ot := twoBackends()
	g := New(Options{Backends: []Backend{jp, ot}})
	cs := connect(t, g, nil)
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "nope__x", Arguments: map[string]any{}})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestDenyArgs(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"attachment": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
					"name": map[string]any{"type": "string"},
				},
			},
		},
	}
	jp := &fakeBackend{name: "jaypost", tools: []*mcp.Tool{tool("ask_user", schema)}}
	g := New(Options{
		Backends: []Backend{jp},
		Policies: map[string]ArgPolicy{"jaypost": NewArgPolicy([]string{"attachment.path"})},
	})
	cs := connect(t, g, nil)

	lres, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(lres.Tools[0].InputSchema)
	if strings.Contains(string(b), `"path"`) || !strings.Contains(string(b), `"name"`) {
		t.Fatalf("listed schema = %s", b)
	}
	// The upstream's own tool must not be mutated.
	b, _ = json.Marshal(jp.Tools()[0].InputSchema)
	if !strings.Contains(string(b), `"path"`) {
		t.Fatalf("upstream schema mutated: %s", b)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jaypost__ask_user",
		Arguments: json.RawMessage(`{"attachment":{"path":"/etc/passwd"}}`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	txt := resultText(res)
	if !res.IsError || !strings.Contains(txt, "attachment.path") || !strings.Contains(txt, "not allowed") {
		t.Fatalf("result = %+v (%q)", res, txt)
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jaypost__ask_user",
		Arguments: json.RawMessage(`{"attachment":[{"name":"a"},{"path":"/etc/passwd"}]}`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "not allowed") {
		t.Fatalf("array: result = %+v (%q)", res, resultText(res))
	}
	if len(jp.gotCalls()) != 0 {
		t.Fatalf("backend called: %+v", jp.gotCalls())
	}
}

func TestDenyArgsInvalidJSONFailsClosed(t *testing.T) {
	jp := &fakeBackend{name: "jaypost", tools: []*mcp.Tool{tool("ask_user", nil)}}
	g := New(Options{
		Backends: []Backend{jp},
		Policies: map[string]ArgPolicy{"jaypost": NewArgPolicy([]string{"attachment.path"})},
	})
	h := g.handler(jp, "ask_user", "jaypost__ask_user")
	res, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Name:      "jaypost__ask_user",
		Arguments: json.RawMessage(`{"attachment":`),
	}})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError, got %+v", res)
	}
	if len(jp.gotCalls()) != 0 {
		t.Fatalf("backend called: %+v", jp.gotCalls())
	}
}

func TestInvalidToolNamesSkipped(t *testing.T) {
	long := strings.Repeat("x", 70)
	jp := &fakeBackend{name: "jaypost", tools: []*mcp.Tool{
		tool(long, nil), tool("a.b", nil), tool("ask_user", nil), tool("notify_user", nil),
	}}
	log, buf := newLogger()
	g := New(Options{Backends: []Backend{jp}, Logger: log})
	cs := connect(t, g, nil)
	got := listNames(t, cs)
	want := []string{"jaypost__ask_user", "jaypost__notify_user"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	logs := buf.String()
	for _, n := range []string{"jaypost__" + long, "jaypost__a.b"} {
		if !strings.Contains(logs, n) {
			t.Errorf("no warning naming %q in logs:\n%s", n, logs)
		}
	}
	if !strings.Contains(logs, `"level":"WARN"`) {
		t.Errorf("no WARN record:\n%s", logs)
	}
}

func TestBadSchemaSkipped(t *testing.T) {
	jp := &fakeBackend{name: "jaypost", tools: []*mcp.Tool{
		tool("ask_user", nil),
		{Name: "arr", InputSchema: map[string]any{"type": "array"}},
		{Name: "noschema"},
	}}
	log, buf := newLogger()
	g := New(Options{Backends: []Backend{jp}, Logger: log})
	cs := connect(t, g, nil)
	got := listNames(t, cs)
	want := []string{"jaypost__ask_user", "jaypost__noschema"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if !strings.Contains(buf.String(), "jaypost__arr") {
		t.Errorf("no warning for jaypost__arr:\n%s", buf.String())
	}
}

func TestDuplicateNamesSkipped(t *testing.T) {
	a := &fakeBackend{name: "dup", tools: []*mcp.Tool{tool("x", nil), tool("x", nil), tool("y", nil)}}
	b := &fakeBackend{name: "dup", tools: []*mcp.Tool{tool("y", nil)}}
	log, buf := newLogger()
	g := New(Options{Backends: []Backend{a, b}, Logger: log})
	cs := connect(t, g, nil)
	got := listNames(t, cs)
	want := []string{"dup__x", "dup__y"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "dup__y", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if len(a.gotCalls()) != 1 || len(b.gotCalls()) != 0 {
		t.Fatalf("first backend should win: a=%v b=%v", a.gotCalls(), b.gotCalls())
	}
	if !strings.Contains(buf.String(), "duplicate") {
		t.Errorf("no duplicate warning:\n%s", buf.String())
	}
}

func TestBackendDownRemovesTools(t *testing.T) {
	jp, ot := twoBackends()
	g := New(Options{Backends: []Backend{jp, ot}})
	changed := make(chan struct{}, 10)
	cs := connect(t, g, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed <- struct{}{} },
	})
	if n := len(listNames(t, cs)); n != 3 {
		t.Fatalf("want 3 tools, got %d", n)
	}
	jp.setTools(nil)
	g.Sync()
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed")
	}
	got := listNames(t, cs)
	if !slices.Equal(got, []string{"other__echo"}) {
		t.Fatalf("tools = %v", got)
	}
}

func TestListChangedWithZeroTools(t *testing.T) {
	jp := &fakeBackend{name: "jaypost"}
	g := New(Options{Backends: []Backend{jp}})
	changed := make(chan struct{}, 10)
	cs := connect(t, g, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed <- struct{}{} },
	})
	caps := cs.InitializeResult().Capabilities
	if caps == nil || caps.Tools == nil || !caps.Tools.ListChanged {
		t.Fatalf("tools.listChanged not advertised: %+v", caps)
	}
	jp.setTools([]*mcp.Tool{tool("ask_user", nil)})
	g.Sync()
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed")
	}
	if got := listNames(t, cs); !slices.Equal(got, []string{"jaypost__ask_user"}) {
		t.Fatalf("tools = %v", got)
	}
}

func TestToolChangeReplaced(t *testing.T) {
	jp := &fakeBackend{name: "jaypost", tools: []*mcp.Tool{tool("ask_user", nil)}}
	g := New(Options{Backends: []Backend{jp}})
	cs := connect(t, g, nil)
	updated := tool("ask_user", nil)
	updated.Description = "new description"
	jp.setTools([]*mcp.Tool{updated})
	g.Sync()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Description != "new description" {
		t.Fatalf("tools = %+v", res.Tools)
	}
}

func TestUnavailableCall(t *testing.T) {
	jp, ot := twoBackends()
	ot.callFn = func(context.Context, string, json.RawMessage, func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
		return nil, fmt.Errorf("%w: connection closed: /secret/path/socket", upstream.ErrUnavailable)
	}
	g := New(Options{Backends: []Backend{jp, ot}})
	cs := connect(t, g, nil)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "other__echo", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	txt := resultText(res)
	if !res.IsError || !strings.Contains(txt, "unavailable") {
		t.Fatalf("result = %+v (%q)", res, txt)
	}
	if strings.Contains(txt, "/secret") {
		t.Fatalf("leaked internals: %q", txt)
	}
}

func TestBackendErrorNoLeak(t *testing.T) {
	jp, ot := twoBackends()
	ot.callFn = func(context.Context, string, json.RawMessage, func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
		return nil, errors.New("open /home/someone/.secret: permission denied")
	}
	g := New(Options{Backends: []Backend{jp, ot}})
	cs := connect(t, g, nil)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "other__echo", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	txt := resultText(res)
	if !res.IsError || strings.Contains(txt, "/home") || strings.Contains(txt, "permission") {
		t.Fatalf("result = %+v (%q)", res, txt)
	}
}

func TestProgressRelayed(t *testing.T) {
	jp, ot := twoBackends()
	ot.callFn = func(_ context.Context, _ string, _ json.RawMessage, onProgress func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
		if onProgress == nil {
			return nil, errors.New("no onProgress")
		}
		onProgress(&mcp.ProgressNotificationParams{ProgressToken: "upstream-1", Progress: 1, Total: 2, Message: "half"})
		onProgress(&mcp.ProgressNotificationParams{ProgressToken: "upstream-1", Progress: 2, Total: 2})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
	}
	g := New(Options{Backends: []Backend{jp, ot}})
	var mu sync.Mutex
	var got []*mcp.ProgressNotificationParams
	cs := connect(t, g, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			got = append(got, req.Params)
			mu.Unlock()
		},
	})
	params := &mcp.CallToolParams{Name: "other__echo", Arguments: map[string]any{}}
	params.SetProgressToken("p1")
	res, err := cs.CallTool(context.Background(), params)
	if err != nil || res.IsError {
		t.Fatalf("CallTool: %v %+v", err, res)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("got %d progress notifications, want 2", len(got))
	}
	for _, p := range got {
		if p.ProgressToken != "p1" {
			t.Errorf("token = %v, want p1", p.ProgressToken)
		}
	}
	if got[0].Progress != 1 || got[0].Total != 2 || got[0].Message != "half" {
		t.Errorf("first = %+v", got[0])
	}
}

func TestNoProgressTokenNoCallback(t *testing.T) {
	jp, ot := twoBackends()
	var sawProgress bool
	ot.callFn = func(_ context.Context, _ string, _ json.RawMessage, onProgress func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
		sawProgress = onProgress != nil
		return &mcp.CallToolResult{}, nil
	}
	g := New(Options{Backends: []Backend{jp, ot}})
	cs := connect(t, g, nil)
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "other__echo", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if sawProgress {
		t.Fatal("onProgress passed without a client progress token")
	}
}

// A progress callback after the call has returned must not panic or block.
func TestLateProgressIgnored(t *testing.T) {
	jp, ot := twoBackends()
	var late func(*mcp.ProgressNotificationParams)
	ot.callFn = func(_ context.Context, _ string, _ json.RawMessage, onProgress func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
		late = onProgress
		return &mcp.CallToolResult{}, nil
	}
	g := New(Options{Backends: []Backend{jp, ot}})
	cs := connect(t, g, nil)
	params := &mcp.CallToolParams{Name: "other__echo", Arguments: map[string]any{}}
	params.SetProgressToken("p1")
	if _, err := cs.CallTool(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	if late == nil {
		t.Fatal("no onProgress")
	}
	done := make(chan struct{})
	go func() {
		for range 100 {
			late(&mcp.ProgressNotificationParams{Progress: 1})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("late onProgress blocked")
	}
}

func TestConcurrentCallsIndependent(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	jp := &fakeBackend{name: "jaypost", tools: []*mcp.Tool{tool("slow", nil), tool("fast", nil)}}
	jp.callFn = func(ctx context.Context, tl string, _ json.RawMessage, _ func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
		if tl == "slow" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: tl}}}, nil
	}
	g := New(Options{Backends: []Backend{jp}})
	cs := connect(t, g, nil)

	aDone := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jaypost__slow", Arguments: map[string]any{}})
		aDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow call never started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "jaypost__fast", Arguments: map[string]any{}})
	if err != nil || resultText(res) != "fast" {
		t.Fatalf("fast call: %v %+v", err, res)
	}
	select {
	case err := <-aDone:
		t.Fatalf("slow call finished early: %v", err)
	default:
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("slow call: %v", err)
	}
}

func TestCancellationReachesBackend(t *testing.T) {
	jp := &fakeBackend{name: "jaypost", tools: []*mcp.Tool{tool("slow", nil)}}
	cancelled := make(chan struct{})
	started := make(chan struct{})
	jp.callFn = func(ctx context.Context, _ string, _ json.RawMessage, _ func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}
	g := New(Options{Backends: []Backend{jp}})
	cs := connect(t, g, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "jaypost__slow", Arguments: map[string]any{}})
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("backend ctx not cancelled")
	}
}

func TestAuditLog(t *testing.T) {
	jp, ot := twoBackends()
	log, buf := newLogger()
	g := New(Options{Backends: []Backend{jp, ot}, Logger: log})
	h := g.handler(ot, "echo", "other__echo")
	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "other__echo", Arguments: json.RawMessage(`{"msg":"supersecretvalue"}`)},
		Extra:  &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: "alice"}},
	}
	res, err := h(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("handler: %v %+v", err, res)
	}
	rec := findCallRecord(t, buf.String())
	if rec["user"] != "alice" || rec["tool"] != "other__echo" || rec["ok"] != true {
		t.Fatalf("record = %v", rec)
	}
	if _, ok := rec["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms missing: %v", rec)
	}
	if strings.Contains(buf.String(), "supersecretvalue") {
		t.Fatalf("argument value logged:\n%s", buf.String())
	}
}

func TestAuditLogAnonymousAndFailure(t *testing.T) {
	jp, ot := twoBackends()
	ot.callFn = func(context.Context, string, json.RawMessage, func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
		return nil, upstream.ErrUnavailable
	}
	log, buf := newLogger()
	g := New(Options{Backends: []Backend{jp, ot}, Logger: log})
	h := g.handler(ot, "echo", "other__echo")
	res, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "other__echo"}})
	if err != nil || !res.IsError {
		t.Fatalf("handler: %v %+v", err, res)
	}
	rec := findCallRecord(t, buf.String())
	if rec["user"] != "-" || rec["ok"] != false {
		t.Fatalf("record = %v", rec)
	}
}

func findCallRecord(t *testing.T, logs string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if _, ok := m["duration_ms"]; ok {
			return m
		}
	}
	t.Fatalf("no tool call record in logs:\n%s", logs)
	return nil
}

func TestDenyArgsUnmatchedWarns(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"attachment": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{}}},
	}}
	jp := &fakeBackend{name: "jaypost"}
	log, buf := newLogger()
	g := New(Options{
		Backends: []Backend{jp},
		Policies: map[string]ArgPolicy{"jaypost": NewArgPolicy([]string{"attachment.path", "attachment.pth"})},
		Logger:   log,
	})
	const msg = "deny_args path matches no tool parameter"
	if strings.Contains(buf.String(), msg) {
		t.Fatalf("warned while upstream has no tools:\n%s", buf)
	}
	jp.setTools([]*mcp.Tool{tool("ask_user", schema)})
	g.Sync()
	g.Sync()
	logs := buf.String()
	if n := strings.Count(logs, msg); n != 1 {
		t.Fatalf("got %d warnings, want 1:\n%s", n, logs)
	}
	var rec map[string]any
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, msg) {
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rec["level"] != "WARN" || rec["upstream"] != "jaypost" || rec["path"] != "attachment.pth" {
		t.Fatalf("record = %v", rec)
	}
}
