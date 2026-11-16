package testutil

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// FakeOIDCOptions tunes a FakeOIDC at creation.
type FakeOIDCOptions struct {
	// IssuerTrailingSlash makes the issuer end in "/" (Auth0 style).
	IssuerTrailingSlash bool
}

type fakeOIDCCode struct {
	clientID    string
	redirectURI string
	challenge   string
	nonce       string
	sub         string
	claims      map[string]any
}

// FakeOIDC is an in-process OpenID Connect provider: discovery, authorize,
// token (client_secret_basic, PKCE S256 required) and JWKS endpoints, with
// RS256-signed ID tokens. Knobs are sticky until changed and safe for
// concurrent use.
type FakeOIDC struct {
	*httptest.Server

	clientID, clientSecret string
	opts                   FakeOIDCOptions

	mu          sync.Mutex
	sub         string
	claims      map[string]any
	errCode     string
	down        bool
	otherKey    bool
	algNone     bool
	discoveries int
	lastAuth    url.Values
	lastAccess  string
	codes       map[string]*fakeOIDCCode
}

var (
	oidcKeysOnce        sync.Once
	oidcKey, oidcKeyBad *rsa.PrivateKey
	oidcKeyErr          error
)

// oidcKeys returns the JWKS signing key and a key the JWKS doesn't know,
// generated once per test binary.
func oidcKeys(t testing.TB) (good, other *rsa.PrivateKey) {
	t.Helper()
	oidcKeysOnce.Do(func() {
		oidcKey, oidcKeyErr = rsa.GenerateKey(rand.Reader, 2048)
		if oidcKeyErr == nil {
			oidcKeyBad, oidcKeyErr = rsa.GenerateKey(rand.Reader, 2048)
		}
	})
	if oidcKeyErr != nil {
		t.Fatal(oidcKeyErr)
	}
	return oidcKey, oidcKeyBad
}

// NewFakeOIDC starts a fake IdP accepting the given client credentials.
func NewFakeOIDC(t testing.TB, clientID, clientSecret string, o FakeOIDCOptions) *FakeOIDC {
	good, other := oidcKeys(t)
	f := &FakeOIDC{
		clientID:     clientID,
		clientSecret: clientSecret,
		opts:         o,
		sub:          "alice",
		codes:        map[string]*fakeOIDCCode{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /authorize", f.authorize)
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) { f.token(w, r, good, other) })
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "k1",
			"n": b64url(good.N.Bytes()),
			"e": b64url(big.NewInt(int64(good.E)).Bytes()),
		}}})
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		if r.URL.Path == "/.well-known/openid-configuration" {
			f.discoveries++
		}
		f.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Server.Close)
	return f
}

// Issuer returns the issuer identifier: the server URL, plus "/" when
// IssuerTrailingSlash is set.
func (f *FakeOIDC) Issuer() string {
	if f.opts.IssuerTrailingSlash {
		return f.URL + "/"
	}
	return f.URL
}

// SetSub sets the sub for the next authorization. Default "alice".
func (f *FakeOIDC) SetSub(sub string) { f.set(func() { f.sub = sub }) }

// SetClaims merges c into the ID tokens of the next authorization, over the
// defaults. A nil value deletes the claim. Replaces earlier SetClaims.
func (f *FakeOIDC) SetClaims(c map[string]any) {
	cp := make(map[string]any, len(c))
	for k, v := range c {
		cp[k] = v
	}
	f.set(func() { f.claims = cp })
}

// SetError makes authorize redirect back with ?error=code&state=... instead
// of a code. Empty restores normal behavior.
func (f *FakeOIDC) SetError(code string) { f.set(func() { f.errCode = code }) }

// SetDown makes every endpoint answer 503.
func (f *FakeOIDC) SetDown(down bool) { f.set(func() { f.down = down }) }

// SetOtherKey signs ID tokens with a key that isn't in the JWKS.
func (f *FakeOIDC) SetOtherKey(v bool) { f.set(func() { f.otherKey = v }) }

// SetAlgNone issues unsigned ID tokens with header alg "none".
func (f *FakeOIDC) SetAlgNone(v bool) { f.set(func() { f.algNone = v }) }

// Discoveries returns the number of hits on the discovery document.
func (f *FakeOIDC) Discoveries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discoveries
}

// LastAccessToken returns the access token in the most recent token response.
func (f *FakeOIDC) LastAccessToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAccess
}

// LastAuthorize returns the query of the most recent /authorize request.
func (f *FakeOIDC) LastAuthorize() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := url.Values{}
	for k, v := range f.lastAuth {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (f *FakeOIDC) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *FakeOIDC) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                f.Issuer(),
		"authorization_endpoint":                f.URL + "/authorize",
		"token_endpoint":                        f.URL + "/token",
		"jwks_uri":                              f.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (f *FakeOIDC) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	f.lastAuth = q
	errCode := f.errCode
	f.mu.Unlock()
	if q.Get("client_id") != f.clientID || q.Get("response_type") != "code" {
		http.Error(w, "bad client_id or response_type", http.StatusBadRequest)
		return
	}
	ru, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || ru.Scheme == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := ru.Query()
	rq.Set("state", q.Get("state"))
	if errCode != "" {
		rq.Set("error", errCode)
		ru.RawQuery = rq.Encode()
		http.Redirect(w, r, ru.String(), http.StatusFound)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "PKCE S256 required", http.StatusBadRequest)
		return
	}
	code := randHex()
	f.mu.Lock()
	c := &fakeOIDCCode{
		clientID:    f.clientID,
		redirectURI: q.Get("redirect_uri"),
		challenge:   q.Get("code_challenge"),
		nonce:       q.Get("nonce"),
		sub:         f.sub,
		claims:      map[string]any{},
	}
	for k, v := range f.claims {
		c.claims[k] = v
	}
	f.codes[code] = c
	f.mu.Unlock()
	rq.Set("code", code)
	ru.RawQuery = rq.Encode()
	http.Redirect(w, r, ru.String(), http.StatusFound)
}

func (f *FakeOIDC) token(w http.ResponseWriter, r *http.Request, good, other *rsa.PrivateKey) {
	id, secret, ok := r.BasicAuth()
	if ok {
		// RFC 6749 2.3.1: credentials are form-urlencoded before Basic.
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	}
	if !ok || id != f.clientID || subtle.ConstantTimeCompare([]byte(secret), []byte(f.clientSecret)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	f.mu.Lock()
	c, found := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	otherKey, algNone := f.otherKey, f.algNone
	f.mu.Unlock()
	if !found || c.redirectURI != r.PostForm.Get("redirect_uri") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if b64url(sum[:]) != c.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss":   f.Issuer(),
		"sub":   c.sub,
		"aud":   c.clientID,
		"iat":   now.Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nonce": c.nonce,
	}
	for k, v := range c.claims {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	key, kid := good, "k1"
	if otherKey {
		key, kid = other, "k2"
	}
	idToken, err := signJWT(claims, key, kid, algNone)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	access := "at_" + randHex()
	f.set(func() { f.lastAccess = access })
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   300,
		"id_token":     idToken,
	})
}

func signJWT(claims map[string]any, key *rsa.PrivateKey, kid string, algNone bool) (string, error) {
	hdr := map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid}
	if algNone {
		hdr = map[string]string{"alg": "none", "typ": "JWT"}
	}
	h, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64url(h) + "." + b64url(p)
	if algNone {
		return signing + ".", nil
	}
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64url(sig), nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
