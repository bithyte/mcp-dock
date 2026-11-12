package oplogin

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestVerifyHash(t *testing.T) {
	const cn, fn, ref, as = "client-nonce", "finish-nonce", "interact-ref", "https://auth.example/"
	sum := sha256.Sum256([]byte(cn + "\n" + fn + "\n" + ref + "\n" + as))
	urlForm := base64.RawURLEncoding.EncodeToString(sum[:])
	stdForm := base64.StdEncoding.EncodeToString(sum[:])

	for _, h := range []string{urlForm, stdForm} {
		if !VerifyHash(cn, fn, ref, as, h) {
			t.Errorf("VerifyHash(%q) = false, want true", h)
		}
	}

	flip := func(s string) string {
		b := []byte(s)
		if b[3] == 'A' {
			b[3] = 'B'
		} else {
			b[3] = 'A'
		}
		return string(b)
	}
	bad := []struct {
		name               string
		cn, fn, ref, as, h string
	}{
		{"client nonce", cn + "x", fn, ref, as, urlForm},
		{"finish nonce", cn, fn + "x", ref, as, urlForm},
		{"interact ref", cn, fn, ref + "x", as, urlForm},
		{"auth server", cn, fn, ref, as + "x", urlForm},
		{"url hash char", cn, fn, ref, as, flip(urlForm)},
		{"std hash char", cn, fn, ref, as, flip(stdForm)},
		{"empty hash", cn, fn, ref, as, ""},
		{"not base64", cn, fn, ref, as, "!!!!"},
	}
	for _, tc := range bad {
		if VerifyHash(tc.cn, tc.fn, tc.ref, tc.as, tc.h) {
			t.Errorf("%s: VerifyHash = true, want false", tc.name)
		}
	}
}
