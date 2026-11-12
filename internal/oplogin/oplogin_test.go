package oplogin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bithyte/mcp-dock/internal/testutil"
)

const (
	gatewayWallet = "https://gateway.example/mcp"
	finishURI     = "https://gateway.example/oauth/open-payments/callback?state=s1"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newClient(t *testing.T, hc *http.Client, pemText string) (*Client, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	c, err := New(Config{
		WalletAddress: gatewayWallet,
		KeyID:         "key-1",
		PrivateKey:    pemText,
		HTTP:          hc,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, logs
}

func setup(t *testing.T) (*Client, *testutil.FakeOpenPayments, *syncBuf) {
	t.Helper()
	pemText, pub := testutil.Ed25519PEM(t)
	f := testutil.NewFakeOpenPayments(t, gatewayWallet, pub)
	c, logs := newClient(t, f.Client(), pemText)
	return c, f, logs
}

// interact follows the wallet's interaction redirect once, without following
// the redirect back, and returns the Location query.
func interact(t *testing.T, f *testutil.FakeOpenPayments, redirectURL string) url.Values {
	t.Helper()
	hc := *f.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Get(redirectURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("interact status = %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc.String(), "https://gateway.example/oauth/open-payments/callback?") {
		t.Fatalf("Location = %q", loc)
	}
	return loc.Query()
}

func start(t *testing.T, c *Client, f *testutil.FakeOpenPayments, name string) (Wallet, Pending) {
	t.Helper()
	ctx := context.Background()
	w, err := c.Wallet(ctx, f.Wallet(name))
	if err != nil {
		t.Fatalf("Wallet: %v", err)
	}
	p, err := c.Start(ctx, w, finishURI)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return w, p
}

func TestLoginHappyPath(t *testing.T) {
	c, f, logs := setup(t)
	w, p := start(t, c, f, "alice")
	if w.ID != f.Wallet("alice") || w.AuthServer != f.AuthServer() {
		t.Fatalf("wallet = %+v", w)
	}
	if p.WalletID != w.ID || p.AuthServer != w.AuthServer || p.ClientNonce == "" || p.FinishNonce == "" ||
		p.ContinueURI == "" || p.ContinueToken == "" || !strings.HasPrefix(p.RedirectURL, f.URL+"/interact/") {
		t.Fatalf("pending = %+v", p)
	}
	q := interact(t, f, p.RedirectURL)
	if q.Get("state") != "s1" {
		t.Fatalf("finish URI query lost: %v", q)
	}
	ref, hash := q.Get("interact_ref"), q.Get("hash")
	if err := c.Finish(context.Background(), p, ref, hash); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if n := f.Cancels(); n != 1 {
		t.Fatalf("Cancels = %d, want 1", n)
	}
	for _, s := range []string{p.ContinueToken, p.ClientNonce, p.FinishNonce, ref, hash} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("logs leak a secret: %s", logs.String())
		}
	}
}

func TestStartNeverRequestsAccessToken(t *testing.T) {
	c, f, _ := setup(t)
	start(t, c, f, "alice")
	bodies := f.GrantBodies()
	if len(bodies) != 1 {
		t.Fatalf("grant bodies = %d", len(bodies))
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(bodies[0], &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["access_token"]; ok {
		t.Fatalf("grant request asks for access_token: %s", bodies[0])
	}
	if _, ok := m["subject"]; !ok {
		t.Fatalf("grant request has no subject: %s", bodies[0])
	}
}

func TestFinishBadHash(t *testing.T) {
	c, f, _ := setup(t)
	f.SetBadHash(true)
	_, p := start(t, c, f, "alice")
	q := interact(t, f, p.RedirectURL)
	err := c.Finish(context.Background(), p, q.Get("interact_ref"), q.Get("hash"))
	if !errors.Is(err, ErrBadHash) {
		t.Fatalf("err = %v, want ErrBadHash", err)
	}
	for _, r := range f.Requests() {
		if strings.HasPrefix(r, "POST /continue/") {
			t.Fatalf("continuation was attempted: %v", f.Requests())
		}
	}
}

func TestFinishSubjectMismatch(t *testing.T) {
	c, f, _ := setup(t)
	f.SetSubject(f.Wallet("mallory"))
	_, p := start(t, c, f, "alice")
	q := interact(t, f, p.RedirectURL)
	err := c.Finish(context.Background(), p, q.Get("interact_ref"), q.Get("hash"))
	if !errors.Is(err, ErrSubjectMismatch) {
		t.Fatalf("err = %v, want ErrSubjectMismatch", err)
	}
	if n := f.Cancels(); n != 1 {
		t.Fatalf("Cancels = %d, want 1", n)
	}
}

func TestFinishRejectsNotFinalized(t *testing.T) {
	c, f, _ := setup(t)
	f.SetContinueInteract(true)
	_, p := start(t, c, f, "alice")
	q := interact(t, f, p.RedirectURL)
	err := c.Finish(context.Background(), p, q.Get("interact_ref"), q.Get("hash"))
	if err == nil {
		t.Fatal("Finish accepted a continue result that still needs interaction")
	}
	if errors.Is(err, ErrSubjectMismatch) || errors.Is(err, ErrBadHash) {
		t.Fatalf("err = %v, want a not-finalized error", err)
	}
}

func TestFinishSkipsCancelOnHTTPContinueURI(t *testing.T) {
	c, f, logs := setup(t)
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer plain.Close()
	f.SetContinueURI(plain.URL + "/continue/1")
	_, p := start(t, c, f, "alice")
	q := interact(t, f, p.RedirectURL)
	if err := c.Finish(context.Background(), p, q.Get("interact_ref"), q.Get("hash")); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if hits.Load() != 0 || f.Cancels() != 0 {
		t.Fatalf("cancel sent over http: hits=%d cancels=%d", hits.Load(), f.Cancels())
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("no warning logged: %q", logs.String())
	}
}

func TestFinishUsesAdvertisedAuthServerInHash(t *testing.T) {
	c, f, _ := setup(t)
	as := f.AuthServer() + "?tenant=1" // still routed to the fake's /auth
	f.SetAuthServer("alice", as)
	w, p := start(t, c, f, "alice")
	if w.AuthServer != as {
		t.Fatalf("AuthServer = %q", w.AuthServer)
	}
	q := interact(t, f, p.RedirectURL)
	if err := c.Finish(context.Background(), p, q.Get("interact_ref"), q.Get("hash")); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func TestFinishCancelHangDoesNotDelay(t *testing.T) {
	old := cancelTimeout
	cancelTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cancelTimeout = old })
	c, f, logs := setup(t)
	f.SetCancelDelay(5 * time.Second)
	_, p := start(t, c, f, "alice")
	q := interact(t, f, p.RedirectURL)
	t0 := time.Now()
	if err := c.Finish(context.Background(), p, q.Get("interact_ref"), q.Get("hash")); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("Finish took %v with a hung cancel", d)
	}
	if !strings.Contains(logs.String(), "grant cancel failed") {
		t.Fatalf("no cancel warning: %q", logs.String())
	}
}

func TestFinishCancelFailureIgnored(t *testing.T) {
	c, f, logs := setup(t)
	f.SetCancelFail(true)
	_, p := start(t, c, f, "alice")
	q := interact(t, f, p.RedirectURL)
	ref, hash := q.Get("interact_ref"), q.Get("hash")
	if err := c.Finish(context.Background(), p, ref, hash); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if f.Cancels() != 0 {
		t.Fatalf("Cancels = %d", f.Cancels())
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "500") {
		t.Fatalf("no cancel warning logged: %q", out)
	}
	for _, s := range []string{p.ContinueToken, p.ClientNonce, p.FinishNonce, ref, hash, "/continue/"} {
		if strings.Contains(out, s) {
			t.Fatalf("log leaks %q: %s", s, out)
		}
	}
}

func TestWalletRejectsHTTPAuthServer(t *testing.T) {
	c, f, _ := setup(t)
	f.SetAuthServer("alice", "http://127.0.0.1:1/auth")
	if _, err := c.Wallet(context.Background(), f.Wallet("alice")); err == nil {
		t.Fatal("Wallet accepted an http authServer")
	}
}

func TestWalletMissingID(t *testing.T) {
	c, f, _ := setup(t)
	f.SetWalletID("alice", "")
	if _, err := c.Wallet(context.Background(), f.Wallet("alice")); err == nil {
		t.Fatal("Wallet accepted a document without id")
	}
}

func TestNoRedirectFollow(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"id":"x","authServer":"https://x/auth"}`))
	}))
	defer other.Close()
	src := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/alice", http.StatusFound)
	}))
	defer src.Close()
	pemText, _ := testutil.Ed25519PEM(t)
	c, _ := newClient(t, src.Client(), pemText)
	if _, err := c.Wallet(context.Background(), src.URL+"/alice"); err == nil {
		t.Fatal("Wallet followed or accepted a redirect")
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target saw %d requests", hits.Load())
	}
}

func TestBodyCap(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"https://x/alice","authServer":"https://x/auth","publicName":"`))
		w.Write(bytes.Repeat([]byte("a"), 1<<20+10))
		w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	pemText, _ := testutil.Ed25519PEM(t)
	c, _ := newClient(t, srv.Client(), pemText)
	if _, err := c.Wallet(context.Background(), srv.URL+"/alice"); err == nil {
		t.Fatal("Wallet accepted a document over 1 MiB")
	}
}

