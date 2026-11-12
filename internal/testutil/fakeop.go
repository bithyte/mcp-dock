package testutil

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/interledger/open-payments-go/httpsignatureutils"
)

type fakeGrant struct {
	subID         string
	finishURI     string
	clientNonce   string
	asNonce       string
	interactRef   string
	continueToken string
	authServer    string // as advertised by the wallet doc; hash input
}

// FakeOpenPayments is an in-process stand-in for an Open Payments wallet
// address server and its GNAP authorization server, over TLS. Use its
// embedded server's Client() (or a copy) to talk to it.
type FakeOpenPayments struct {
	*httptest.Server

	gatewayWallet string
	gatewayPub    ed25519.PublicKey

	mu          sync.Mutex
	decline     bool
	badHash     bool
	subject     string
	walletIDs   map[string]string // name -> id override ("" omits id)
	authServers map[string]string // name -> authServer override
	delay       time.Duration
	cancelFail  bool
	contInter   bool   // continue responses still carry interact
	contURI     string // continue responses use this uri when set
	grants      map[string]*fakeGrant
	requests    []string
	grantBodies [][]byte
	cancels     int
}

// NewFakeOpenPayments starts a fake wallet and auth server. Grant requests
// must identify themselves as gatewayWallet and be signed with the key
// matching gatewayPub.
func NewFakeOpenPayments(t testing.TB, gatewayWallet string, gatewayPub ed25519.PublicKey) *FakeOpenPayments {
	f := &FakeOpenPayments{
		gatewayWallet: gatewayWallet,
		gatewayPub:    gatewayPub,
		walletIDs:     map[string]string{},
		authServers:   map[string]string{},
		grants:        map[string]*fakeGrant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{name}", f.walletDoc)
	mux.HandleFunc("POST /auth", f.grantRequest)
	mux.HandleFunc("GET /interact/{id}", f.interact)
	mux.HandleFunc("POST /continue/{id}", f.continueGrant)
	mux.HandleFunc("DELETE /continue/{id}", f.cancelGrant)
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		d := f.delay
		f.mu.Unlock()
		if d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Server.Close)
	return f
}

// Wallet returns the URL of the wallet address called name.
func (f *FakeOpenPayments) Wallet(name string) string { return f.URL + "/" + name }

// AuthServer returns the auth server URL that wallet documents advertise.
func (f *FakeOpenPayments) AuthServer() string { return f.URL + "/auth" }

// SetDecline makes the interaction redirect back with result=grant_rejected.
func (f *FakeOpenPayments) SetDecline(v bool) { f.set(func() { f.decline = v }) }

// SetBadHash makes the interaction redirect back with a wrong hash.
func (f *FakeOpenPayments) SetBadHash(v bool) { f.set(func() { f.badHash = v }) }

// SetSubject overrides the subject id returned by grant continuation.
// Empty restores the default (the requested id).
func (f *FakeOpenPayments) SetSubject(id string) { f.set(func() { f.subject = id }) }

// SetWalletID overrides the id in the document of wallet name. An empty id
// makes the document omit it.
func (f *FakeOpenPayments) SetWalletID(name, id string) { f.set(func() { f.walletIDs[name] = id }) }

// SetAuthServer overrides the authServer in the document of wallet name.
func (f *FakeOpenPayments) SetAuthServer(name, u string) { f.set(func() { f.authServers[name] = u }) }

// SetDelay delays every response by d (or until the client gives up).
func (f *FakeOpenPayments) SetDelay(d time.Duration) { f.set(func() { f.delay = d }) }

// SetCancelFail makes grant cancellation answer 500.
func (f *FakeOpenPayments) SetCancelFail(v bool) { f.set(func() { f.cancelFail = v }) }

// SetContinueInteract makes grant continuation answer with an interact
// block, as if the grant were not finalized.
func (f *FakeOpenPayments) SetContinueInteract(v bool) { f.set(func() { f.contInter = v }) }

// SetContinueURI makes grant continuation return u as the continue uri.
// Empty restores the default.
func (f *FakeOpenPayments) SetContinueURI(u string) { f.set(func() { f.contURI = u }) }

// Requests returns the "METHOD /path" log of every request received.
func (f *FakeOpenPayments) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// GrantBodies returns the raw bodies of every grant request received.
func (f *FakeOpenPayments) GrantBodies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.grantBodies...)
}

// Cancels returns how many grants were cancelled successfully.
func (f *FakeOpenPayments) Cancels() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}

