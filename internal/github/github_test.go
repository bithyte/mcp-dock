package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bithyte/mcp-dock/internal/github"
	"github.com/bithyte/mcp-dock/internal/testutil"
)

func newClient(f *testutil.FakeGitHub) *github.Client {
	return &github.Client{ClientID: "cid", ClientSecret: "s3cret-value", WebBaseURL: f.URL, APIBaseURL: f.URL}
}

func TestFlowAgainstFake(t *testing.T) {
	f := testutil.NewFakeGitHub(t, "cid", "s3cret-value")
	f.SetUser("octo", 42)
	c := newClient(f)

	au := c.AuthorizeURL("st ate&x", "http://localhost:1234/cb?a=b")
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(au)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Query().Get("state") != "st ate&x" || loc.Query().Get("code") == "" {
		t.Fatalf("bad location %s", loc)
	}
	if loc.Host != "localhost:1234" || loc.Query().Get("a") != "b" {
		t.Fatalf("bad location %s", loc)
	}
	tok, err := c.Exchange(context.Background(), loc.Query().Get("code"), "http://localhost:1234/cb?a=b")
	if err != nil || tok == "" {
		t.Fatalf("exchange: %q %v", tok, err)
	}
	u, err := c.User(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if u.Login != "octo" || u.ID != 42 {
		t.Fatalf("user %+v", u)
	}
}

func TestExchangeBadCode(t *testing.T) {
	f := testutil.NewFakeGitHub(t, "cid", "s3cret-value")
	c := newClient(f)
	_, err := c.Exchange(context.Background(), "nope", "http://localhost/cb")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Fatalf("error leaks secret: %v", err)
	}
}

func TestExchangeBadSecretNoLeak(t *testing.T) {
	f := testutil.NewFakeGitHub(t, "cid", "s3cret-value")
	c := newClient(f)
	c.ClientSecret = "wrong-secret"
	_, err := c.Exchange(context.Background(), "x", "http://localhost/cb")
	if err == nil || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("err %v", err)
	}
}

func TestUserBadToken(t *testing.T) {
	f := testutil.NewFakeGitHub(t, "cid", "s3cret-value")
	c := newClient(f)
	if _, err := c.User(context.Background(), "bogus-token"); err == nil {
		t.Fatal("want error")
	}
}

func TestUserRejectsEmptyLoginOrZeroID(t *testing.T) {
	for _, body := range []string{`{"login":"","id":5}`, `{"login":"a","id":0}`, `{}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))
		c := &github.Client{APIBaseURL: srv.URL}
		if _, err := c.User(context.Background(), "tok-abc"); err == nil {
			t.Errorf("body %s: want error", body)
		}
		srv.Close()
	}
}

func TestExchangeEmptyTokenAndTokenNotInError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":""}`))
	}))
	defer srv.Close()
	c := &github.Client{WebBaseURL: srv.URL}
	if _, err := c.Exchange(context.Background(), "c", "r"); err == nil {
		t.Fatal("want error")
	}
	// Non-200 on /user must not echo the token.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad "+r.Header.Get("Authorization"), 401)
	}))
	defer srv2.Close()
	c2 := &github.Client{APIBaseURL: srv2.URL}
	_, err := c2.User(context.Background(), "tok-abc")
	if err == nil || strings.Contains(err.Error(), "tok-abc") {
		t.Fatalf("err %v", err)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":"a","id":1,"pad":"` + strings.Repeat("x", 2<<20) + `"}`))
	}))
	defer srv.Close()
	c := &github.Client{APIBaseURL: srv.URL}
	if _, err := c.User(context.Background(), "t"); err == nil {
		t.Fatal("want error")
	}
}

func TestSetUserConcurrent(t *testing.T) {
	f := testutil.NewFakeGitHub(t, "cid", "s")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.SetUser("u", int64(i+1))
			http.Get(f.URL + "/user")
		}(i)
	}
	wg.Wait()
}
