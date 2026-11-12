// Command mcp-runner serves one stdio MCP server command as streamable HTTP,
// one child process per session.
//
//	mcp-runner [flags] [--] <command> [args...]
//
// Every flag except -version falls back to an MCP_RUNNER_* environment
// variable. The bearer token is read from MCP_RUNNER_BEARER_TOKEN only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bithyte/mcp-dock/internal/runner"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute // idle keep-alive connections
	shutdownTimeout   = 10 * time.Second
)

const usage = `usage: mcp-runner [flags] [--] <command> [args...]
flags (each falls back to the env var shown, then to the default):
  -listen addr         listen address (MCP_RUNNER_LISTEN, default :8080)
  -max-sessions n      concurrent sessions (MCP_RUNNER_MAX_SESSIONS, default 4)
  -idle-timeout dur    end idle sessions after dur (MCP_RUNNER_IDLE_TIMEOUT, default 10m)
  -workdir dir         child working dir (MCP_RUNNER_WORKDIR, default: current dir)
  -version             print version and exit
bearer token: MCP_RUNNER_BEARER_TOKEN (env only)`

// usageErr is a command-line mistake; main exits 2 for it.
type usageErr struct{ msg string }

func (e *usageErr) Error() string { return e.msg + "\n" + usage }

func usageError(format string, a ...any) error {
	return &usageErr{msg: fmt.Sprintf(format, a...)}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // a second signal kills the process instead of waiting
	}()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Environ(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-runner:", err)
		var ue *usageErr
		if errors.As(err, &ue) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type config struct {
	listen      string
	maxSessions int
	idleTimeout time.Duration
	workDir     string
	bearerToken string
	showVersion bool
	command     string
	args        []string
}

// envOr returns getenv(key), or def if it is empty.
func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// parseConfig parses args. For each setting the flag wins over its
// MCP_RUNNER_* variable, which wins over the default.
func parseConfig(args []string, getenv func(string) string) (config, error) {
	var cfg config
	workDir := getenv("MCP_RUNNER_WORKDIR")
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return cfg, fmt.Errorf("working dir: %w", err)
		}
		workDir = wd
	}

	fs := flag.NewFlagSet("mcp-runner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen", envOr(getenv, "MCP_RUNNER_LISTEN", ":8080"), "listen address")
	maxSessions := fs.String("max-sessions", envOr(getenv, "MCP_RUNNER_MAX_SESSIONS", "4"), "max concurrent sessions")
	idle := fs.String("idle-timeout", envOr(getenv, "MCP_RUNNER_IDLE_TIMEOUT", "10m"), "idle session timeout")
	wd := fs.String("workdir", workDir, "child working dir")
	showVersion := fs.Bool("version", false, "print version")
	if err := fs.Parse(args); err != nil {
		return cfg, usageError("%v", err)
	}

	n, err := strconv.Atoi(*maxSessions)
	if err != nil || n < 1 {
		return cfg, usageError("invalid max sessions %q: want an integer >= 1", *maxSessions)
	}
	d, err := time.ParseDuration(*idle)
	if err != nil || d <= 0 {
		return cfg, usageError("invalid idle timeout %q: want a positive duration like 10m", *idle)
	}

	cfg = config{
		listen:      *listen,
		maxSessions: n,
		idleTimeout: d,
		workDir:     *wd,
		bearerToken: getenv("MCP_RUNNER_BEARER_TOKEN"),
		showVersion: *showVersion,
	}
	if rest := fs.Args(); len(rest) > 0 {
		cfg.command, cfg.args = rest[0], rest[1:]
	}
	return cfg, nil
}

// run starts the runner and returns when ctx is done. environ is the
// process environment the child's is derived from.
func run(ctx context.Context, args []string, getenv func(string) string, environ []string, stdout io.Writer) error {
	cfg, err := parseConfig(args, getenv)
	if err != nil {
		return err
	}
	if cfg.showVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}
	if cfg.command == "" {
		return usageError("missing command")
	}
	// Fail at startup rather than on the first session.
	path, err := exec.LookPath(cfg.command)
	if err != nil {
		return fmt.Errorf("command %q: %w", cfg.command, err)
	}

	log := slog.New(slog.NewJSONHandler(stdout, nil))
	r := runner.New(runner.Options{
		Command:     path,
		Args:        cfg.args,
		Env:         runner.ChildEnv(environ),
		WorkDir:     cfg.workDir,
		BearerToken: cfg.bearerToken,
		MaxSessions: cfg.maxSessions,
		IdleTimeout: cfg.idleTimeout,
		Logger:      log,
	})

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := newHTTPServer(r.Handler(), log)
	log.Info("mcp-runner listening", "version", version, "listen", ln.Addr().String(),
		"command", cfg.command, "auth", cfg.bearerToken != "", "max_sessions", cfg.maxSessions)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var result error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
		// End sessions first: that closes their SSE streams, which would
		// otherwise keep srv.Shutdown waiting.
		r.Close()
		shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		if err := srv.Shutdown(shCtx); err != nil {
			log.Warn("graceful shutdown incomplete; closing connections", "error", err.Error())
			srv.Close()
		}
		cancel()
		<-serveErr // http.ErrServerClosed
	case err := <-serveErr:
		result = fmt.Errorf("serve: %w", err)
		r.Close()
		srv.Close()
	}
	if result == nil {
		log.Info("stopped")
	}
	return result
}

func newHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout or ReadTimeout: tool calls can block for hours.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
