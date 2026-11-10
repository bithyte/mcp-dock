// Package gateway holds tool naming and argument policy for the dock.
package gateway

import (
	"regexp"
	"strings"
)

var toolNameRE = regexp.MustCompile(`\A[a-zA-Z0-9_-]{1,64}\z`)

// ToolName builds the exposed tool name: server + "__" + tool.
func ToolName(server, tool string) string { return server + "__" + tool }

// SplitToolName splits at the first "__". Both halves must be non-empty.
func SplitToolName(name string) (server, tool string, ok bool) {
	i := strings.Index(name, "__")
	if i <= 0 || i+2 >= len(name) {
		return "", "", false
	}
	return name[:i], name[i+2:], true
}

// ValidToolName reports whether name is a legal exposed tool name.
func ValidToolName(name string) bool { return toolNameRE.MatchString(name) }
