package gateway

import (
	"strings"
	"testing"
)

func TestToolName(t *testing.T) {
	if got := ToolName("jaypost", "ask_user"); got != "jaypost__ask_user" {
		t.Fatalf("got %q", got)
	}
}

func TestSplitToolName(t *testing.T) {
	cases := []struct {
		in, s, tool string
		ok          bool
	}{
		{"jaypost__ask_user", "jaypost", "ask_user", true},
		{"a__b__c", "a", "b__c", true},
		{"noprefix", "", "", false},
		{"__x", "", "", false},
		{"x__", "", "", false},
	}
	for _, c := range cases {
		s, tl, ok := SplitToolName(c.in)
		if s != c.s || tl != c.tool || ok != c.ok {
			t.Errorf("%q: got (%q,%q,%v)", c.in, s, tl, ok)
		}
	}
}

func TestValidToolName(t *testing.T) {
	cases := map[string]bool{
		strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false,
		"":                      false,
		"a.b":                   false,
		"jaypost__ask_user":     true,
		"a-b_C9":                true,
		"a\n":                   false,
	}
	for in, want := range cases {
		if got := ValidToolName(in); got != want {
			t.Errorf("%q: got %v", in, got)
		}
	}
}
