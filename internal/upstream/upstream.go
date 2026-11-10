// Package upstream supervises one stdio MCP server process: it starts it,
// caches its tool list, forwards calls, and restarts it with backoff when it
// exits or fails to start. A broken upstream never stops Run; it just stays
// down and is retried.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
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
	// stableUptime is how long a process must stay up before the backoff
	// resets to MinBackoff.
	stableUptime = 60 * time.Second

	connectTimeout   = 30 * time.Second
	listToolsTimeout = 30 * time.Second
	// terminateDuration is how long Close waits after closing stdin before
	// SIGTERM, and again before SIGKILL.
	terminateDuration = 2 * time.Second
	// closeGrace is how long a graceful session close may take on shutdown
	// before the process is killed. The SDK's Close waits for in-flight
	// calls, so without this a call whose ctx never ends blocks Run.
	closeGrace = 2 * time.Second
	// waitDelay bounds how long we wait for the stderr pipe to drain after
	// the process exits (e.g. a grandchild still holds it open).
	waitDelay = 2 * time.Second
)

type Options struct {
	Name           string
	Command        string
	Args           []string
	Env            []string // from UpstreamConfig.Environ; the process gets only this
	WorkDir        string   // created if missing
	Logger         *slog.Logger
	OnToolsChanged func() // called after tool cache changes (up, down, list_changed)
	MinBackoff     time.Duration
	MaxBackoff     time.Duration
}

// Upstream is safe for concurrent use. Calls are not serialized: the SDK
// session multiplexes concurrent requests over the one stdio connection.
type Upstream struct {
	opts Options
	log  *slog.Logger

	mu      sync.RWMutex
	session *mcp.ClientSession // nil while down
	dead    chan struct{}      // closed when session's connection ends
	tools   []*mcp.Tool        // nil while down

	progress sync.Map // progress token (string) -> func(*mcp.ProgressNotificationParams)
	tokenSeq atomic.Uint64
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
	return &Upstream{opts: opts, log: log.With("upstream", opts.Name)}
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
	s, dead := u.session, u.dead
	u.mu.RUnlock()
	if s == nil {
		return nil, ErrUnavailable
	}
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
	res, err := s.CallTool(ctx, params)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if sessionLost(err, dead) {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return nil, err
	}
	return res, nil
}

// sessionLost reports whether a failed call failed because the connection to
// the upstream went away (process died, pipe broke, session closed), as
// opposed to the upstream answering with an error. When the process dies
// mid-call the SDK retires the call with the read error (io.EOF), not
// mcp.ErrConnectionClosed, so check transport errors and the session's
// liveness too.
func sessionLost(err error, dead <-chan struct{}) bool {
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

// nextBackoff returns how long to wait before the next start and the backoff
// to use after that. A process that stayed up for stableUptime resets the
// backoff to lo.
func nextBackoff(cur, uptime, lo, hi time.Duration) (wait, next time.Duration) {
	if uptime >= stableUptime {
		cur = lo
	}
	return cur, min(cur*2, hi)
}

// runOnce starts the process, serves it until it exits or ctx is done, and
// reports how long it was up (0 if it never connected).
func (u *Upstream) runOnce(ctx context.Context) (time.Duration, error) {
	if u.opts.WorkDir != "" {
		if err := os.MkdirAll(u.opts.WorkDir, 0o700); err != nil {
			return 0, fmt.Errorf("create workdir: %w", err)
		}
	}
	stderr := newLineLogger(u.log)
	defer stderr.Flush()

	cmd := exec.Command(u.opts.Command, u.opts.Args...)
	// Never nil: a nil Env would inherit the gateway's environment.
	cmd.Env = append([]string{}, u.opts.Env...)
	cmd.Dir = u.opts.WorkDir
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	refresh := make(chan struct{}, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-dock", Version: "0.1.0"}, &mcp.ClientOptions{
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

	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	session, err := client.Connect(cctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: terminateDuration}, nil)
	cancel()
	if err != nil {
		if cmd.Process != nil {
			// Connect normally closes the session (and reaps the process) on
			// failure; kill anyway so a hung child can't linger.
			_ = cmd.Process.Kill()
		}
		return 0, err
	}
	started := time.Now()

	tools, err := u.fetchTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return time.Since(started), fmt.Errorf("list tools: %w", err)
	}
	dead := make(chan struct{})
	exited := make(chan error, 1)
	go func() {
		err := session.Wait()
		close(dead)
		exited <- err
	}()
	u.mu.Lock()
	u.session, u.tools, u.dead = session, tools, dead
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

	// Mark down before closing so new calls fail fast with ErrUnavailable.
	u.mu.Lock()
	u.session, u.tools, u.dead = nil, nil, nil
	u.mu.Unlock()
	u.notify()

	u.closeSession(session, cmd)
	<-refreshDone
	return uptime, err
}

// closeSession closes the session (stdin, then SIGTERM/SIGKILL, reaping the
// process). If in-flight calls keep it open past closeGrace, the process is
// killed, which fails those calls and lets Close finish.
func (u *Upstream) closeSession(s *mcp.ClientSession, cmd *exec.Cmd) {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = s.Close()
	}()
	select {
	case <-closed:
		return
	case <-time.After(closeGrace):
	}
	u.log.Warn("upstream close timed out; killing")
	_ = cmd.Process.Kill()
	<-closed
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
