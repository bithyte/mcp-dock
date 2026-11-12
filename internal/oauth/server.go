// Package oauth is the gateway's OAuth 2.1 authorization server. It issues its
// own tokens to MCP clients and delegates user login to GitHub.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/bithyte/mcp-dock/internal/config"
	"github.com/bithyte/mcp-dock/internal/github"
	"github.com/bithyte/mcp-dock/internal/store"
)

// maxBodyBytes caps every oauth POST body.
const maxBodyBytes = 64 << 10

type Options struct {
	Config *config.Config
	Store  *store.Store
	GitHub *github.Client // nil when GitHub login is not configured
	Logger *slog.Logger
	Now    func() time.Time // default time.Now
}

type Server struct {
	cfg   *config.Config
	store *store.Store
	gh    *github.Client
	log   *slog.Logger
	now   func() time.Time
}

func New(opts Options) *Server {
	s := &Server{cfg: opts.Config, store: opts.Store, gh: opts.GitHub, log: opts.Logger, now: opts.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Routes registers every oauth path on mux. Every POST goes through
// limitBody. The consent POST is /oauth/consent (not /consent) so the
// mcpdock_auth cookie, scoped to /oauth/, is sent with it.
func (s *Server) Routes(mux *http.ServeMux) {
	prm := cors(http.HandlerFunc(s.handleProtectedResource))
	mux.Handle("GET /.well-known/oauth-protected-resource", prm)
	mux.Handle("OPTIONS /.well-known/oauth-protected-resource", prm)
	mux.Handle("GET /.well-known/oauth-protected-resource/mcp", prm)
	mux.Handle("OPTIONS /.well-known/oauth-protected-resource/mcp", prm)
	asm := cors(http.HandlerFunc(s.handleAuthServer))
	mux.Handle("GET /.well-known/oauth-authorization-server", asm)
	mux.Handle("OPTIONS /.well-known/oauth-authorization-server", asm)

	reg := cors(limitBody(http.HandlerFunc(s.handleRegister)))
	mux.Handle("POST /register", reg)
	mux.Handle("OPTIONS /register", reg)

	mux.Handle("GET /authorize", http.HandlerFunc(s.handleAuthorize))
	if s.gh != nil {
		mux.Handle("GET "+callbackPath, http.HandlerFunc(s.handleGitHubCallback))
	}
	mux.Handle("POST "+consentPath, limitBody(http.HandlerFunc(s.handleConsent)))
	tok := cors(limitBody(http.HandlerFunc(s.handleToken)))
	mux.Handle("POST /token", tok)
	mux.Handle("OPTIONS /token", tok)
}

// ResourceMetadataURL is the RFC 9728 metadata URL for the /mcp resource,
// used in WWW-Authenticate challenges.
func (s *Server) ResourceMetadataURL() string {
	return s.cfg.PublicURL + "/.well-known/oauth-protected-resource/mcp"
}

// newSecret returns 32 random bytes, base64url without padding.
func newSecret() string {
	b := make([]byte, 32)
	rand.Read(b) // crypto/rand.Read never fails (Go 1.24+); it crashes the program instead.
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashSecret is the hex SHA-256 used to store secrets, codes and tokens.
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// limitBody caps the request body at maxBodyBytes. Reads past the cap fail
// with *http.MaxBytesError (see isTooLarge). Wrap every oauth POST in it.
func limitBody(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		h.ServeHTTP(w, r)
	})
}

// isTooLarge reports whether err came from limitBody's cap.
func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// cors allows cross-origin use from browser-based clients (e.g. MCP
// Inspector). These endpoints use no cookies, so a wildcard origin is safe.
// OPTIONS preflights are answered here.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// writeJSON writes v as JSON with Cache-Control: no-store.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}
	writeRawJSON(w, status, b)
}

func writeRawJSON(w http.ResponseWriter, status int, b []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	w.Write(b)
}

// writeError writes an OAuth error body {"error":..., "error_description":...}.
func writeError(w http.ResponseWriter, status int, code, desc string) {
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeJSON(w, status, body)
}
