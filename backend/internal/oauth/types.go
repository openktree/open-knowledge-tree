package oauth

import "time"

// Config is the transport-agnostic configuration the Server needs. It
// is a subset of config.Config (the HTTP layer maps the wider
// config.OAuthConfig into this shape). Keeping the dependency one-way
// (oauth <- config, never the reverse) lets the oauth package stay
// reusable by a future CLI or worker.
type Config struct {
	// Issuer is the `iss` claim on access tokens and the `issuer`
	// field in the OAuth metadata. It MUST be the externally
	// resolvable base URL of the OKT instance (e.g.
	// "https://okt.example.com"). The default config falls back to
	// "http://localhost:<port>" for local dev.
	Issuer string
	// AccessTokenTTL is the lifetime of issued access JWTs.
	AccessTokenTTL time.Duration
	// RefreshTokenTTL is the lifetime of issued refresh tokens.
	// Refresh rotation deletes the old token, so this TTL bounds
	// how long an idle session can stay valid without a re-login.
	RefreshTokenTTL time.Duration
	// AuthCodeTTL is the lifetime of an authorization code. 10m is
	// the spec-recommended default; short enough that a leaked code
	// is quickly useless.
	AuthCodeTTL time.Duration
}

// TokenPair is the standard OAuth token response body returned by
// the token endpoint. AccessToken is the JWT the client presents to
// the MCP endpoint; RefreshToken is the opaque token the client
// posts to /token with grant_type=refresh_token to get a new pair.
// TokenType is always "Bearer"; ExpiresIn is the access token TTL
// in seconds (per RFC 6749 §4.1.4).
type TokenPair struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

// ClientRegistrationResponse is the RFC 7591 response body returned
// by POST /oauth/register. ClientID is the public identifier the
// client uses on the authorize + token endpoints; ClientSecret is
// omitted for public clients (the default; PKCE is the
// confidentiality mechanism). ClientIDIssuedAt is unix seconds.
type ClientRegistrationResponse struct {
	ClientID           string `json:"client_id"`
	ClientSecret       string `json:"client_secret,omitempty"`
	ClientIDIssuedAt   int64  `json:"client_id_issued_at"`
	ClientName        string `json:"client_name,omitempty"`
	RedirectUris       []string `json:"redirect_uris"`
	GrantTypes        []string `json:"grant_types"`
	ResponseTypes     []string `json:"response_types"`
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
	Scope             string `json:"scope"`
}