func TestTimeout(t *testing.T) {
	pemText, pub := testutil.Ed25519PEM(t)
	f := testutil.NewFakeOpenPayments(t, gatewayWallet, pub)
	f.SetDelay(2 * time.Second)
	hc := *f.Client()
	hc.Timeout = 200 * time.Millisecond
	c, _ := newClient(t, &hc, pemText)
	t0 := time.Now()
	_, err := c.Wallet(context.Background(), f.Wallet("alice"))
	if err == nil {
		t.Fatal("Wallet succeeded despite delay")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("Wallet took %v", d)
	}
}

func TestDefaultTimeout(t *testing.T) {
	if got := hardened(nil).Timeout; got != 15*time.Second {
		t.Fatalf("default timeout = %v", got)
	}
	if got := hardened(&http.Client{}).Timeout; got != 15*time.Second {
		t.Fatalf("zero timeout not defaulted: %v", got)
	}
	base := &http.Client{Timeout: time.Second}
	h := hardened(base)
	if h.Timeout != time.Second || base.CheckRedirect != nil || base.Transport != nil {
		t.Fatalf("hardened mutated or ignored base: %+v", base)
	}
}

func TestErrorsSanitized(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":"invalid_request","description":"secret-body ` +
			r.Header.Get("Authorization") + `"}}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	pemText, _ := testutil.Ed25519PEM(t)
	c, _ := newClient(t, srv.Client(), pemText)
	ctx := context.Background()

	_, err := c.Start(ctx, Wallet{ID: srv.URL + "/alice", AuthServer: srv.URL + "/auth"}, finishURI)
	if err == nil {
		t.Fatal("Start succeeded on 400")
	}
	if msg := err.Error(); !strings.Contains(msg, host) || !strings.Contains(msg, "400") ||
		strings.Contains(msg, "secret-body") || strings.Contains(msg, "/auth") {
		t.Fatalf("Start error not sanitized: %q", msg)
	}

	p := Pending{
		WalletID: srv.URL + "/alice", AuthServer: srv.URL + "/auth",
		ClientNonce: "cn-secret", FinishNonce: "fn-secret",
		ContinueURI: srv.URL + "/continue/1", ContinueToken: "tok-secret-123",
	}
	ref := "ref-secret-456"
	hash := testutil.FakeOPHash(p.ClientNonce, p.FinishNonce, ref, p.AuthServer)
	err = c.Finish(ctx, p, ref, hash)
	if err == nil {
		t.Fatal("Finish succeeded on 400")
	}
	msg := err.Error()
	if !strings.Contains(msg, host) || !strings.Contains(msg, "400") {
		t.Fatalf("Finish error lacks host or status: %q", msg)
	}
	for _, s := range []string{"secret-body", "tok-secret", "ref-secret", "cn-secret", "fn-secret", hash, "/continue/"} {
		if strings.Contains(msg, s) {
			t.Fatalf("Finish error leaks %q: %q", s, msg)
		}
	}

	// Transport errors keep method and host but drop the path.
	srv.Close()
	err = c.Finish(ctx, p, ref, hash)
	if err == nil || strings.Contains(err.Error(), "/continue/") || strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("transport error not sanitized: %v", err)
	}
}

