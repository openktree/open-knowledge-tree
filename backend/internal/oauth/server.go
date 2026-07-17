package oauth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/auth"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// UserLookup is the callback the Server uses to resolve a user from
// their email + password during the authorize flow's login step. It
// returns the user row and a bool indicating the password matched.
// The HTTP layer passes store.Queries.GetUserByEmail +
// auth.CheckPassword; keeping it as an injected callback lets tests
// supply a stub without a database.
type UserLookup func(ctx context.Context, email, password string) (store.User, bool, error)

// Server is the transport-agnostic OAuth 2.1 authorization server.
// The HTTP layer (internal/api/handler/oauth.go) is a thin wrapper
// that parses requests, calls into these methods, and writes
// responses. All persistence goes through the sqlc-generated
// *store.Queries against the system pool (oauth_* tables live in
// okt_system, next to users/sessions).
type Server struct {
	cfg    Config
	jwtSecret string
	q      *store.Queries
	lookup UserLookup
}

// NewServer constructs an OAuth Server. jwtSecret is shared with the
// existing session JWT (cfg.Auth.JWTSecret) so the access-token
// verification middleware can use the same secret. lookup is the
// email/password resolver used by the authorize login step.
func NewServer(cfg Config, jwtSecret string, q *store.Queries, lookup UserLookup) *Server {
	return &Server{cfg: cfg, jwtSecret: jwtSecret, q: q, lookup: lookup}
}

// ResolveClient looks up a registered client by client_id. Returns
// ErrInvalidClient when the client is unknown.
func (s *Server) ResolveClient(ctx context.Context, clientID string) (store.OktSystemOauthClient, error) {
	if clientID == "" {
		return store.OktSystemOauthClient{}, ErrInvalidClient
	}
	c, err := s.q.GetOAuthClientByClientID(ctx, clientID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.OktSystemOauthClient{}, ErrInvalidClient
		}
		return store.OktSystemOauthClient{}, err
	}
	return c, nil
}

// RegisterClient implements RFC 7591 Dynamic Client Registration.
// It validates the submitted metadata, mints a client_id (random
// 16-byte URL-safe string), persists the client, and returns the
// registration response. We accept public clients only
// (token_endpoint_auth_method="none", PKCE is the confidentiality
// mechanism) — a non-empty client_secret in the request is hashed
// and stored but never returned in subsequent responses (per RFC
// 7591 §3.2.1 the secret is shown once).
//
// Validation is intentionally permissive: MCP clients (Claude
// Desktop, etc.) send a minimal metadata document with just
// redirect_uris; we fill in the OAuth 2.1 defaults the spec
// requires (grant_types, response_types, S256 PKCE). An empty
// redirect_uris list is rejected because the authorize endpoint
// needs at least one URI to compare against.
func (s *Server) RegisterClient(ctx context.Context, redirectURIs []string, clientName string) (ClientRegistrationResponse, error) {
	if len(redirectURIs) == 0 {
		return ClientRegistrationResponse{}, ErrInvalidRequest
	}
	for i := range redirectURIs {
		redirectURIs[i] = strings.TrimSpace(redirectURIs[i])
		if redirectURIs[i] == "" {
			return ClientRegistrationResponse{}, ErrInvalidRequest
		}
	}
	// Mint a client_id. We use the same opaque-token generator so
	// client ids are uniformly random and URL-safe.
	clientID, _, err := GenerateToken()
	if err != nil {
		return ClientRegistrationResponse{}, err
	}
	// Public client: no secret stored. Confidential clients are
	// out of scope for the first cut; the OAuth 2.1 spec prefers
	// PKCE for native/desktop clients anyway.
	row, err := s.q.CreateOAuthClient(ctx, store.CreateOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        nil,
		RedirectUris:            redirectURIs,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		ClientName:              clientName,
		Scope:                   Scope,
	})
	if err != nil {
		return ClientRegistrationResponse{}, err
	}
	return ClientRegistrationResponse{
		ClientID:                 row.ClientID,
		ClientIDIssuedAt:         row.ClientIDIssuedAt.Time.Unix(),
		ClientName:               row.ClientName,
		RedirectUris:             row.RedirectUris,
		GrantTypes:               row.GrantTypes,
		ResponseTypes:            row.ResponseTypes,
		TokenEndpointAuthMethod:  row.TokenEndpointAuthMethod,
		Scope:                    row.Scope,
	}, nil
}

