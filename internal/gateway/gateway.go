package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bithyte/mcp-dock/internal/upstream"
)

// Backend is one upstream MCP server as seen by the gateway.
// *upstream.Upstream implements it.
type Backend interface {
	Name() string
	// Tools returns the upstream's current tools, or nil while it is down.
	// The returned values must not be modified.
	Tools() []*mcp.Tool
	// Call invokes tool. onProgress, if non-nil, runs on the upstream
	// session's receive loop and must not block.
	Call(ctx context.Context, tool string, args json.RawMessage,
		onProgress func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error)
}

var _ Backend = (*upstream.Upstream)(nil)

type Options struct {
	Backends []Backend
	Policies map[string]ArgPolicy // keyed by backend name
	Logger   *slog.Logger
	Version  string
}

// Gateway exposes the merged tools of all backends on one MCP server.
type Gateway struct {
	backends []Backend
	policies map[string]ArgPolicy
	log      *slog.Logger
	srv      *mcp.Server

	mu        sync.Mutex        // serializes Sync
	current   map[string]string // exposed name -> fingerprint of what's registered
	unmatched map[string]string // backend name -> unmatched deny paths last warned about
}

// progressBuffer is how many progress notifications per call may wait for
// a slow client before the oldest is dropped.
const progressBuffer = 16

func New(opts Options) *Gateway {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	g := &Gateway{
		backends:  slices.Clone(opts.Backends),
		policies:  opts.Policies,
		log:       log,
		current:   map[string]string{},
		unmatched: map[string]string{},
	}
	g.srv = mcp.NewServer(&mcp.Implementation{Name: "mcp-dock", Version: opts.Version}, &mcp.ServerOptions{
		Logger: log,
		// Advertise tools.listChanged even with zero tools, so clients
		// subscribe and learn about upstreams that come up later.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
	})
	g.Sync()
	return g
}

// Server returns the single shared MCP server.
func (g *Gateway) Server() *mcp.Server { return g.srv }

type exposed struct {
	backend Backend
	tool    string // upstream tool name
	def     *mcp.Tool
	fp      string
}

// Sync recomputes the merged tool list from the backends and updates the
// server. The SDK sends notifications/tools/list_changed for any change.
// Safe to call concurrently; wire it as the upstreams' OnToolsChanged.
func (g *Gateway) Sync() {
	g.mu.Lock()
	defer g.mu.Unlock()

	desired := map[string]exposed{}
	var order []string
	for _, b := range g.backends {
		pol := g.policies[b.Name()]
		tools := b.Tools()
		g.warnUnmatched(b.Name(), pol, tools)
		for _, t := range tools {
			if t == nil {
				continue
			}
			name := ToolName(b.Name(), t.Name)
			if !ValidToolName(name) {
				g.log.Warn("skipping tool: exposed name not allowed", "upstream", b.Name(), "tool", name)
				continue
			}
			if _, dup := desired[name]; dup {
				g.log.Warn("skipping tool: duplicate exposed name", "upstream", b.Name(), "tool", name)
				continue
			}
			def, err := exposeTool(t, name, pol)
			if err != nil {
				g.log.Warn("skipping tool: unusable definition", "upstream", b.Name(), "tool", name, "error", err)
				continue
			}
			raw, err := json.Marshal(def)
			if err != nil {
				g.log.Warn("skipping tool: unusable definition", "upstream", b.Name(), "tool", name, "error", err)
				continue
			}
			desired[name] = exposed{backend: b, tool: t.Name, def: def, fp: b.Name() + "\x00" + string(raw)}
			order = append(order, name)
		}
	}

	var stale []string
	for name := range g.current {
		if _, ok := desired[name]; !ok {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		g.srv.RemoveTools(stale...)
		for _, name := range stale {
			delete(g.current, name)
		}
	}
	for _, name := range order {
		e := desired[name]
		if g.current[name] == e.fp {
			continue
		}
		if err := g.addTool(e.def, g.handler(e.backend, e.tool, name)); err != nil {
			g.log.Warn("skipping tool: rejected by server", "upstream", e.backend.Name(), "tool", name, "error", err)
			if _, had := g.current[name]; had {
				g.srv.RemoveTools(name)
				delete(g.current, name)
			}
			continue
		}
		g.current[name] = e.fp
	}
}

// warnUnmatched logs deny_args paths that name no parameter of any of the
// backend's tools, which usually means a typo in the config. It stays quiet
// while the backend is down, and only logs when the set of unmatched paths
// changes, so repeated Syncs with the same tools don't repeat the warning.
func (g *Gateway) warnUnmatched(name string, pol ArgPolicy, tools []*mcp.Tool) {
	if len(tools) == 0 {
		return
	}
	schemas := make([]any, 0, len(tools))
	for _, t := range tools {
		if t != nil {
			schemas = append(schemas, t.InputSchema)
		}
	}
	un := pol.Unmatched(schemas)
	key := strings.Join(un, "\x00")
	if key == g.unmatched[name] {
		return
	}
	g.unmatched[name] = key
	for _, path := range un {
		g.log.Warn("deny_args path matches no tool parameter", "upstream", name, "path", path)
	}
}

// addTool registers t, turning the SDK's panics on bad definitions into
// errors. Upstream definitions are untrusted; one bad tool must not take
// the gateway down.
func (g *Gateway) addTool(t *mcp.Tool, h mcp.ToolHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	g.srv.AddTool(t, h)
	return nil
}

// exposeTool returns a copy of t named name, with denied paths stripped
// from its input schema. The input is not modified.
func exposeTool(t *mcp.Tool, name string, pol ArgPolicy) (*mcp.Tool, error) {
	cp := *t
	cp.Name = name
	var schema any = map[string]any{"type": "object"}
	if t.InputSchema != nil {
		schema = t.InputSchema
	}
	m, ok := pol.StripSchema(schema).(map[string]any)
	if !ok || m["type"] != "object" {
		return nil, errors.New(`input schema is not an object schema of type "object"`)
	}
	cp.InputSchema = m
	return &cp, nil
}

// handler returns the tool handler for exposed name, routed to tool on b.
func (g *Gateway) handler(b Backend, tool, name string) mcp.ToolHandler {
	pol := g.policies[b.Name()]
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		res, failure := g.call(ctx, req, b, tool, pol)
		attrs := []any{
			"user", callerID(req),
			"tool", name,
			"duration_ms", time.Since(start).Milliseconds(),
			"ok", failure == "",
		}
		if failure != "" {
			attrs = append(attrs, "failure", failure)
		}
		g.log.Info("tool call", attrs...)
		return res, nil
	}
}

