package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/bithyte/mcp-dock/internal/linelog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Reasons a session ends, as logged.
const (
	reasonDelete    = "delete"
	reasonChildExit = "child_exit"
	reasonIdle      = "idle"
	reasonShutdown  = "shutdown"
)

// closeTimeout is how long the child gets after stdin closes before SIGTERM,
// and again before SIGKILL. It's also the exec WaitDelay.
const closeTimeout = 2 * time.Second

// session is one HTTP session relayed to one child process.
type session struct {
	id        string
	log       *slog.Logger
	r         *Runner
	transport *mcp.StreamableServerTransport
	sconn     mcp.Connection // client side, fed by transport.ServeHTTP
	cconn     mcp.Connection // child side, over its stdio
	stderr    *linelog.Writer
	ctx       context.Context
	cancel    context.CancelFunc

	mu    sync.Mutex
	ended bool
	done  chan struct{} // closed once the child has exited and cleanup finished
}

// newSession starts the child and connects both transports. The session isn't
// in the table and its pumps aren't running yet; see start.
func newSession(r *Runner, id string) (*session, error) {
	log := r.log.With("session", id)
	if r.opts.WorkDir != "" {
		if err := os.MkdirAll(r.opts.WorkDir, 0o700); err != nil {
			return nil, fmt.Errorf("create work dir: %w", err)
		}
	}
	env := r.opts.Env
	if env == nil {
		env = []string{} // nil would make exec inherit the runner's env
	}
	stderr := linelog.New(log)
	cmd := exec.Command(r.opts.Command, r.opts.Args...)
	cmd.Env = env
	cmd.Dir = r.opts.WorkDir
	cmd.Stderr = stderr
	cmd.WaitDelay = closeTimeout

	ctx, cancel := context.WithCancel(context.Background())
	cconn, err := (&mcp.CommandTransport{Command: cmd, TerminateDuration: closeTimeout}).Connect(ctx)
	if err != nil {
		cancel()
		stderr.Flush()
		return nil, fmt.Errorf("start child: %w", err)
	}
	transport := &mcp.StreamableServerTransport{SessionID: id}
	sconn, err := transport.Connect(ctx)
	if err != nil {
		cancel()
		cconn.Close()
		stderr.Flush()
		return nil, fmt.Errorf("connect server transport: %w", err)
	}
	return &session{
		id:        id,
		log:       log,
		r:         r,
		transport: transport,
		sconn:     sconn,
		cconn:     cconn,
		stderr:    stderr,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}, nil
}

// start runs the relay pumps. Call it once the session is in the table, so a
// child that exits right away still removes it.
func (s *session) start() {
	go s.clientToChild()
	go s.childToClient()
}

func (s *session) clientToChild() {
	for {
		msg, err := s.sconn.Read(s.ctx)
		if err != nil {
			return // sconn closed: the session is ending
		}
		if err := s.cconn.Write(s.ctx, msg); err != nil {
			// The child is likely gone; childToClient sees that and ends the session.
			s.log.Debug("write to child failed", "err", err)
		}
	}
}

func (s *session) childToClient() {
	for {
		msg, err := s.cconn.Read(s.ctx)
		if err != nil {
			s.end(reasonChildExit)
			return
		}
		if err := s.sconn.Write(s.ctx, msg); err != nil {
			// Undeliverable (no open stream for it, request already done):
			// not fatal for the session.
			s.log.Debug("write to client failed", "err", err)
		}
	}
}

// end ends the session once: it removes it from the table, closes both sides
// and waits for the child to exit. Later calls return at once; use wait to
// block until the first one finishes.
func (s *session) end(reason string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.mu.Unlock()

	n := s.r.remove(s)
	s.cancel()
	s.sconn.Close()
	err := s.cconn.Close() // closes stdin, then SIGTERM, then SIGKILL; waits for exit
	s.stderr.Flush()
	attrs := []any{"reason", reason, "sessions", n}
	if err != nil && !errors.Is(err, os.ErrClosed) {
		attrs = append(attrs, "exit", err.Error())
	}
	s.log.Info("session ended", attrs...)
	close(s.done)
	s.r.wg.Done()
}

func (s *session) wait() { <-s.done }
