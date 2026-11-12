package identity

import "testing"

func TestNormalizeWalletAddress(t *testing.T) {
	ok := map[string]string{
		"$ilp.interledger-test.dev/cozmin":           "https://ilp.interledger-test.dev/cozmin",
		"  https://ILP.Interledger-Test.dev/Cozmin ": "https://ilp.interledger-test.dev/Cozmin",
		"https://ilp.interledger-test.dev:443/a":     "https://ilp.interledger-test.dev/a",
		"https://w.example/a/":                       "https://w.example/a",
		"$w.example":                                 "https://w.example/.well-known/pay",
		"https://w.example/":                         "https://w.example/.well-known/pay",
		"https://127.0.0.1:8443/alice":               "https://127.0.0.1:8443/alice",
	}
	for in, want := range ok {
		got, err := NormalizeWalletAddress(in)
		if err != nil || got != want {
			t.Errorf("NormalizeWalletAddress(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "alice", "http://w.example/a", "https://w.example/a?x=1", "https://w.example/a?",
		"https://w.example/a#f", "https://w.example/a#", "https://u:p@w.example/a", "https:///a", "ftp://w.example/a", "$"}
	for _, in := range bad {
		if got, err := NormalizeWalletAddress(in); err == nil {
			t.Errorf("NormalizeWalletAddress(%q) = %q, want error", in, got)
		}
	}
}

func TestSplitAndUsers(t *testing.T) {
	if got := GitHubUser("Cozmin"); got != "github:cozmin" {
		t.Errorf("GitHubUser = %q", got)
	}
	if got := WalletUser("https://w.example/Al"); got != "open_payments:https://w.example/Al" {
		t.Errorf("WalletUser = %q", got)
	}
	p, name, ok := Split("open_payments:https://w.example/a")
	if p != OpenPayments || name != "https://w.example/a" || !ok {
		t.Errorf("Split = %q, %q, %v", p, name, ok)
	}
	p, name, ok = Split("github:cozmin")
	if p != GitHub || name != "cozmin" || !ok {
		t.Errorf("Split github = %q, %q, %v", p, name, ok)
	}
	for _, id := range []string{"cozmin", "github:", "google:x", ""} {
		if _, _, ok := Split(id); ok {
			t.Errorf("Split(%q) ok = true", id)
		}
	}
}

func TestParseCLIUser(t *testing.T) {
	ok := map[string]string{
		"Cozmin":                             "github:cozmin",
		"github:Cozmin":                      "github:cozmin",
		"$w.example/Al":                      "open_payments:https://w.example/Al",
		"https://W.example/Al":               "open_payments:https://w.example/Al",
		"open_payments:$w.example/Al":        "open_payments:https://w.example/Al",
		"open_payments:https://w.example/Al": "open_payments:https://w.example/Al",
	}
	for in, want := range ok {
		got, err := ParseCLIUser(in)
		if err != nil || got != want {
			t.Errorf("ParseCLIUser(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "http://w.example/a", "google:x", "github:", "open_payments:", "open_payments:http://w.example/a"} {
		if got, err := ParseCLIUser(in); err == nil {
			t.Errorf("ParseCLIUser(%q) = %q, want error", in, got)
		}
	}
}

func TestDescribeAndLabel(t *testing.T) {
	if got := Describe("github:cozmin"); got != "GitHub account cozmin" {
		t.Errorf("Describe = %q", got)
	}
	if got := Describe("open_payments:https://w.example/a"); got != "Open Payments wallet address https://w.example/a" {
		t.Errorf("Describe = %q", got)
	}
	if got := Describe("weird"); got != "weird" {
		t.Errorf("Describe unknown = %q", got)
	}
	if Label(GitHub) != "GitHub" || Label(OpenPayments) != "Open Payments" {
		t.Error("Label")
	}
}
