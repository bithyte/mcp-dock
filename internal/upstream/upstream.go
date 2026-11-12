// Package upstream supervises one streamable HTTP MCP upstream (usually an
// mcp-runner): it connects, caches the tool list, forwards calls, and
// reconnects with backoff when the session is lost or the connect fails. A
// broken upstream never stops Run; it just stays down and is retried.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrUnavailable is returned by Call while the upstream is down.
var ErrUnavailable = errors.New("upstream unavailable")

const (
	defaultMinBackoff = time.Second
	defaultMaxBackoff = 60 * time.Second
	// stableUptime is how long a session must stay up before the backoff
	// resets to MinBackoff.
	stableUptime = 60 * time.Second

	connectTimeout   = 30 * time.Second
	listToolsTimeout = 30 * time.Second
	// closeGrace is how long a graceful session close (the DELETE to the
	// upstream) may take before Run gives up on it.
	closeGrace = 2 * time.Second
)

type Options struct {
	Name string
	URL  string // streamable HTTP endpoint, e.g. http://jaypost:8080/mcp
	// BearerToken, if non-empty, is sent as Authorization: Bearer on every
	// request.
	BearerToken string
	// HTTPClient is the base client. Nil means a client with no overall
	// timeout, since tool calls can run for a long time.
	HTTPClient     *http.Client
	Version        string // client implementation version sent on initialize
	Logger         *slog.Logger
	OnToolsChanged func() // called after tool cache changes (up, down, list_changed)
	MinBackoff     time.Duration
	MaxBackoff     time.Duration
}

// Upstream is safe for concurrent use. Calls are not serialized: each one is
// its own HTTP request on the shared session.
type Upstream struct {
	opts Options
	log  *slog.Logger
	http *http.Client // opts.HTTPClient wrapped with the header transport

	mu      sync.RWMutex
	session *mcp.ClientSession // nil while down
	dead    chan struct{}      // closed when session's connection ends
	live    context.Context    // canceled when the session is torn down
	tools   []*mcp.Tool        // nil while down

	progress sync.Map // progress token (string) -> func(*mcp.ProgressNotificationParams)
	tokenSeq atomic.Uint64
}

// headerTransport adds the upstream's auth header to every request.
type headerTransport struct {
	base   http.RoundTripper
	bearer string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.bearer != "" {
		// A RoundTripper must not modify the caller's request.
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.bearer)
	}
	return t.base.RoundTrip(req)
}

// wrapClient returns a copy of base (or a default client) whose transport
// adds the upstream's headers.
func wrapClient(base *http.Client, bearer string) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = &headerTransport{base: rt, bearer: bearer}
	return c
}

func New(opts Options) *Upstream {
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = defaultMinBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = defaultMaxBackoff
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = opts.MinBackoff
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	return &Upstream{
		opts: opts,
		log:  log.With("upstream", opts.Name),
		http: wrapClient(opts.HTTPClient, opts.BearerToken),
	}
}

func (u *Upstream) Name() string { return u.opts.Name }

// Tools returns the cached tool list, or nil while the upstream is down.
// The returned slice is a copy; the *mcp.Tool values are shared and must
// not be modified.
func (u *Upstream) Tools() []*mcp.Tool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return slices.Clone(u.tools)
}

// Call invokes tool on the upstream. If onProgress is non-nil, progress
// notifications for this call are passed to it; it runs on the session's
// receive loop and must not block. Returns ErrUnavailable while down, and
// ctx.Err() if ctx ends first (the SDK sends notifications/cancelled).
func (u *Upstream) Call(ctx context.Context, tool string, args json.RawMessage,
	onProgress func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
	u.mu.RLock()
	s, dead, live := u.session, u.dead, u.live
	u.mu.RUnlock()
	if s == nil {
		return nil, ErrUnavailable
	}
	// End the request when the session is torn down, so shutdown doesn't
	// wait on a call whose ctx never ends.
	cctx, stop := context.WithCancel(ctx)
	defer stop()
	defer context.AfterFunc(live, stop)()
	params := &mcp.CallToolParams{Name: tool}
	if len(args) > 0 {
		params.Arguments = args
	}
	if onProgress != nil {
		token := fmt.Sprintf("%s-%d", u.opts.Name, u.tokenSeq.Add(1))
		params.SetProgressToken(token)
		u.progress.Store(token, onProgress)
		defer u.progress.Delete(token)
	}
	res, err := s.CallTool(cctx, params)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if live.Err() != nil || sessionLost(err, dead) {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return nil, err
	}
	return res, nil
}

// sessionLost reports whether a failed call failed because the session with
// the upstream went away (404 for the session id, connection closed), as
// opposed to the upstream answering with an error. Once the client conn has
// failed, the SDK retires calls with that failure, which may be any of these
// or a plain transport error, so check the session's liveness too.
func sessionLost(err error, dead <-chan struct{}) bool {
	if errors.Is(err, mcp.ErrSessionMissing) {
		return true
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		return false // a real response from the upstream
	}
	if errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	select {
	case <-dead:
		return true
	default:
		return false
	}
}

