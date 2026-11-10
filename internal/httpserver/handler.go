// Package httpserver wires the oauth endpoints, /healthz and the
// authenticated MCP endpoint into one http.Handler.
package httpserver

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bithyte/mcp-dock/internal/gateway"
	"github.com/bithyte/mcp-dock/internal/oauth"
)

// sessionTimeout closes MCP sessions that got no POST for this long, so
// sessions that clients abandon without a DELETE don't pile up forever.
// In-flight calls pause the timer. A client that comes back later gets a
// 404 and, per the spec, starts a new session.
const sessionTimeout = 24 * time.Hour

// NewHandler returns the gateway's root handler.
func NewHandler(oauthSrv *oauth.Server, gw *gateway.Gateway) http.Handler {
	mux := http.NewServeMux()
	oauthSrv.Routes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok"))
	})

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return gw.Server() },
		&mcp.StreamableHTTPOptions{
			// Replaced by hostCheck, which also lets the public host through
			// for a reverse proxy connecting over loopback.
			DisableLocalhostProtection: true,
			SessionTimeout:             sessionTimeout,
		})
	requireAuth := auth.RequireBearerToken(oauthSrv.Verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: oauthSrv.ResourceMetadataURL(),
	})
	// The metadata URL is <public_url>/.well-known/..., so its host is the
	// public host.
	pub, _ := url.Parse(oauthSrv.ResourceMetadataURL())
	mux.Handle("/mcp", hostCheck(pub, requireAuth(mcpHandler)))
	return mux
}

// hostCheck is DNS rebinding protection. A request that arrived on a
// loopback address must carry a loopback Host or the public_url host; a
// rebound attacker page sends its own domain. This is the SDK's check
// (StreamableHTTPOptions.DisableLocalhostProtection) plus the public host,
// which a reverse proxy on the same machine forwards over loopback.
func hostCheck(public *url.URL, h http.Handler) http.Handler {
	want := canonicalHost(public.Host, public.Scheme)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && local != nil &&
			isLoopback(local.String()) && !isLoopback(r.Host) && canonicalHost(r.Host, public.Scheme) != want {
			http.Error(w, "Forbidden: invalid Host header", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// canonicalHost lowercases host and adds the scheme's default port.
func canonicalHost(host, scheme string) string {
	host = strings.ToLower(host)
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	port := "80"
	if scheme == "https" {
		port = "443"
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

// isLoopback reports whether addr (host or host:port) is localhost or a
// loopback IP.
func isLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
