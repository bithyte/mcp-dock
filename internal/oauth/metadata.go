package oauth

import (
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// protectedResourceMetadata is the RFC 9728 document, served at both the
// root and the /mcp-suffixed well-known path.
func (s *Server) protectedResourceMetadata() *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource:               s.cfg.ResourceURL(),
		AuthorizationServers:   []string{s.cfg.PublicURL},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "mcp-dock",
	}
}

// authServerMetadata is the RFC 8414 document. public_url always comes from
// config, never from the request.
func (s *Server) authServerMetadata() *oauthex.AuthServerMeta {
	base := s.cfg.PublicURL
	return &oauthex.AuthServerMeta{
		Issuer:                            base,
		AuthorizationEndpoint:             base + "/authorize",
		TokenEndpoint:                     base + "/token",
		RegistrationEndpoint:              base + "/register",
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none", "client_secret_post", "client_secret_basic"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		// The authorization response carries iss (RFC 9207).
		AuthorizationResponseIssParameterSupported: true,
	}
}

func (s *Server) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.protectedResourceMetadata())
}

func (s *Server) handleAuthServer(w http.ResponseWriter, r *http.Request) {
	b, err := marshalWithout(s.authServerMetadata(), "jwks_uri")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	writeRawJSON(w, http.StatusOK, b)
}

// marshalWithout marshals v and drops the named top-level keys when their
// value is the empty string. oauthex.AuthServerMeta tags jwks_uri without
// omitempty, and we have no JWKS (tokens are opaque); an empty "jwks_uri"
// fails URL validation in some clients.
func marshalWithout(v any, keys ...string) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for _, k := range keys {
		if string(m[k]) == `""` {
			delete(m, k)
		}
	}
	return json.Marshal(m)
}
