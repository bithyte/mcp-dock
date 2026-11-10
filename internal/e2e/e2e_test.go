// Package e2e runs the whole gateway (oauth, gateway, a real stdio upstream)
// behind the real HTTP handler and drives it with the SDK client.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bithyte/mcp-dock/internal/app"
	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testutil.Cleanup()
	os.Exit(code)
}

const (
	ghClientID     = "gh-cid"
	ghClientSecret = "gh-secret"
	clientRedirect = "http://127.0.0.1:33333/callback"
)

// logBuffer is a goroutine-safe log sink.
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

// records returns every JSON log record with the given msg.
func (b *logBuffer) records(msg string) []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

type env struct {
	ts   *httptest.Server
	cfg  *config.Config
	fake *testutil.FakeGitHub
	logs *logBuffer
	app  *app.App
}

// newEnv starts the full gateway with the fake upstream as "fake"
// (deny_args [attachment.path]) plus any extra upstreams. Allowlist:
// cozmin, friend.
func newEnv(t *testing.T, extra ...config.UpstreamConfig) *env {
	t.Helper()
	bin := testutil.BuildFakeUpstream(t)
	e := &env{logs: &logBuffer{}}
	e.fake = testutil.NewFakeGitHub(t, ghClientID, ghClientSecret)

	// public_url must be known before the handler is built, so start the
	// listener first and attach the handler afterwards.
	e.ts = httptest.NewUnstartedServer(nil)
	publicURL := "http://" + e.ts.Listener.Addr().String()

	e.cfg = &config.Config{
		PublicURL:    publicURL,
		DataDir:      t.TempDir(),
		GitHub:       config.GitHubConfig{ClientID: ghClientID, ClientSecret: ghClientSecret},
		AllowedUsers: []string{"cozmin", "friend"},
		Upstreams: append([]config.UpstreamConfig{{
			Name:     "fake",
			Command:  bin,
			DenyArgs: []string{"attachment.path"},
		}}, extra...),
	}
	log := slog.New(slog.NewJSONHandler(e.logs, nil))

	st, err := store.Open(filepath.Join(e.cfg.DataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}

	getenv := func(k string) string {
		if k == "PATH" {
			return os.Getenv("PATH")
		}
		return ""
	}
	e.app = app.Build(e.cfg, getenv, log, st, app.Options{
		Version:          "test",
		GitHubWebBaseURL: e.fake.URL,
		GitHubAPIBaseURL: e.fake.URL,
		MinBackoff:       50 * time.Millisecond,
		MaxBackoff:       200 * time.Millisecond,
	})
	e.ts.Config.Handler = e.app.Handler
	e.ts.Start()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { e.app.RunUpstreams(ctx) })
	t.Cleanup(func() {
		e.ts.CloseClientConnections()
		e.ts.Close()
		cancel()
		wg.Wait()
		st.Close()
	})
	return e
}

// --- OAuth flow over HTTP, as a browser plus an MCP client would do it ---

var requestField = regexp.MustCompile(`name="request" value="([^"]+)"`)

func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// reader lets an (*http.Response, error) call be passed straight in:
// rd.read(http.Get(u)).
type reader struct{ t *testing.T }

func (rd reader) read(resp *http.Response, err error) (*http.Response, string) {
	t := rd.t
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func expect(t *testing.T, step string, resp *http.Response, body string, status int) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("%s: status %d, want %d: %s", step, resp.StatusCode, status, body)
	}
}