func TestStartNotPending(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"continue":{"access_token":{"value":"t"},"uri":"https://x/continue/1"}}`))
	}))
	defer srv.Close()
	pemText, _ := testutil.Ed25519PEM(t)
	c, _ := newClient(t, srv.Client(), pemText)
	_, err := c.Start(context.Background(), Wallet{ID: srv.URL + "/alice", AuthServer: srv.URL + "/auth"}, finishURI)
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}
}

func TestNewKeyForms(t *testing.T) {
	pemText, pub := testutil.Ed25519PEM(t)
	f := testutil.NewFakeOpenPayments(t, gatewayWallet, pub)
	c, _ := newClient(t, f.Client(), base64.StdEncoding.EncodeToString([]byte(pemText)))
	start(t, c, f, "alice") // the fake verifies the signature

	_, err := New(Config{WalletAddress: gatewayWallet, KeyID: "k", PrivateKey: "not-a-key-secretish"})
	if err == nil || strings.Contains(err.Error(), "secretish") {
		t.Fatalf("bad key err = %v", err)
	}
	if _, err := New(Config{WalletAddress: gatewayWallet, PrivateKey: pemText}); err == nil {
		t.Fatal("New accepted an empty key id")
	}
}

// The happy path relies on the fake's request checks; make sure a request
// signed with the wrong key is refused.
func TestFakeRejectsWrongKey(t *testing.T) {
	_, pub := testutil.Ed25519PEM(t)
	otherPEM, _ := testutil.Ed25519PEM(t)
	f := testutil.NewFakeOpenPayments(t, gatewayWallet, pub)
	c, _ := newClient(t, f.Client(), otherPEM)
	w, err := c.Wallet(context.Background(), f.Wallet("alice"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Start(context.Background(), w, finishURI)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401", err)
	}
}

func TestFakeRejectsWrongClient(t *testing.T) {
	pemText, pub := testutil.Ed25519PEM(t)
	f := testutil.NewFakeOpenPayments(t, "https://someone-else.example/w", pub)
	c, _ := newClient(t, f.Client(), pemText)
	w, err := c.Wallet(context.Background(), f.Wallet("alice"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Start(context.Background(), w, finishURI)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want 400", err)
	}
}
