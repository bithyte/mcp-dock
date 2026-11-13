package runner

import (
	"slices"
	"testing"
)

func TestChildEnv(t *testing.T) {
	got := ChildEnv([]string{"PATH=/bin", "MCP_RUNNER_BEARER_TOKEN=x", "MCP_RUNNER_LISTEN=:1", "MCP_RUNNERX=y", "FOO=bar"})
	want := []string{"PATH=/bin", "MCP_RUNNERX=y", "FOO=bar"}
	if !slices.Equal(got, want) {
		t.Fatalf("ChildEnv = %q, want %q", got, want)
	}
	if got := ChildEnv(nil); got == nil {
		t.Fatal("ChildEnv(nil) is nil, want empty non-nil slice")
	}
}