// Run supervises the upstream until ctx is done. It never returns early:
// start failures and exits are logged and retried with backoff.
func (u *Upstream) Run(ctx context.Context) {
	backoff := u.opts.MinBackoff
	for {
		uptime, err := u.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		var wait time.Duration
		wait, backoff = nextBackoff(backoff, uptime, u.opts.MinBackoff, u.opts.MaxBackoff)
		attrs := []any{"retry_in_ms", wait.Milliseconds()}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		if uptime > 0 {
			u.log.Warn("upstream exited", append(attrs, "uptime_ms", uptime.Milliseconds())...)
		} else {
			u.log.Warn("upstream start failed", attrs...)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// nextBackoff returns how long to wait before the next connect and the
// backoff to use after that. A session that stayed up for stableUptime resets the
// backoff to lo.
func nextBackoff(cur, uptime, lo, hi time.Duration) (wait, next time.Duration) {
	if uptime >= stableUptime {
		cur = lo
	}
	return cur, min(cur*2, hi)
}

// runOnce connects, serves the session until it ends or ctx is done, and
// reports how long it was up (0 if it never connected).
func (u *Upstream) runOnce(ctx context.Context) (time.Duration, error) {
	refresh := make(chan struct{}, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-dock", Version: u.opts.Version}, &mcp.ClientOptions{
		Logger: u.log,
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			// Notifications run on the receive loop; refetch elsewhere.
			select {
			case refresh <- struct{}{}:
			default:
			}
		},
		ProgressNotificationHandler: u.handleProgress,
	})

	transport := &mcp.StreamableClientTransport{Endpoint: u.opts.URL, HTTPClient: u.http}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	session, err := client.Connect(cctx, transport, nil)
	cancel()
	if err != nil {
		return 0, err
	}
	started := time.Now()

	tools, err := u.fetchTools(ctx, session)
	if err != nil {
		u.closeSession(session)
		return time.Since(started), fmt.Errorf("list tools: %w", err)
	}
	// The session ends (Wait returns) when the client conn fails: a 404 for
	// the session id, or the standalone GET stream failing to reconnect.
	dead := make(chan struct{})
	exited := make(chan error, 1)
	go func() {
		err := session.Wait()
		close(dead)
		exited <- err
	}()
	live, kill := context.WithCancel(context.Background())
	u.mu.Lock()
	u.session, u.tools, u.dead, u.live = session, tools, dead, live
	u.mu.Unlock()
	u.log.Info("upstream started", "tools", len(tools))
	u.notify()

	rctx, rcancel := context.WithCancel(ctx)
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		u.refreshLoop(rctx, session, refresh)
	}()

	select {
	case <-ctx.Done():
		err = nil
	case err = <-exited:
	}
	uptime := time.Since(started)
	rcancel()

	// Mark down before closing so new calls fail fast with ErrUnavailable,
	// then end in-flight calls so Close doesn't wait on them.
	u.mu.Lock()
	u.session, u.tools, u.dead, u.live = nil, nil, nil, nil
	u.mu.Unlock()
	kill()
	u.notify()

	u.closeSession(session)
	<-refreshDone
	return uptime, err
}

// closeSession closes the session, which DELETEs it on the upstream. If that
// takes longer than closeGrace (an upstream that accepts the connection but
// never answers), give up; the SDK bounds the DELETE itself and cancels its
// remaining requests when it returns.
func (u *Upstream) closeSession(s *mcp.ClientSession) {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = s.Close()
	}()
	select {
	case <-closed:
	case <-time.After(closeGrace):
		u.log.Warn("upstream close timed out")
	}
}

func (u *Upstream) refreshLoop(ctx context.Context, s *mcp.ClientSession, refresh <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh:
		}
		tools, err := u.fetchTools(ctx, s)
		if err != nil {
			if ctx.Err() == nil {
				u.log.Warn("refresh tools failed", "error", err.Error())
			}
			continue
		}
		u.mu.Lock()
		if u.session != s {
			u.mu.Unlock()
			return
		}
		u.tools = tools
		u.mu.Unlock()
		u.log.Info("upstream tools changed", "tools", len(tools))
		u.notify()
	}
}

func (u *Upstream) fetchTools(ctx context.Context, s *mcp.ClientSession) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, listToolsTimeout)
	defer cancel()
	tools := []*mcp.Tool{} // non-nil: an up upstream with no tools is not "down"
	for t, err := range s.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func (u *Upstream) handleProgress(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	token, ok := req.Params.ProgressToken.(string)
	if !ok {
		return
	}
	if fn, ok := u.progress.Load(token); ok {
		fn.(func(*mcp.ProgressNotificationParams))(req.Params)
	}
}

func (u *Upstream) notify() {
	if u.opts.OnToolsChanged != nil {
		u.opts.OnToolsChanged()
	}
}
