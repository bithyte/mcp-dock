package runner

import "strings"

// ChildEnv returns environ without any MCP_RUNNER_* entries, so the runner's
// own settings (the bearer token in particular) never reach the child. The
// result is never nil.
func ChildEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		if strings.HasPrefix(e, "MCP_RUNNER_") {
			continue
		}
		out = append(out, e)
	}
	return out
}
