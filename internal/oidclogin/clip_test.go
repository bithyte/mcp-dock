package oidclogin

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClipKeepsRunesWhole(t *testing.T) {
	if got := clip("short"); got != "short" {
		t.Fatalf("clip(short) = %q", got)
	}
	// Two-byte runes starting at an odd offset put byte 200 mid-rune.
	in := "a" + strings.Repeat("\u00e9", 150)
	got := clip(in)
	if !utf8.ValidString(got) {
		t.Fatalf("clip split a rune: %q", got)
	}
	if !strings.HasSuffix(got, "...") || len(got) > 203 {
		t.Fatalf("clip = %q (%d bytes)", got, len(got))
	}
}