func (f *FakeOpenPayments) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// walletID returns the effective id of wallet name. Callers hold f.mu.
func (f *FakeOpenPayments) walletID(name string) (string, bool) {
	if id, ok := f.walletIDs[name]; ok {
		return id, id != ""
	}
	return f.Wallet(name), true
}

// knownID reports whether id is the effective id of some wallet. Callers
// hold f.mu.
func (f *FakeOpenPayments) knownID(id string) bool {
	for _, v := range f.walletIDs {
		if v != "" && v == id {
			return true
		}
	}
	name, ok := strings.CutPrefix(id, f.URL+"/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return false
	}
	_, overridden := f.walletIDs[name]
	return !overridden
}

// authServerFor returns the authServer that the document of the wallet
// with id advertises. Callers hold f.mu.
func (f *FakeOpenPayments) authServerFor(id string) string {
	name := ""
	for n, v := range f.walletIDs {
		if v != "" && v == id {
			name = n
			break
		}
	}
	if name == "" {
		name, _ = strings.CutPrefix(id, f.URL+"/")
	}
	if as, ok := f.authServers[name]; ok {
		return as
	}
	return f.AuthServer()
}

func (f *FakeOpenPayments) walletDoc(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	f.mu.Lock()
	id, hasID := f.walletID(name)
	as, ok := f.authServers[name]
	f.mu.Unlock()
	if !ok {
		as = f.AuthServer()
	}
	doc := map[string]any{
		"authServer":     as,
		"resourceServer": f.URL + "/rs",
		"assetCode":      "USD",
		"assetScale":     2,
		"publicName":     name,
	}
	if hasID {
		doc["id"] = id
	}
	writeJSON(w, http.StatusOK, doc)
}