// call runs one tool call. failure is "" on success, otherwise a short
// fixed label for the audit log (never upstream text).
func (g *Gateway) call(ctx context.Context, req *mcp.CallToolRequest, b Backend, tool string,
	pol ArgPolicy) (res *mcp.CallToolResult, failure string) {
	var args json.RawMessage
	if req.Params != nil {
		args = req.Params.Arguments
	}
	denied, err := pol.Check(args)
	if err != nil {
		return errorResult("invalid tool arguments: not valid JSON"), "invalid_args"
	}
	if denied != "" {
		return errorResult(fmt.Sprintf("argument %q is not allowed by gateway policy", denied)), "denied_arg"
	}

	var onProgress func(*mcp.ProgressNotificationParams)
	if req.Params != nil && req.Session != nil {
		if tok := req.Params.GetProgressToken(); tok != nil {
			r := startProgressRelay(ctx, req.Session, tok)
			defer r.stop()
			onProgress = r.send
		}
	}

	// args are the exact bytes Check inspected.
	res, err = b.Call(ctx, tool, args, onProgress)
	switch {
	case err == nil && res == nil:
		return &mcp.CallToolResult{Content: []mcp.Content{}}, ""
	case err == nil && res.IsError:
		return res, "tool_error"
	case err == nil:
		return res, ""
	case errors.Is(err, upstream.ErrUnavailable):
		return errorResult(fmt.Sprintf("upstream %q is unavailable, try again later", b.Name())), "unavailable"
	case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return errorResult("tool call cancelled"), "cancelled"
	default:
		return errorResult(fmt.Sprintf("upstream %q failed to handle the call", b.Name())), "upstream_error"
	}
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// callerID is the authenticated GitHub login, or "-".
func callerID(req *mcp.CallToolRequest) string {
	if req.Extra != nil && req.Extra.TokenInfo != nil && req.Extra.TokenInfo.UserID != "" {
		return req.Extra.TokenInfo.UserID
	}
	return "-"
}

// progressRelay forwards upstream progress to the downstream client
// without ever blocking the upstream receive loop: send only does a
// non-blocking channel send, and a per-call goroutine does the possibly
// slow NotifyProgress. When the buffer is full the oldest pending
// notification is dropped, so the latest progress wins.
type progressRelay struct {
	ctx     context.Context
	session *mcp.ServerSession
	token   any

	ch      chan *mcp.ProgressNotificationParams
	done    chan struct{} // closed by stop
	exited  chan struct{} // closed when run returns
	stopped atomic.Bool
}

func startProgressRelay(ctx context.Context, ss *mcp.ServerSession, token any) *progressRelay {
	r := &progressRelay{
		ctx:     ctx,
		session: ss,
		token:   token,
		ch:      make(chan *mcp.ProgressNotificationParams, progressBuffer),
		done:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
	go r.run()
	return r
}

// send is the Backend onProgress callback. The upstream's params carry the
// upstream's token; the client gets its own token back.
func (r *progressRelay) send(p *mcp.ProgressNotificationParams) {
	if p == nil || r.stopped.Load() {
		return
	}
	n := &mcp.ProgressNotificationParams{
		ProgressToken: r.token,
		Progress:      p.Progress,
		Total:         p.Total,
		Message:       p.Message,
	}
	select {
	case r.ch <- n:
		return
	default:
	}
	select { // full: drop the oldest, then retry once
	case <-r.ch:
	default:
	}
	select {
	case r.ch <- n:
	default:
	}
}

func (r *progressRelay) run() {
	defer close(r.exited)
	for {
		select {
		case n := <-r.ch:
			r.notify(n)
		case <-r.done:
			for { // flush what's queued so it arrives before the result
				select {
				case n := <-r.ch:
					r.notify(n)
				default:
					return
				}
			}
		}
	}
}

func (r *progressRelay) notify(n *mcp.ProgressNotificationParams) {
	// The handler ctx carries the request ID, which the streamable
	// transport uses to route the notification to the call's stream.
	_ = r.session.NotifyProgress(r.ctx, n)
}

// stop makes later send calls no-ops, flushes queued notifications, and
// waits for the relay goroutine to exit.
func (r *progressRelay) stop() {
	r.stopped.Store(true)
	close(r.done)
	<-r.exited
}