// AuthorizeRequest is the parsed authorize-endpoint input. The HTTP
// layer fills this from the query string; the Server validates it
// and either returns a redirect URL (with the code) or an error
// code for the HTTP layer to render.
type AuthorizeRequest struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// AuthorizeResult is the outcome of validating an authorize request
// and obtaining the user's consent. On success, RedirectURL holds the
// full URL the HTTP layer should 302 the user's browser to (it
// already includes the code + state as query params). The raw code
// is returned separately so the HTTP layer doesn't have to re-parse
// the URL.
type AuthorizeResult struct {
	RedirectURL string
	Code        string
}

// ValidateAuthorize checks that the authorize request is well-formed
// and that the client is registered with a matching redirect_uri.
// It enforces OAuth 2.1 mandates: response_type=code, PKCE required
// (code_challenge present, method=S256). It does NOT touch the user
// session — the HTTP layer runs the login + consent flow first and
// only calls IssueAuthorizationCode once it has a confirmed user.
func (s *Server) ValidateAuthorize(ctx context.Context, req AuthorizeRequest) (store.OktSystemOauthClient, error) {
	if req.ResponseType != "code" {
		return store.OktSystemOauthClient{}, ErrInvalidRequest
	}
	if req.CodeChallenge == "" || req.CodeChallengeMethod != "S256" {
		// OAuth 2.1 mandates PKCE with S256. We reject plain (method
		// "plain") and missing challenges outright; a client that
		// doesn't send a challenge gets an error, not a fallback.
		return store.OktSystemOauthClient{}, ErrInvalidRequest
	}
	if req.Scope != "" && req.Scope != Scope {
		return store.OktSystemOauthClient{}, ErrInvalidScope
	}
	client, err := s.ResolveClient(ctx, req.ClientID)
	if err != nil {
		return store.OktSystemOauthClient{}, err
	}
	if !uriAllowed(client.RedirectUris, req.RedirectURI) {
		return store.OktSystemOauthClient{}, ErrInvalidRequest
	}
	return client, nil
}

