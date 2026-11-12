package oplogin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// VerifyHash checks the interaction finish hash the auth server sent back:
// sha256 over clientNonce, finishNonce, interactRef and authServer joined by
// newlines. The hash may be unpadded base64url or padded standard base64.
// The comparison is constant time.
func VerifyHash(clientNonce, finishNonce, interactRef, authServer, hash string) bool {
	if hash == "" {
		return false
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(hash)
	if err != nil {
		if got, err = base64.StdEncoding.Strict().DecodeString(hash); err != nil {
			return false
		}
	}
	want := sha256.Sum256([]byte(clientNonce + "\n" + finishNonce + "\n" + interactRef + "\n" + authServer))
	return subtle.ConstantTimeCompare(got, want[:]) == 1
}
