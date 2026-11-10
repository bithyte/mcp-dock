// Command mcp-dock is an OAuth-protected MCP gateway in front of local
// stdio MCP servers.
//
//	mcp-dock serve [-config path]
//	mcp-dock users reset [-config path] <login>
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/gateway"
	"github.com/bithyte/mcp-dock/internal/github"
	"github.com/bithyte/mcp-dock/internal/httpserver"
	"github.com/bithyte/mcp-dock/internal/oauth"
	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/upstream"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
	cleanupInterval   = time.Hour
)

const usage = `usage:
  mcp-dock serve [-config path]
  mcp-dock users reset [-config path] <login>
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
		return serve(ctx, cfg, getenv, stdout)
	case "users":
		if len(args) < 2 || args[1] != "reset" {
			return usageError("unknown users command")
		}
		cfgPath, rest, err := parseFlags("users reset", args[2:], getenv)
		if err != nil {
			return err
		}
		if len(rest) != 1 || rest[0] == "" {
			return usageError("users reset takes exactly one login")
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
	return store.Open(filepath.Join(cfg.DataDir, "gateway.db"))
}

func resetUser(ctx context.Context, cfg *config.Config, login string, stdout io.Writer) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.ResetUser(ctx, login); err != nil {
		return fmt.Errorf("reset user: %w", err)
	}
	fmt.Fprintf(stdout, "reset %s: cleared stored GitHub ID, consents and tokens\n", strings.ToLower(login))
	return nil
}

func serve(ctx context.Context, cfg *config.Config, getenv func(string) string, stdout io.Writer) error {
	log := slog.New(slog.NewJSONHandler(stdout, nil))

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	var gw *gateway.Gateway
	backends := make([]gateway.Backend, 0, len(cfg.Upstreams))
	ups := make([]*upstream.Upstream, 0, len(cfg.Upstreams))
	policies := map[string]gateway.ArgPolicy{}
	names := make([]string, 0, len(cfg.Upstreams))
	for _, uc := range cfg.Upstreams {
		u := upstream.New(upstream.Options{
			Name:    uc.Name,
			Command: uc.Command,
			Args:    uc.Args,
			Env:     uc.Environ(getenv),
			WorkDir: filepath.Join(cfg.DataDir, "upstreams", uc.Name, "cwd"),
			Logger:  log,
			// gw is set below, before any upstream runs.
			OnToolsChanged: func() { gw.Sync() },
		})
		ups = append(ups, u)
		backends = append(backends, u)
		policies[uc.Name] = gateway.NewArgPolicy(uc.DenyArgs)
		names = append(names, uc.Name)
	}
	gw = gateway.New(gateway.Options{Backends: backends, Policies: policies, Logger: log, Version: version})
	gw.Sync()

	osrv := oauth.New(oauth.Options{
		Config: cfg,
		Store:  st,
		GitHub: &github.Client{ClientID: cfg.GitHub.ClientID, ClientSecret: cfg.GitHub.ClientSecret},
		Logger: log,
	})
	srv := &http.Server{
		Handler:           httpserver.NewHandler(osrv, gw),
		ReadHeaderTimeout: readHeaderTimeout,
		// No WriteTimeout: tool calls can block for hours.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	upCtx, cancelUp := context.WithCancel(context.Background())
	defer cancelUp()
	var wg sync.WaitGroup
	for _, u := range ups {
		wg.Go(func() { u.Run(upCtx) })
	}
	wg.Go(func() { cleanupLoop(upCtx, st, log) })

	log.Info("mcp-dock listening", "addr", ln.Addr().String(), "public_url", cfg.PublicURL,
		"version", version, "upstreams", names, "allowed_users", len(cfg.AllowedUsers))

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
