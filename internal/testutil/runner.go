package testutil

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/bithyte/mcp-dock/internal/runner"
)

// RunnerOptions customizes StartRunner.
type RunnerOptions struct {
	// Mod edits the runner options after defaults are filled in.
	Mod func(*runner.Options)
	// Wrap, if set, wraps the runner's handler before serving it.
	Wrap func(http.Handler) http.Handler
}

// StartRunner serves a runner in front of the fake upstream and returns its
// /mcp URL. Cleanup closes the runner, then the server.
func StartRunner(t testing.TB, o RunnerOptions) (string, *runner.Runner) {
	t.Helper()
	opts := runner.Options{
		Command: BuildFakeUpstream(t),
		Env:     []string{"PATH=" + os.Getenv("PATH")},
		WorkDir: t.TempDir(),
	}
	if o.Mod != nil {
		o.Mod(&opts)
	}
	r := runner.New(opts)
	h := r.Handler()
	if o.Wrap != nil {
		h = o.Wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		r.Close()
		srv.Close()
	})
	return srv.URL + "/mcp", r
}