// login runs register, authorize, GitHub, callback, consent and token for
// a GitHub user and returns the access token.
func (e *env) login(t *testing.T, ghLogin string, ghID int64) string {
	t.Helper()
	rd := reader{t}
	reg, _ := json.Marshal(map[string]any{
		"client_name":                "e2e",
		"redirect_uris":              []string{clientRedirect},
		"token_endpoint_auth_method": "none",
	})
	resp, body := rd.read(http.Post(e.ts.URL+"/register", "application/json", bytes.NewReader(reg)))
	expect(t, "register", resp, body, http.StatusCreated)
	var regResp struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal([]byte(body), &regResp); err != nil || regResp.ClientID == "" {
		t.Fatalf("register response %s: %v", body, err)
	}

	vb := make([]byte, 32)
	rand.Read(vb)
	verifier := base64.RawURLEncoding.EncodeToString(vb)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {regResp.ClientID},
		"redirect_uri":          {clientRedirect},
		"state":                 {"st"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"resource":              {e.cfg.ResourceURL()},
	}

	e.fake.SetUser(ghLogin, ghID)
	b := newBrowser(t)
	resp, body = rd.read(b.Get(e.ts.URL + "/authorize?" + q.Encode()))
	expect(t, "authorize", resp, body, http.StatusFound)
	resp, body = rd.read(b.Get(resp.Header.Get("Location"))) // fake GitHub
	expect(t, "github", resp, body, http.StatusFound)
	resp, body = rd.read(b.Get(resp.Header.Get("Location"))) // our callback
	expect(t, "callback", resp, body, http.StatusOK)
	m := requestField.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no consent form: %s", body)
	}
	resp, body = rd.read(b.PostForm(e.ts.URL+"/oauth/consent", url.Values{"request": {m[1]}, "action": {"approve"}}))
	expect(t, "consent", resp, body, http.StatusFound)
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), clientRedirect) || loc.Query().Get("code") == "" {
		t.Fatalf("consent Location %q", resp.Header.Get("Location"))
	}

	resp, body = rd.read(http.PostForm(e.ts.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {regResp.ClientID},
		"code":          {loc.Query().Get("code")},
		"redirect_uri":  {clientRedirect},
		"code_verifier": {verifier},
		"resource":      {e.cfg.ResourceURL()},
	}))
	expect(t, "token", resp, body, http.StatusOK)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(body), &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("token response %s: %v", body, err)
	}
	return tok.AccessToken
}

// --- MCP client ---

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

type progressCounter struct{ n atomic.Int64 }

