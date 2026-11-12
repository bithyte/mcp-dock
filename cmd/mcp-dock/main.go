// Command mcp-dock is an OAuth-protected MCP gateway in front of streamable
// HTTP MCP servers (usually mcp-runner containers).
//
//	mcp-dock serve [-config path]
//	mcp-dock users reset [-config path] <login | wallet address>
//	mcp-dock version
//
// The config path defaults to $MCP_DOCK_CONFIG, else ./config.yaml.
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
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bithyte/mcp-dock/internal/app"
	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/identity"
	"github.com/bithyte/mcp-dock/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute // idle keep-alive connections
	shutdownTimeout   = 10 * time.Second
	cleanupInterval   = time.Hour
)

const usage = `usage:
  mcp-dock serve [-config path]
  mcp-dock users reset [-config path] <login | wallet address>
  mcp-dock version
config path defaults to $MCP_DOCK_CONFIG, else ./config.yaml`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // a second signal kills the process instead of waiting
	}()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-dock:", err)
		os.Exit(1)
	}
}

func usageError(format string, a ...any) error {
	return fmt.Errorf("%s\n%s", fmt.Sprintf(format, a...), usage)
}

// run executes one CLI command. serve returns when ctx is done.
func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError("missing command")
	}
	switch args[0] {
	case "serve":
		cfgPath, rest, err := parseFlags("serve", args[1:], getenv)
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return usageError("serve takes no arguments")
		}
		cfg, err := config.Load(cfgPath, getenv)
		if err != nil {
			return err
		}
		return serve(ctx, cfg, stdout)
	case "version":
		if len(args) > 1 {
			return usageError("version takes no arguments")
		}
		fmt.Fprintln(stdout, version)
		return nil
	case "users":
		if len(args) < 2 || args[1] != "reset" {
			return usageError("unknown users command")
		}
		cfgPath, rest, err := parseFlags("users reset", args[2:], getenv)
		if err != nil {
			return err
		}
		if len(rest) != 1 || rest[0] == "" {
			return usageError("users reset takes exactly one user")
		}
		cfg, err := config.Load(cfgPath, getenv)
		if err != nil {
			return err
		}
		return resetUser(ctx, cfg, rest[0], stdout)
	default:
		return usageError("unknown command %q", args[0])
	}
}

// parseFlags parses -config and returns the config path and the remaining
// arguments.
func parseFlags(name string, args []string, getenv func(string) string) (string, []string, error) {
	def := getenv("MCP_DOCK_CONFIG")
	if def == "" {
		def = "config.yaml"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", def, "config file path")
	if err := fs.Parse(args); err != nil {
		return "", nil, usageError("%v", err)
	}
	return *path, fs.Args(), nil
}

func openStore(cfg *config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// MkdirAll leaves an existing dir's mode alone. The dir holds the
	// token database, so tighten it.
	if err := os.Chmod(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod data dir: %w", err)
	}
	return store.Open(filepath.Join(cfg.DataDir, "gateway.db"))
}

func resetUser(ctx context.Context, cfg *config.Config, arg string, stdout io.Writer) error {
	uid, err := identity.ParseCLIUser(arg)
	if err != nil {
		return usageError("users reset: %v", err)
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.ResetUser(ctx, uid); err != nil {
		return fmt.Errorf("reset user: %w", err)
	}
	msg := "reset " + uid + ": cleared consents and tokens"
	if p, _, _ := identity.Split(uid); p == identity.GitHub {
		msg += " and stored GitHub ID"
	}
	fmt.Fprintln(stdout, msg)
	return nil
}

func serve(ctx context.Context, cfg *config.Config, stdout io.Writer) error {
	log := slog.New(slog.NewJSONHandler(stdout, nil))

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	a, err := app.Build(cfg, log, st, app.Options{Version: version})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	names := make([]string, 0, len(cfg.Upstreams))
	for _, uc := range cfg.Upstreams {
		names = append(names, uc.Name)
	}

	srv := newHTTPServer(a.Handler, log)

	upCtx, cancelUp := context.WithCancel(context.Background())
	defer cancelUp()
	var wg sync.WaitGroup
	wg.Go(func() { a.RunUpstreams(upCtx) })
	wg.Go(func() { cleanupLoop(upCtx, st, log) })

	log.Info("mcp-dock listening", "addr", ln.Addr().String(), "public_url", cfg.PublicURL,
		"version", version, "upstreams", names, "allowed_github", len(cfg.AllowedUsers.GitHub),
		"allowed_open_payments", len(cfg.AllowedUsers.OpenPayments))

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var result error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		if err := srv.Shutdown(shCtx); err != nil {
			log.Warn("graceful shutdown incomplete; closing connections", "error", err.Error())
			srv.Close()
		}
		cancel()
		<-serveErr // http.ErrServerClosed
	case err := <-serveErr:
		result = fmt.Errorf("serve: %w", err)
		srv.Close()
	}

	cancelUp()
	wg.Wait()
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

// cleanupLoop deletes expired oauth rows at startup and then hourly.
func cleanupLoop(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		if err := st.DeleteExpired(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("delete expired oauth state failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
