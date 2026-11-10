// Package app builds the gateway from a config: upstream supervisors, the
// merged MCP gateway with its deny_args policies, the OAuth server and the
// root HTTP handler. mcp-dock serve and the end-to-end tests both use Build,
// so the tests exercise the same wiring as production.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/gateway"
	"github.com/bithyte/mcp-dock/internal/github"
	"github.com/bithyte/mcp-dock/internal/httpserver"
	"github.com/bithyte/mcp-dock/internal/oauth"
	"github.com/bithyte/mcp-dock/internal/store"
	"github.com/bithyte/mcp-dock/internal/upstream"
)

// Options holds the build version and test overrides. Zero values mean
// the production defaults.
type Options struct {
	Version string

	GitHubWebBaseURL string       // default https://github.com
	GitHubAPIBaseURL string       // default https://api.github.com
	GitHubHTTP       *http.Client // default: the github package's client
	MinBackoff       time.Duration
	MaxBackoff       time.Duration
	Now              func() time.Time // oauth clock, default time.Now
}

// App is a built, not yet running, gateway.
type App struct {
	Handler   http.Handler
	Gateway   *gateway.Gateway
	OAuth     *oauth.Server
	Upstreams []*upstream.Upstream
}

// Build wires everything from cfg. Upstream processes get only the env
// Environ builds from getenv, and an empty working dir under cfg.DataDir.
// Nothing runs until RunUpstreams.
func Build(cfg *config.Config, getenv func(string) string, log *slog.Logger, st *store.Store, opts Options) *App {
	a := &App{}
	var gw *gateway.Gateway
	backends := make([]gateway.Backend, 0, len(cfg.Upstreams))
	policies := map[string]gateway.ArgPolicy{}
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
			MinBackoff:     opts.MinBackoff,
			MaxBackoff:     opts.MaxBackoff,
		})
		a.Upstreams = append(a.Upstreams, u)
		backends = append(backends, u)
		policies[uc.Name] = gateway.NewArgPolicy(uc.DenyArgs)
	}
	gw = gateway.New(gateway.Options{Backends: backends, Policies: policies, Logger: log, Version: opts.Version})
	gw.Sync()
	a.Gateway = gw

	a.OAuth = oauth.New(oauth.Options{
		Config: cfg,
		Store:  st,
		GitHub: &github.Client{
			ClientID:     cfg.GitHub.ClientID,
			ClientSecret: cfg.GitHub.ClientSecret,
			WebBaseURL:   opts.GitHubWebBaseURL,
			APIBaseURL:   opts.GitHubAPIBaseURL,
			HTTP:         opts.GitHubHTTP,
		},
		Logger: log,
		Now:    opts.Now,
	})
	a.Handler = httpserver.NewHandler(a.OAuth, gw)
	return a
}

// RunUpstreams runs every upstream supervisor until ctx is done, then
// waits for all of them to stop.
func (a *App) RunUpstreams(ctx context.Context) {
	var wg sync.WaitGroup
	for _, u := range a.Upstreams {
		wg.Go(func() { u.Run(ctx) })
	}
	wg.Wait()
}