// connect opens an MCP session with token. Progress notifications are
// counted in pc if non-nil.
func (e *env) connect(t *testing.T, token string, pc *progressCounter) *mcp.ClientSession {
	t.Helper()
	opts := &mcp.ClientOptions{}
	if pc != nil {
		opts.ProgressNotificationHandler = func(context.Context, *mcp.ProgressNotificationClientRequest) { pc.n.Add(1) }
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, opts)
	tr := &mcp.StreamableClientTransport{
		Endpoint:   e.cfg.ResourceURL(),
		HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}},
		MaxRetries: -1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := c.Connect(ctx, tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// waitTools polls ListTools until all want names are present.
func waitTools(t *testing.T, cs *mcp.ClientSession, want ...string) []string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		names := toolNames(t, cs)
		ok := true
		for _, w := range want {
			ok = ok && slices.Contains(names, w)
		}
		if ok {
			return names
		}
		if time.Now().After(deadline) {
			t.Fatalf("tools %v, want %v", names, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "")
}

// --- tests ---

func TestEndToEnd(t *testing.T) {
	e := newEnv(t)
	token := e.login(t, "Cozmin", 1)
	cs := e.connect(t, token, nil)

	waitTools(t, cs, "fake__echo", "fake__notify")

	res := call(t, cs, "fake__echo", map[string]any{"text": "hi"})
	if res.IsError || resultText(res) != "hi" {
		t.Fatalf("echo: isError=%v %q", res.IsError, resultText(res))
	}

	// deny_args from the config reach the gateway through app.Build: the
	// call is refused by the gateway (the fake upstream would accept it)
	// and the path is gone from the advertised schema.
	res = call(t, cs, "fake__notify", map[string]any{"text": "x", "attachment": map[string]any{"path": "/etc/passwd"}})
	if !res.IsError || !strings.Contains(resultText(res), "not allowed by gateway policy") {
		t.Fatalf("notify with attachment.path: want policy error, got isError=%v %q", res.IsError, resultText(res))
	}
	res = call(t, cs, "fake__notify", map[string]any{"text": "x", "attachment": []any{map[string]any{"path": "/etc/passwd"}}})
	if !res.IsError || !strings.Contains(resultText(res), "not allowed by gateway policy") {
		t.Fatalf("notify with attachment[].path: want policy error, got isError=%v %q", res.IsError, resultText(res))
	}
	if res := call(t, cs, "fake__notify", map[string]any{"text": "x"}); res.IsError {
		t.Fatalf("notify without attachment: %q", resultText(res))
	}
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range lt.Tools {
		if tl.Name != "fake__notify" {
			continue
		}
		b, _ := json.Marshal(tl.InputSchema)
		if !strings.Contains(string(b), `"attachment"`) || strings.Contains(string(b), `"path"`) {
			t.Fatalf("fake__notify schema not stripped: %s", b)
		}
	}

	// The audit log names the authenticated user, from TokenInfo.
	var echoLogged bool
	for _, r := range e.logs.records("tool call") {
		if r["tool"] == "fake__echo" {
			echoLogged = true
			if r["user"] != "cozmin" || r["ok"] != true {
				t.Errorf("audit record %v, want user cozmin ok true", r)
			}
		}
	}
	if !echoLogged {
		t.Fatalf("no audit record for fake__echo in logs:\n%s", e.logs)
	}
	if strings.Contains(e.logs.String(), token) {
		t.Fatal("access token appears in logs")
	}
}

func TestEndToEndProgress(t *testing.T) {
	e := newEnv(t)
	pc := &progressCounter{}
	cs := e.connect(t, e.login(t, "cozmin", 1), pc)
	waitTools(t, cs, "fake__sleep")

	params := &mcp.CallToolParams{Name: "fake__sleep", Arguments: map[string]any{"ms": 300}}
	params.SetProgressToken("p1")
	res, err := cs.CallTool(context.Background(), params)
	if err != nil || res.IsError {
		t.Fatalf("sleep: %v %v", err, res)
	}
	if n := pc.n.Load(); n < 2 {
		t.Fatalf("got %d progress notifications, want >= 2", n)
	}
}

func TestEndToEndTwoUsers(t *testing.T) {
	e := newEnv(t)
	friendPC := &progressCounter{}
	cozmin := e.connect(t, e.login(t, "cozmin", 1), nil)
	friend := e.connect(t, e.login(t, "friend", 2), friendPC)
	waitTools(t, cozmin, "fake__echo", "fake__sleep")

	done := make(chan error, 1)
	go func() {
		params := &mcp.CallToolParams{Name: "fake__sleep", Arguments: map[string]any{"ms": 2000}}
		params.SetProgressToken("friend")
		_, err := friend.CallTool(context.Background(), params)
		done <- err
	}()
	// Wait until friend's call is running in the upstream.
	deadline := time.Now().Add(5 * time.Second)
	for friendPC.n.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("friend's sleep never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	res := call(t, cozmin, "fake__echo", map[string]any{"text": "fast"})
	if d := time.Since(start); d >= 500*time.Millisecond {
		t.Fatalf("cozmin's echo took %v while friend's sleep ran", d)
	}
	if resultText(res) != "fast" {
		t.Fatalf("echo %q", resultText(res))
	}
	select {
	case err := <-done:
		t.Fatalf("friend's sleep already finished, so the timing check proves nothing: %v", err)
	default:
	}
	if err := <-done; err != nil {
		t.Fatalf("friend's sleep: %v", err)
	}

	users := map[string]bool{}
	for _, r := range e.logs.records("tool call") {
		users[r["user"].(string)] = true
	}
	if !users["cozmin"] || !users["friend"] {
		t.Fatalf("audit users %v, want cozmin and friend", users)
	}
}

func TestEndToEndRemovedUser(t *testing.T) {
	e := newEnv(t)
	token := e.login(t, "cozmin", 1)
	cs := e.connect(t, token, nil)
	waitTools(t, cs, "fake__echo")
	call(t, cs, "fake__echo", map[string]any{"text": "before"})

	e.cfg.AllowedUsers = []string{"friend"}

	req, _ := http.NewRequest(http.MethodPost, e.cfg.ResourceURL(), strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, body := reader{t}.read(http.DefaultClient.Do(req))
	expect(t, "initialize after removal", resp, body, http.StatusUnauthorized)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "fake__echo", Arguments: map[string]any{"text": "after"}}); err == nil {
		t.Fatal("existing session still works after removal from allowlist")
	}
}

// A broken upstream (command missing) must not stop the gateway serving
// the others.
func TestEndToEndMissingUpstream(t *testing.T) {
	e := newEnv(t, config.UpstreamConfig{Name: "broken", Command: "/nonexistent"})
	cs := e.connect(t, e.login(t, "cozmin", 1), nil)
	names := waitTools(t, cs, "fake__echo")
	for _, n := range names {
		if strings.HasPrefix(n, "broken__") {
			t.Fatalf("tool %q from a broken upstream", n)
		}
	}
	if res := call(t, cs, "fake__echo", map[string]any{"text": "hi"}); resultText(res) != "hi" {
		t.Fatalf("echo %q", resultText(res))
	}
	var failed bool
	for _, r := range e.logs.records("upstream start failed") {
		failed = failed || r["upstream"] == "broken"
	}
	if !failed {
		t.Fatalf("no start failure logged for broken:\n%s", e.logs)
	}
}

// An upstream that comes up after clients connected must show up in their
// tool list. Only OnToolsChanged (wired to gw.Sync in app.Build) makes that
// happen; nothing else re-syncs the gateway.
func TestEndToEndLateUpstream(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := testutil.BuildFakeUpstream(t)
	e := newEnv(t, config.UpstreamConfig{Name: "late", Command: bin, Env: map[string]string{"FAKE_FAIL_START": gate}})

	changed := make(chan struct{}, 100)
	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case changed <- struct{}{}:
			default:
			}
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   e.cfg.ResourceURL(),
		HTTPClient: &http.Client{Transport: bearer{e.login(t, "cozmin", 1), http.DefaultTransport}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	waitTools(t, cs, "fake__echo")
	for _, n := range toolNames(t, cs) {
		if strings.HasPrefix(n, "late__") {
			t.Fatalf("late upstream tool %q before it started", n)
		}
	}
	for len(changed) > 0 {
		<-changed
	}
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	waitTools(t, cs, "late__echo")
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed after the late upstream came up")
	}
}

