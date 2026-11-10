package testutil

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type fakeUser struct {
	login string
	id    int64
}

// FakeGitHub is an in-process stand-in for GitHub's OAuth web flow and /user.
type FakeGitHub struct {
	*httptest.Server
	// SetUser sets the user returned for the next authorization. Safe for
	// concurrent use.
	SetUser func(login string, id int64)

	mu     sync.Mutex
	next   fakeUser
	codes  map[string]fakeUser // code -> user
	tokens map[string]fakeUser // token -> user
}

func randHex() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewFakeGitHub starts a fake GitHub accepting the given client credentials.
func NewFakeGitHub(t testing.TB, clientID, clientSecret string) *FakeGitHub {
	f := &FakeGitHub{
		next:   fakeUser{"octo", 1},
		codes:  map[string]fakeUser{},
		tokens: map[string]fakeUser{},
	}
	f.SetUser = func(login string, id int64) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.next = fakeUser{login, id}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != clientID {
			http.Error(w, "bad client", http.StatusBadRequest)
			return
		}
		ru, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || ru.Scheme == "" {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		code := randHex()
		f.mu.Lock()
		f.codes[code] = f.next
		f.mu.Unlock()
		rq := ru.Query()
		rq.Set("code", code)
		rq.Set("state", q.Get("state"))
		ru.RawQuery = rq.Encode()
		http.Redirect(w, r, ru.String(), http.StatusFound)
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("client_id") != clientID || r.PostForm.Get("client_secret") != clientSecret {
			json.NewEncoder(w).Encode(map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		f.mu.Lock()
		u, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		var tok string
		if ok {
			tok = "gho_" + randHex()
			f.tokens[tok] = u
		}
		f.mu.Unlock()
		if !ok {
			json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": tok, "token_type": "bearer"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		u, found := f.tokens[tok]
		f.mu.Unlock()
		if !ok || !found {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"login": u.login, "id": u.id})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}
