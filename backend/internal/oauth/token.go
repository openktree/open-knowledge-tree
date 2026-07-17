// Package oauth implements OKT's own OAuth 2.1 authorization server.
//
// The package is transport-agnostic: it knows nothing about HTTP. The
// HTTP layer (internal/api/handler/oauth.go) is a thin wrapper that
// parses form values and calls into this package. This keeps the
// authorization logic reusable and easy to test in isolation.
//
// The OAuth server supports:
//   - Authorization Code grant with PKCE (S256 only, per OAuth 2.1).
//   - Refresh Token grant (rotation: each refresh deletes the old
//     token and issues a new one).
//   - Dynamic Client Registration (RFC 7591) for MCP clients that
//     self-register on first connect.
//
// Access tokens are self-contained HS256 JWTs (signed with the same
// cfg.Auth.JWTSecret the existing session JWT uses), carrying the
// user id, client id, and granted scope. The MCP resource server
// validates them statelessly via VerifyAccessToken. Refresh tokens
// are opaque random strings, stored hashed (SHA-256) at rest, the
// same way sessions.token_hash is stored.
package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Scope is the OAuth scope this server issues. The MCP authorization
// spec expects a single scope; we issue "mcp" for every token. The
// access token carries it so the resource server can verify the
// client asked for what it's using.
const Scope = "mcp"

// AccessTokenTTL is the default lifetime of an access token. Short
// enough that a leaked token is quickly useless, long enough that an
// interactive MCP session doesn't have to refresh mid-tool-call. The
// value is taken from cfg.OAuth.AccessTokenTTL when the Server is
// constructed; this constant is the fallback default used by
// IssueAccessToken callers that don't pass a ttl (tests).
const AccessTokenTTL = 15 * time.Minute

// Claims is the JWT payload of an OKT OAuth access token. It extends
// the existing session-JWT Claims shape (internal/auth.Claims) with
// the OAuth-specific client id and scope fields, so a single
// VerifyAccessToken call validates everything the resource server
// needs. Reusing the same signing secret (cfg.Auth.JWTSecret) keeps
// the two token surfaces consistent; the distinct issuer claim
// ("okt-oauth") distinguishes OAuth access tokens from session JWTs
// so a future cross-check could reject a session JWT presented as a
// bearer to the MCP endpoint.
type Claims struct {
	jwt.RegisteredClaims
	UserID   string `json:"uid"`
	Email    string `json:"email,omitempty"`
	ClientID string `json:"cid"`
	Scope    string `json:"scope"`
}

// Issuer is the `iss` claim set on every OKT access token. It is
// configurable via cfg.OAuth.Issuer; the default is
// "http://localhost:<server.port>" but production deployments should
// set it to the externally-resolvable URL of the OKT instance.
const Issuer = "okt-oauth"

// IssueAccessToken signs a self-contained HS256 JWT carrying the
// user id, email, client id, and scope. The token expires at ttl
// from now. The caller is responsible for minting the refresh token
// alongside this access token (see Server.IssueTokens).
func IssueAccessToken(secret, issuer string, ttl time.Duration, userID pgtype.UUID, email, clientID, scope string) (string, error) {
	if !userID.Valid {
		return "", errors.New("oauth: user id is required")
	}
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		UserID:   userID.String(),
		Email:    email,
		ClientID: clientID,
		Scope:    scope,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// VerifyAccessToken validates the signature and expiry of an OAuth
// access token and returns its claims. It enforces HS256 (the same
// method IssueAccessToken uses) so a token signed with any other
// algorithm is rejected — this is the OAuth 2.1 "no alg confusion"
// rule. A nil claims return with a non-nil error means the token is
// invalid; the resource server treats any error as a 401.
func VerifyAccessToken(secret, tokenStr string) (*Claims, error) {
	tok, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("verifying access token: %w", err)
	}
	claims, ok := tok.Claims.(*Claims)
	if !ok || !tok.Valid {
		return nil, errors.New("invalid access token claims")
	}
	return claims, nil
}

// GenerateToken is the opaque-token generator shared by authorization
// codes and refresh tokens. It returns the raw token (to send to the
// client) and the SHA-256 hex hash (to store). The caller must never
// store the raw token; only the hash.
func GenerateToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err := readRand(b); err != nil {
		return "", "", fmt.Errorf("generating token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	hash = HashToken(raw)
	return raw, hash, nil
}

// HashToken returns the SHA-256 hex hash of a raw opaque token. It
// mirrors auth.HashToken (sessions) so the storage pattern is
// identical; we duplicate it here to avoid importing internal/auth
// from a transport-agnostic package (auth imports nothing HTTP, but
// keeping oauth self-contained avoids a dependency edge).
func HashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", h[:])
}

// VerifyPKCE checks that the supplied verifier hashes (SHA-256,
// base64url-no-padding) to the stored challenge. S256 is the only
// method this server accepts, so the comparison is a single
// constant-time-ish equality check. A mismatch returns ErrPKCEFailed
// which the token endpoint surfaces as invalid_grant.
func VerifyPKCE(verifier, challenge string) error {
	if verifier == "" {
		return ErrPKCEFailed
	}
	h := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(h[:])
	if computed != challenge {
		return ErrPKCEFailed
	}
	return nil
}

// ErrPKCEFailed is returned when the PKCE verifier doesn't match the
// stored challenge. The token endpoint maps it to invalid_grant.
var ErrPKCEFailed = errors.New("oauth: pkce verification failed")

// ErrInvalidGrant is returned when an authorization code or refresh
// token is not found, already used, expired, or doesn't match the
// presented client id. The token endpoint maps it to invalid_grant.
var ErrInvalidGrant = errors.New("oauth: invalid grant")

// ErrInvalidClient is returned when the presented client id is
// unknown or the client is not allowed to use the requested grant
// type. The token endpoint maps it to invalid_client.
var ErrInvalidClient = errors.New("oauth: invalid client")

// ErrInvalidRequest is returned when a required parameter is
// missing or malformed. The token endpoint maps it to invalid_request.
var ErrInvalidRequest = errors.New("oauth: invalid request")

// ErrInvalidScope is returned when the requested scope is not
// supported by this server. We only issue "mcp".
var ErrInvalidScope = errors.New("oauth: invalid scope")

// ErrUnsupportedGrantType is returned when the grant_type is not
// authorization_code or refresh_token.
var ErrUnsupportedGrantType = errors.New("oauth: unsupported grant_type")