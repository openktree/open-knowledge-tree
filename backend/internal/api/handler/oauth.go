package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/oauth"
)

// OAuth bundles the OAuth 2.1 authorization-server HTTP handlers.
// It is a thin wrapper over internal/oauth.Server: it parses form
// values, calls the Server, and writes the spec-shaped responses.
// All the authorization logic (PKCE, code lifetime, refresh
// rotation) lives in internal/oauth so it stays transport-agnostic
// and unit-testable.
type OAuth struct {
	srv    *oauth.Server
	issuer string
	// mcpResourceURL is the absolute URL of the MCP endpoint this
	// server protects. It is advertised in the RFC 9728
	// /.well-known/oauth-protected-resource document so MCP clients
	// can discover where to send bearer tokens.
	mcpResourceURL string
}

// NewOAuth constructs an OAuth handler bundle. issuer is the
// externally-resolvable base URL of the OKT instance (cfg.OAuth.Issuer
// or the http://localhost:<port> fallback). mcpResourceURL is the
// full URL of the MCP endpoint (issuer + "/api/v1/mcp").
func NewOAuth(srv *oauth.Server, issuer, mcpResourceURL string) *OAuth {
	return &OAuth{srv: srv, issuer: strings.TrimRight(issuer, "/"), mcpResourceURL: mcpResourceURL}
}

