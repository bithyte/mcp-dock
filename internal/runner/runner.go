// Package runner serves one stdio MCP server command as streamable HTTP. Each
// HTTP session gets its own child process; JSON-RPC messages pass through
// unchanged in both directions.
package runner

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// Defaults for zero Options fields.
const (
	DefaultMaxSessions  = 4
	DefaultIdleTimeout  = 10 * time.Minute
	DefaultMaxBodyBytes = 4 << 20
)

const sessionIDHeader = "Mcp-Session-Id"

// Options configures a Runner.
type Options struct {
	Command string
	Args    []string
	// Env is the child's environment, exactly. Nothing is inherited.
	Env []string
	// WorkDir is the child's working dir, created 0700 if missing.
	WorkDir     string
	BearerToken string
	// MaxSessions caps live sessions. Zero means DefaultMaxSessions.
	MaxSessions int
	// IdleTimeout ends sessions with nothing in flight. Zero means DefaultIdleTimeout.
	IdleTimeout time.Duration
	// MaxBodyBytes caps a POST body. Zero means DefaultMaxBodyBytes.
	MaxBodyBytes int64
	Logger       *slog.Logger
}

// Runner relays streamable HTTP sessions to child processes.
type Runner struct {
	opts Options
	log  *slog.Logger
	wg   sync.WaitGroup // one per session not yet fully ended

	mu       sync.Mutex
	closed   bool
	sessions map[string]*session
}

// New returns a Runner. It starts nothing until the first initialize.
func New(opts Options) *Runner {
	if opts.MaxSessions == 0 {
		opts.MaxSessions = DefaultMaxSessions
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{opts: opts, log: log, sessions: make(map[string]*session)}
}

// Handler serves /mcp and /healthz.
func (r *Runner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", r.serveMCP)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	return mux
}

// Sessions returns the number of live sessions.
func (r *Runner) Sessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Close ends every session, waits for their children to exit, and makes later
// initialize requests get 503.
func (r *Runner) Close() {
	r.mu.Lock()
	r.closed = true
	live := make([]*session, 0, len(r.sessions))
	for _, s := range r.sessions {
		live = append(live, s)
	}
	r.mu.Unlock()
	for _, s := range live {
		go s.end(reasonShutdown)
	}
	r.wg.Wait()
}

// remove drops s from the table and returns how many sessions are left.
func (r *Runner) remove(s *session) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[s.id] == s {
		delete(r.sessions, s.id)
	}
	return len(r.sessions)
}

func (r *Runner) lookup(id string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *Runner) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (r *Runner) serveMCP(w http.ResponseWriter, req *http.Request) {
	id := req.Header.Get(sessionIDHeader)
	switch req.Method {
	case http.MethodPost:
		if !checkPostHeaders(w, req) {
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, r.opts.MaxBodyBytes)
		if id == "" {
			r.serveInitialize(w, req)
			return
		}
	case http.MethodGet:
		if id == "" {
			http.Error(w, "GET requires an Mcp-Session-Id header", http.StatusBadRequest)
			return
		}
	case http.MethodDelete:
		if id == "" {
			http.Error(w, "DELETE requires an Mcp-Session-Id header", http.StatusBadRequest)
			return
		}
		s := r.lookup(id)
		if s == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		s.end(reasonDelete)
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := r.lookup(id)
	if s == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.transport.ServeHTTP(w, req)
}

// serveInitialize handles a POST without a session id. Only a lone initialize
// request may start a child.
func (r *Runner) serveInitialize(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	msg, err := jsonrpc.DecodeMessage(body)
	if jreq, ok := msg.(*jsonrpc.Request); err != nil || !ok || jreq.Method != "initialize" || !jreq.IsCall() {
		http.Error(w, "missing Mcp-Session-Id: only an initialize request may start a session", http.StatusBadRequest)
		return
	}
	if r.isClosed() {
		http.Error(w, "runner is shutting down", http.StatusServiceUnavailable)
		return
	}

	id := rand.Text()
	s, err := newSession(r, id)
	if err != nil {
		r.log.Error("upstream failed to start", "session", id, "err", err)
		http.Error(w, "upstream failed to start", http.StatusBadGateway)
		return
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		s.cancel()
		s.sconn.Close()
		s.cconn.Close()
		s.stderr.Flush()
		http.Error(w, "runner is shutting down", http.StatusServiceUnavailable)
		return
	}
	r.sessions[id] = s
	r.wg.Add(1)
	n := len(r.sessions)
	r.mu.Unlock()

	s.log.Info("session started", "sessions", n)
	s.start()
	req.Body = io.NopCloser(bytes.NewReader(body))
	s.transport.ServeHTTP(w, req)
}

// checkPostHeaders enforces the streamable HTTP Content-Type and Accept rules
// for POST, writing the error response if they fail.
func checkPostHeaders(w http.ResponseWriter, req *http.Request) bool {
	mt, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	jsonOK, streamOK := false, false
	for _, v := range req.Header.Values("Accept") {
		for _, tok := range strings.Split(v, ",") {
			base, _, _ := strings.Cut(tok, ";")
			switch strings.ToLower(strings.TrimSpace(base)) {
			case "application/json", "application/*":
				jsonOK = true
			case "text/event-stream", "text/*":
				streamOK = true
			case "*/*":
				jsonOK, streamOK = true, true
			}
		}
	}
	if !jsonOK || !streamOK {
		http.Error(w, "Accept must contain both application/json and text/event-stream", http.StatusBadRequest)
		return false
	}
	return true
}
