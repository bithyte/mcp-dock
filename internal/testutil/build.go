// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
	buildOut  []byte
)

// BuildFakeUpstream compiles ./internal/testutil/fakeupstream once per test
// binary and returns the path to the executable.
//
// The binary lives in a directory from os.MkdirTemp rather than t.TempDir:
// t.TempDir is removed when the first calling test ends, which would break
// every later test sharing the sync.Once result.
func BuildFakeUpstream(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mcp-dock-fakeupstream-")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "fakeupstream")
		cmd := exec.Command("go", "build", "-o", buildPath,
			"github.com/bithyte/mcp-dock/internal/testutil/fakeupstream")
		buildOut, buildErr = cmd.CombinedOutput()
	})
	if buildErr != nil {
		t.Fatalf("build fakeupstream: %v\n%s", buildErr, buildOut)
	}
	return buildPath
}