func (f *FakeOpenPayments) grantRequest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.grantBodies = append(f.grantBodies, body)
	f.mu.Unlock()
	if err := f.verifySig(r, body); err != nil {
		http.Error(w, "signature: "+err.Error(), http.StatusUnauthorized)
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if _, ok := raw["access_token"]; ok {
		http.Error(w, "access_token must not be requested", http.StatusBadRequest)
		return
	}
	if c := clientWallet(raw["client"]); c != f.gatewayWallet {
		http.Error(w, "bad client", http.StatusBadRequest)
		return
	}
	var req struct {
		Subject struct {
			SubIDs []struct {
				ID     string `json:"id"`
				Format string `json:"format"`
			} `json:"sub_ids"`
		} `json:"subject"`
		Interact struct {
			Start  []string `json:"start"`
			Finish struct {
				Method string `json:"method"`
				URI    string `json:"uri"`
				Nonce  string `json:"nonce"`
			} `json:"finish"`
		} `json:"interact"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	subs := req.Subject.SubIDs
	fin := req.Interact.Finish
	if len(subs) == 0 || subs[0].Format != "uri" {
		http.Error(w, "bad subject", http.StatusBadRequest)
		return
	}
	if len(req.Interact.Start) != 1 || req.Interact.Start[0] != "redirect" {
		http.Error(w, "bad interact.start", http.StatusBadRequest)
		return
	}
	if fin.Method != "redirect" || fin.Nonce == "" {
		http.Error(w, "bad interact.finish", http.StatusBadRequest)
		return
	}
	if u, err := url.Parse(fin.URI); err != nil || u.Scheme == "" || u.Host == "" {
		http.Error(w, "bad interact.finish.uri", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	known := f.knownID(subs[0].ID)
	authServer := f.authServerFor(subs[0].ID)
	f.mu.Unlock()
	if !known {
		http.Error(w, "unknown subject", http.StatusBadRequest)
		return
	}
	id := randHex()
	g := &fakeGrant{
		subID:         subs[0].ID,
		finishURI:     fin.URI,
		clientNonce:   fin.Nonce,
		asNonce:       randHex(),
		authServer:    authServer,
		continueToken: randHex(),
	}
	f.mu.Lock()
	f.grants[id] = g
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"interact": map[string]any{
			"redirect": f.URL + "/interact/" + id,
			"finish":   g.asNonce,
		},
		"continue": f.continueObj(id, g.continueToken),
	})
}

func (f *FakeOpenPayments) interact(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	g, ok := f.grants[r.PathValue("id")]
	decline, badHash := f.decline, f.badHash
	var finishURI, hash, ref string
	if ok {
		finishURI = g.finishURI
		if !decline {
			g.interactRef = randHex()
			ref = g.interactRef
			hash = FakeOPHash(g.clientNonce, g.asNonce, ref, g.authServer)
			if badHash {
				hash = FakeOPHash(g.clientNonce, g.asNonce, ref+"x", g.authServer)
			}
		}
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unknown grant", http.StatusNotFound)
		return
	}
	u, _ := url.Parse(finishURI)
	q := u.Query()
	if decline {
		q.Set("result", "grant_rejected")
	} else {
		q.Set("interact_ref", ref)
		q.Set("hash", hash)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *FakeOpenPayments) continueGrant(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := f.verifySig(r, body); err != nil {
		http.Error(w, "signature: "+err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		InteractRef string `json:"interact_ref"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	f.mu.Lock()
	g, ok := f.grants[id]
	var sub, tok, contURI string
	var contInter bool
	if ok && r.Header.Get("Authorization") == "GNAP "+g.continueToken &&
		g.interactRef != "" && req.InteractRef == g.interactRef {
		sub = g.subID
		if f.subject != "" {
			sub = f.subject
		}
		g.continueToken = randHex()
		g.interactRef = "" // single use
		tok = g.continueToken
		contURI, contInter = f.contURI, f.contInter
	} else {
		ok = false
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, "invalid continuation", http.StatusUnauthorized)
		return
	}
	resp := map[string]any{
		"subject": map[string]any{
			"sub_ids": []map[string]string{{"id": sub, "format": "uri"}},
		},
		"continue": f.continueObj(id, tok),
	}
	if contURI != "" {
		resp["continue"].(map[string]any)["uri"] = contURI
	}
	if contInter {
		resp["interact"] = map[string]any{"redirect": f.URL + "/interact/" + id, "finish": randHex()}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (f *FakeOpenPayments) cancelGrant(w http.ResponseWriter, r *http.Request) {
	if err := f.verifySig(r, nil); err != nil {
		http.Error(w, "signature: "+err.Error(), http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.grants[id]
	if !ok || r.Header.Get("Authorization") != "GNAP "+g.continueToken {
		http.Error(w, "invalid continuation", http.StatusUnauthorized)
		return
	}
	if f.cancelFail {
		http.Error(w, "cancel failed", http.StatusInternalServerError)
		return
	}
	delete(f.grants, id)
	f.cancels++
	w.WriteHeader(http.StatusNoContent)
}

func (f *FakeOpenPayments) continueObj(id, token string) map[string]any {
	return map[string]any{
		"access_token": map[string]string{"value": token},
		"uri":          f.URL + "/continue/" + id,
	}
}

// verifySig checks the request's HTTP message signature against the
// gateway's public key, and that the signature covers the body and the
// Authorization header when present.
func (f *FakeOpenPayments) verifySig(r *http.Request, body []byte) error {
	sig, input := r.Header.Get("Signature"), r.Header.Get("Signature-Input")
	if sig == "" || input == "" {
		return errors.New("missing signature headers")
	}
	inner, ok := strings.CutPrefix(sig, "sig1=:")
	if ok {
		inner, ok = strings.CutSuffix(inner, ":")
	}
	if !ok {
		return errors.New("malformed signature header")
	}
	need := []string{`"@method"`, `"@target-uri"`}
	if r.Header.Get("Authorization") != "" {
		need = append(need, `"authorization"`)
	}
	if len(body) > 0 {
		need = append(need, `"content-digest"`, `"content-length"`, `"content-type"`)
		if r.Header.Get("Content-Digest") != httpsignatureutils.CreateContentHeaders(body).ContentDigest {
			return errors.New("content digest mismatch")
		}
	}
	for _, c := range need {
		if !strings.Contains(input, c) {
			return fmt.Errorf("signature does not cover %s", c)
		}
	}
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host = "https", r.Host
	r2.URL = &u
	if len(body) > 0 {
		r2.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	h := r.Header.Clone()
	h.Set("Signature", inner)
	return httpsignatureutils.ValidateSignature(httpsignatureutils.NewValidationOptions(r2, h, f.gatewayPub))
}

// clientWallet extracts the client wallet address from either the object
// form {"walletAddress": ...} or the deprecated string form.
func clientWallet(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		WalletAddress string `json:"walletAddress"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.WalletAddress
	}
	return ""
}

// FakeOPHash computes the interaction finish hash the way the fake does:
// unpadded base64url of sha256 over the newline-joined inputs.
func FakeOPHash(clientNonce, finishNonce, interactRef, authServer string) string {
	sum := sha256.Sum256([]byte(clientNonce + "\n" + finishNonce + "\n" + interactRef + "\n" + authServer))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