// IssueAuthorizationCode creates a short-lived authorization code
// bound to the user, client, redirect_uri, and PKCE challenge. The
// HTTP layer calls this after the user has logged in and consented.
// The returned code is the raw opaque token the client receives;
// the stored row holds the hash so a database leak doesn't expose
// live codes.
func (s *Server) IssueAuthorizationCode(ctx context.Context, client store.OktSystemOauthClient, redirectURI, scope string, userID pgtype.UUID, codeChallenge, codeChallengeMethod string) (string, error) {
	raw, hash, err := GenerateToken()
	if err != nil {
		return "", err
	}
	scopeToStore := scope
	if scopeToStore == "" {
		scopeToStore = Scope
	}
	_, err = s.q.CreateOAuthAuthorizationCode(ctx, store.CreateOAuthAuthorizationCodeParams{
		CodeHash:            hash,
		ClientID:            client.ClientID,
		RedirectUri:         redirectURI,
		Scope:               scopeToStore,
		UserID:              userID,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		ExpiresAt:           pgtype.Timestamptz{Time: time.Now().Add(s.cfg.AuthCodeTTL), Valid: true},
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// ExchangeAuthorizationCode implements the authorization_code grant
// at the token endpoint. It verifies the code exists, belongs to the
// presented client, hasn't expired, and that the PKCE verifier
// matches the stored challenge. On success it deletes the code
// (single-use) and issues an access + refresh token pair. A
// mismatched verifier or client id returns ErrInvalidGrant.
func (s *Server) ExchangeAuthorizationCode(ctx context.Context, clientID, code, redirectURI, codeVerifier string) (TokenPair, error) {
	codeHash := HashToken(code)
	ac, err := s.q.GetOAuthAuthorizationCodeByHash(ctx, codeHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenPair{}, ErrInvalidGrant
		}
		return TokenPair{}, err
	}
	// Single-use: delete the code now, before issuing tokens. A
	// concurrent retry with the same code will find the row gone
	// and fail with ErrInvalidGrant — this is the spec-mandated
	// replay protection.
	_ = s.q.DeleteOAuthAuthorizationCodeByHash(ctx, codeHash)

	if ac.ClientID != clientID {
		return TokenPair{}, ErrInvalidGrant
	}
	if ac.RedirectUri != redirectURI {
		return TokenPair{}, ErrInvalidGrant
	}
	if time.Now().After(ac.ExpiresAt.Time) {
		return TokenPair{}, ErrInvalidGrant
	}
	if err := VerifyPKCE(codeVerifier, ac.CodeChallenge); err != nil {
		return TokenPair{}, err
	}
	return s.issueTokens(ctx, clientID, ac.UserID, ac.Scope)
}

// RefreshAccessToken implements the refresh_token grant. It verifies
// the refresh token exists, belongs to the presented client, hasn't
// expired or been revoked, then rotates it (deletes the old row,
// issues a new pair). Rotation means a stolen-and-used refresh token
// is detectable: the legitimate client's next refresh fails because
// the old token is gone.
func (s *Server) RefreshAccessToken(ctx context.Context, clientID, refreshToken string) (TokenPair, error) {
	rtHash := HashToken(refreshToken)
	rt, err := s.q.GetOAuthRefreshTokenByHash(ctx, rtHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenPair{}, ErrInvalidGrant
		}
		return TokenPair{}, err
	}
	if rt.ClientID != clientID || rt.Revoked || time.Now().After(rt.ExpiresAt.Time) {
		return TokenPair{}, ErrInvalidGrant
	}
	// Rotation: delete the old refresh token, then issue a new
	// pair. Deletion (not just revoke) is the rotation signal.
	_ = s.q.DeleteOAuthRefreshTokenByHash(ctx, rtHash)
	return s.issueTokens(ctx, clientID, rt.UserID, rt.Scope)
}

// RevokeRefreshToken implements RFC 7009 token revocation for
// refresh tokens. We don't revoke access tokens (they're short-lived
// JWTs; revocation would require a blocklist). The endpoint is
// idempotent: revoking an unknown or already-revoked token returns
// no error.
func (s *Server) RevokeRefreshToken(ctx context.Context, refreshToken string) error {
	rtHash := HashToken(refreshToken)
	_ = s.q.DeleteOAuthRefreshTokenByHash(ctx, rtHash)
	return nil
}

// issueTokens is the shared helper that mints a new access JWT and a
// new opaque refresh token, persists the refresh token, and returns
// the pair. It is called by both the authorization_code and
// refresh_token grants.
func (s *Server) issueTokens(ctx context.Context, clientID string, userID pgtype.UUID, scope string) (TokenPair, error) {
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		// The user row must exist (the authorize flow resolved them
		// via the lookup callback). A failure here is a race with
		// user deletion; surface it as invalid_grant.
		return TokenPair{}, ErrInvalidGrant
	}
	access, err := IssueAccessToken(s.jwtSecret, s.cfg.Issuer, s.cfg.AccessTokenTTL, userID, user.Email, clientID, scope)
	if err != nil {
		return TokenPair{}, err
	}
	rawRefresh, refreshHash, err := GenerateToken()
	if err != nil {
		return TokenPair{}, err
	}
	_, err = s.q.CreateOAuthRefreshToken(ctx, store.CreateOAuthRefreshTokenParams{
		TokenHash:  refreshHash,
		ClientID:   clientID,
		UserID:     userID,
		Scope:      scope,
		ExpiresAt:  pgtype.Timestamptz{Time: time.Now().Add(s.cfg.RefreshTokenTTL), Valid: true},
	})
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int(s.cfg.AccessTokenTTL.Seconds()),
		RefreshToken: rawRefresh,
		Scope:        scope,
	}, nil
}

// LoginUser is the authorize-endpoint login step. It resolves the
// OKT user from email + password via the injected UserLookup
// callback. The HTTP consent flow calls this when the user submits
// the login form. Returns the user row and a bool indicating
// success; a false bool means the credentials were wrong (the HTTP
// layer re-renders the login form with an error).
func (s *Server) LoginUser(ctx context.Context, email, password string) (store.User, bool, error) {
	return s.lookup(ctx, email, password)
}

// DefaultUserLookup is the production UserLookup: it resolves the
// user by email and checks the password with the existing bcrypt
// helper. It is wired in cmd/app/api.go; tests inject a stub.
func DefaultUserLookup(q *store.Queries) UserLookup {
	return func(ctx context.Context, email, password string) (store.User, bool, error) {
		user, err := q.GetUserByEmail(ctx, email)
		if err != nil {
			// A missing user is a login failure, not an error. The
			// HTTP layer treats the false bool as "bad credentials"
			// and re-renders the form; we don't distinguish
			// "no such user" from "wrong password" to avoid user
			// enumeration.
			return store.User{}, false, nil
		}
		if !auth.CheckPassword(user.PasswordHash, password) {
			return store.User{}, false, nil
		}
		return user, true, nil
	}
}

// uriAllowed returns true if the requested redirect_uri exactly
// matches one of the client's registered URIs. OAuth 2.1 forbids
// wildcard matching and requires exact string comparison, so this
// is a simple linear scan. The list is small (typically one URI
// per MCP client).
func uriAllowed(registered []string, requested string) bool {
	for _, u := range registered {
		if u == requested {
			return true
		}
	}
	return false
}