// An MCP session is bound to the user who opened it: another user's token
// with that session's ID is refused by the SDK.
func TestEndToEndSessionBoundToUser(t *testing.T) {
	e := newEnv(t)
	cozminToken := e.login(t, "cozmin", 1)
	cozmin := e.connect(t, cozminToken, nil)
	waitTools(t, cozmin, "fake__echo")
	sid := cozmin.ID()
	if sid == "" {
		t.Fatal("no session ID")
	}
	friendToken := e.login(t, "friend", 2)

	post := func(token, body string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, e.cfg.ResourceURL(), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		req.Header.Set("Mcp-Session-Id", sid)
		req.Header.Set("Authorization", "Bearer "+token)
		return reader{t}.read(http.DefaultClient.Do(req))
	}
	list := `{"jsonrpc":"2.0","id":99,"method":"tools/list","params":{}}`
	echo := `{"jsonrpc":"2.0","id":100,"method":"tools/call","params":{"name":"fake__echo","arguments":{"text":"hijack"}}}`
	// Control: the same raw request with the owner's token is accepted, so
	// the 403 below is about the user, not the request shape.
	resp, rb := post(cozminToken, list)
	expect(t, "cozmin using own session", resp, rb, http.StatusOK)
	if !strings.Contains(rb, "fake__echo") {
		t.Fatalf("control tools/list: %s", rb)
	}
	for _, body := range []string{list, echo} {
		resp, rb := post(friendToken, body)
		expect(t, "friend using cozmin's session", resp, rb, http.StatusForbidden)
		if strings.Contains(rb, "fake__echo") || strings.Contains(rb, "hijack") {
			t.Fatalf("response leaks session data: %s", rb)
		}
	}
	for _, r := range e.logs.records("tool call") {
		if r["user"] == "friend" {
			t.Fatalf("friend's call reached the gateway: %v", r)
		}
	}
	// The owner's session still works.
	if res := call(t, cozmin, "fake__echo", map[string]any{"text": "still mine"}); resultText(res) != "still mine" {
		t.Fatalf("echo %q", resultText(res))
	}
}