// Register handles POST /api/v1/oauth/register (RFC 7591 Dynamic
// Client Registration). MCP clients self-register on first connect;
// the response carries the client_id they use on the authorize +
// token endpoints. The endpoint accepts a JSON body with
// redirect_uris (required) and client_name (optional). Public clients
// only — we don't issue client secrets (PKCE is the confidentiality
// mechanism per OAuth 2.1).
func (o *OAuth) Register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		GrantTypes   []string `json:"grant_types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	resp, err := o.srv.RegisterClient(r.Context(), body.RedirectURIs, body.ClientName)
	if err != nil {
		writeOAuthServerErr(w, err)
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, resp)
}

// Authorize handles GET /api/v1/oauth/authorize. It is the
// interactive endpoint: the user's browser hits it, the handler
// validates the request, and then either:
//   - renders a login form (no signed login cookie yet), or
//   - renders a consent form (logged in but not yet consented), or
//   - 302s to the client's redirect_uri with the code (consented).
//
// The login + consent flow is a small server-rendered HTML surface
// (see oauth_login.html / oauth_consent.html) kept independent of
// the SolidJS frontend so the OAuth flow doesn't pull the SPA in.
// A short-lived signed cookie ("okt_oauth_login") carries the
// logged-in user id from the login POST to the consent GET.
//
// On validation errors the handler renders a plain-text error page
// rather than redirecting with an error code, because a misconfigured
// client (wrong redirect_uri, missing PKCE) is a developer error the
// user can't act on via the OAuth error flow.
func (o *OAuth) Authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := oauth.AuthorizeRequest{
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		ResponseType:        q.Get("response_type"),
		Scope:               q.Get("scope"),
		State:               q.Get("state"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
	}
	client, err := o.srv.ValidateAuthorize(r.Context(), req)
	if err != nil {
		writeAuthorizeErr(w, err)
		return
	}

	// Step 1: is the user logged in? The login cookie carries the
	// user id; if absent, render the login form with the original
	// authorize query echoed back as a hidden field so the POST can
	// re-issue the authorize after a successful login.
	userID, loggedIn := readLoginCookie(r)
	if !loggedIn {
		renderOAuthLogin(w, r.URL.RawQuery, client.ClientName)
		return
	}

	// Step 2: logged in. Render the consent form so the user
	// explicitly approves the MCP client's access. Consent is
	// implicit for the POST (the form posts back to /authorize with
	// a consent=yes field); a "deny" aborts with access_denied.
	if r.Method == http.MethodGet {
		renderOAuthConsent(w, r.URL.RawQuery, client.ClientName, userID)
		return
	}
	// POST: consent decision.
	if r.ParseForm() != nil {
		writeAuthorizeErr(w, oauth.ErrInvalidRequest)
		return
	}
	if r.FormValue("consent") != "yes" {
		redirectOAuthError(w, req.RedirectURI, req.State, "access_denied")
		return
	}

	// Issue the code and 302 to the client's redirect_uri with the
	// code + state. The cookie is cleared so the next authorize
	// starts fresh.
	code, err := o.srv.IssueAuthorizationCode(r.Context(), client, req.RedirectURI, req.Scope, userID, req.CodeChallenge, req.CodeChallengeMethod)
	if err != nil {
		writeAuthorizeErr(w, err)
		return
	}
	clearLoginCookie(w, r)
	redirectOAuthCode(w, req.RedirectURI, code, req.State)
}

// AuthorizeLoginPOST handles POST /api/v1/oauth/authorize/login. It
// is the form-action target of the login form: the user submits
// email + password, the handler resolves them via the OAuth
// Server's UserLookup, sets the short-lived login cookie, and 302s
// back to /authorize with the original query so the consent form
// renders. Bad credentials re-render the login form with an error.
func (o *OAuth) AuthorizeLoginPOST(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil {
		writeAuthorizeErr(w, oauth.ErrInvalidRequest)
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")
	authorizeQuery := r.FormValue("authorize_query")
	user, ok, err := o.srv.LoginUser(r.Context(), email, password)
	if err != nil {
		writeAuthorizeErr(w, err)
		return
	}
	if !ok {
		renderOAuthLoginError(w, authorizeQuery, "Invalid email or password")
		return
	}
	setLoginCookie(w, r, user.ID)
	// Redirect back to /authorize with the original query so the
	// now-logged-in user sees the consent form.
	http.Redirect(w, r, "/api/v1/oauth/authorize?"+authorizeQuery, http.StatusSeeOther)
}

// Token handles POST /api/v1/oauth/token. It supports two grant
// types: authorization_code (the OAuth 2.1 primary) and
// refresh_token (rotation). Responses are the standard
// {access_token, token_type, expires_in, refresh_token, scope}
// JSON. Errors are RFC 6749 §5.2 shaped
// {error, error_description}.
func (o *OAuth) Token(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "could not parse form body")
		return
	}
	grantType := r.FormValue("grant_type")
	clientID := r.FormValue("client_id")
	switch grantType {
	case "authorization_code":
		code := r.FormValue("code")
		redirectURI := r.FormValue("redirect_uri")
		verifier := r.FormValue("code_verifier")
		pair, err := o.srv.ExchangeAuthorizationCode(r.Context(), clientID, code, redirectURI, verifier)
		if err != nil {
			writeOAuthServerErr(w, err)
			return
		}
		httputil.WriteJSON(w, http.StatusOK, pair)
	case "refresh_token":
		refresh := r.FormValue("refresh_token")
		pair, err := o.srv.RefreshAccessToken(r.Context(), clientID, refresh)
		if err != nil {
			writeOAuthServerErr(w, err)
			return
		}
		httputil.WriteJSON(w, http.StatusOK, pair)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

// Revoke handles POST /api/v1/oauth/revoke (RFC 7009). It revokes a
// refresh token. The endpoint is idempotent: an unknown or
// already-revoked token returns 200 with an empty body, matching
// the spec's "always 200" rule. We don't revoke access tokens
// (they're short-lived JWTs; revocation would need a blocklist).
func (o *OAuth) Revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	refresh := r.FormValue("token")
	if refresh == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	_ = o.srv.RevokeRefreshToken(r.Context(), refresh)
	w.WriteHeader(http.StatusOK)
}

// Metadata handles GET /.well-known/oauth-authorization-server
// (RFC 8414). It advertises the issuer + endpoint URLs + supported
// grant types + PKCE method so MCP clients can auto-configure from
// a single well-known URL.
func (o *OAuth) Metadata(w http.ResponseWriter, r *http.Request) {
	base := o.issuer
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/api/v1/oauth/authorize",
		"token_endpoint":                        base + "/api/v1/oauth/token",
		"registration_endpoint":                 base + "/api/v1/oauth/register",
		"revocation_endpoint":                  base + "/api/v1/oauth/revoke",
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"response_types_supported":              []string{"code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                     []string{oauth.Scope},
	})
}

// ProtectedResource handles GET
// /.well-known/oauth-protected-resource (RFC 9728). It tells MCP
// clients that the MCP endpoint is OAuth-protected and where its
// authorization server's metadata lives. The 401 response from the
// MCP endpoint also points here via WWW-Authenticate.
func (o *OAuth) ProtectedResource(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"resource":                  o.mcpResourceURL,
		"authorization_servers":     []string{o.issuer},
		"bearer_methods_supported":  []string{"header"},
		"scopes_supported":          []string{oauth.Scope},
	})
}

// writeOAuthError writes an RFC 6749 §5.2 error response.
func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	httputil.WriteJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

// writeOAuthServerErr maps an internal/oauth error to its RFC 6749
// error code + HTTP status. Unknown errors become a 500
// server_error so the client sees something actionable.
func writeOAuthServerErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, oauth.ErrInvalidGrant):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code or refresh token is invalid, expired, or already used")
	case errors.Is(err, oauth.ErrPKCEFailed):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "pkce verifier does not match the challenge")
	case errors.Is(err, oauth.ErrInvalidClient):
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown or unauthorized client")
	case errors.Is(err, oauth.ErrInvalidRequest):
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing or malformed parameter")
	case errors.Is(err, oauth.ErrInvalidScope):
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "requested scope is not supported")
	case errors.Is(err, oauth.ErrUnsupportedGrantType):
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type not supported")
	default:
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "internal error")
	}
}

// writeAuthorizeErr renders an authorize-endpoint validation error
// as a plain-text page (not an OAuth redirect), because a bad
// authorize request is a developer error the user can't act on via
// the OAuth error flow.
func writeAuthorizeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	switch {
	case errors.Is(err, oauth.ErrInvalidClient):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("OAuth authorize error: unknown client_id.\n"))
	case errors.Is(err, oauth.ErrInvalidRequest):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("OAuth authorize error: request is malformed (response_type must be 'code', PKCE required with S256, redirect_uri must match a registered URI).\n"))
	case errors.Is(err, oauth.ErrInvalidScope):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("OAuth authorize error: requested scope is not supported.\n"))
	default:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("OAuth authorize error: internal error.\n"))
	}
}

// redirectOAuthCode 302s to the redirect_uri with code + state.
func redirectOAuthCode(w http.ResponseWriter, redirectURI, code, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, &http.Request{URL: u}, u.String(), http.StatusFound)
}

// redirectOAuthError 302s to the redirect_uri with an error code +
// state. Used for user-facing denials (access_denied) where the
// client should be told the user said no.
func redirectOAuthError(w http.ResponseWriter, redirectURI, state, errCode string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", errCode)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, &http.Request{URL: u}, u.String(), http.StatusFound)
}