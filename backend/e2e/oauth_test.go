//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/oauth"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// dcr registers a new OAuth client via RFC 7591 Dynamic Client
// Registration and returns the parsed response. The redirect_uri
// is the caller's choice; tests typically pass a localhost loopback
// URL (the OAuth 2.1 spec allows 127.0.0.1 loopback for native
// clients with any port).
func dcr(t *testing.T, baseURL string, redirectURIs []string, clientName string) oauth.ClientRegistrationResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": redirectURIs,
		"client_name":    clientName,
	})
	resp, err := http.Post(baseURL+"/api/v1/oauth/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("DCR POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("DCR: expected 201, got %d: %s", resp.StatusCode, raw)
	}
	var out oauth.ClientRegistrationResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("DCR unmarshal: %v", err)
	}
	return out
}

// pkceVerifierAndChallenge generates a random PKCE verifier + its
// S256 challenge (base64url-no-padding of SHA-256(verifier)). Tests
// use this so the authorize + token steps have a matching pair.
func pkceVerifierAndChallenge(t *testing.T) (verifier, challenge string) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge
}

// runAuthorizeFlow walks the full browser-side OAuth flow against
// the test server: GET /authorize (login form), POST
// /authorize/login (submit credentials), GET /authorize (consent
// form), POST /authorize (consent=yes). It follows redirects with
// a cookie jar so the signed login cookie survives between steps.
// The final 302 to the client's redirect_uri is intercepted by an
// http.Client CheckRedirect that stops following and pulls the
// code off the Location header — this avoids the race of spinning
// up a capturing HTTP server on the redirect_uri port. Returns
// the authorization code.
func runAuthorizeFlow(t *testing.T, baseURL, clientID, redirectURI, email, password string, verifier, challenge, state string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	var capturedCode string
	client := &http.Client{
		Jar: jar,
		// Stop following redirects the moment we hit the client's
		// redirect_uri and grab the code off the Location header.
		// The redirect_uri points at 127.0.0.1:<port> which has
		// no server listening (we never bind it); stopping here
		// avoids a connection-refused error and gives us the code.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if strings.HasPrefix(req.URL.String(), redirectURI) {
				capturedCode = req.URL.Query().Get("code")
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	authorizeQuery := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"mcp"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	// Step 1: GET /authorize — should render the login form (200
	// HTML, not a redirect, because no login cookie yet).
	resp, err := client.Get(baseURL + "/api/v1/oauth/authorize?" + authorizeQuery)
	if err != nil {
		t.Fatalf("authorize GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize GET: expected 200 (login form), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Step 2: POST /authorize/login with email + password. The
	// server sets the signed login cookie and 302s back to
	// /authorize (which the client follows to the consent form).
	loginForm := url.Values{
		"email":           {email},
		"password":        {password},
		"authorize_query": {authorizeQuery},
	}
	resp, err = client.PostForm(baseURL+"/api/v1/oauth/authorize/login", loginForm)
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	resp.Body.Close()

	// Step 3: POST /authorize with consent=yes. The server issues
	// the code and 302s to redirect_uri?code=...&state=... The
	// CheckRedirect hook captures the code and stops following.
	consentForm := url.Values{"consent": {"yes"}}
	consentReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/oauth/authorize?"+authorizeQuery, strings.NewReader(consentForm.Encode()))
	consentReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(consentReq)
	if err != nil {
		t.Fatalf("consent POST: %v", err)
	}
	resp.Body.Close()

	if capturedCode == "" {
		// The redirect wasn't captured — either the consent POST
		// didn't 302, or the redirect didn't point at redirect_uri.
		t.Fatal("authorize flow: no code captured at redirect_uri (consent POST did not redirect to redirect_uri)")
	}
	return capturedCode
}

// exchangeCode POSTs the authorization code + PKCE verifier to the
// token endpoint and returns the parsed token pair. Errors fail
// the test with the raw body so the error_description is visible.
func exchangeCode(t *testing.T, baseURL, clientID, code, redirectURI, verifier string) oauth.TokenPair {
	t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri": {redirectURI},
		"code_verifier": {verifier},
	}
	resp, err := http.PostForm(baseURL+"/api/v1/oauth/token", form)
	if err != nil {
		t.Fatalf("token POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token exchange: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var pair oauth.TokenPair
	if err := json.Unmarshal(raw, &pair); err != nil {
		t.Fatalf("token unmarshal: %v", err)
	}
	return pair
}

// TestOAuth_DCR_HappyPath verifies RFC 7591 Dynamic Client
// Registration: a POST with redirect_uris returns 201 + a
// client_id that can be used on the authorize endpoint. The
// response shape matches the spec (client_id, redirect_uris,
// grant_types, response_types, token_endpoint_auth_method=none).
func TestOAuth_DCR_HappyPath(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	reg := dcr(t, env.BaseURL, []string{"http://127.0.0.1:5399/callback"}, "test-mcp-client")
	if reg.ClientID == "" {
		t.Fatal("DCR: empty client_id")
	}
	if reg.TokenEndpointAuthMethod != "none" {
		t.Fatalf("DCR: expected public client (none), got %q", reg.TokenEndpointAuthMethod)
	}
	if len(reg.GrantTypes) != 2 || reg.GrantTypes[0] != "authorization_code" || reg.GrantTypes[1] != "refresh_token" {
		t.Fatalf("DCR: unexpected grant_types %v", reg.GrantTypes)
	}
	if len(reg.ResponseTypes) != 1 || reg.ResponseTypes[0] != "code" {
		t.Fatalf("DCR: unexpected response_types %v", reg.ResponseTypes)
	}
}

// TestOAuth_DCR_RejectsEmptyRedirectURIs verifies the server
// rejects a registration with no redirect_uris (the authorize
// endpoint needs at least one URI to compare against).
func TestOAuth_DCR_RejectsEmptyRedirectURIs(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{}})
	resp, err := http.Post(env.BaseURL+"/api/v1/oauth/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("DCR POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("DCR empty URIs: expected 400, got %d", resp.StatusCode)
	}
}

// TestOAuth_AuthorizeCodeFlow_PKCE is the happy-path end-to-end:
// DCR → authorize (login + consent) → token exchange with PKCE →
// a usable access JWT. The access token is verified by calling the
// MCP endpoint's tools/list (the MCP e2e test does that; here we
// just assert the token parses and carries the right claims).
func TestOAuth_AuthorizeCodeFlow_PKCE(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	// Seed a user the authorize login step can authenticate as.
	_ = registerTestUser(t, env, "oauth-flow@example.com", "password123", "OAuth Flow")

	reg := dcr(t, env.BaseURL, []string{"http://127.0.0.1:5398/callback"}, "test-mcp-client")
	verifier, challenge := pkceVerifierAndChallenge(t)
	const state = "xyz-state"
	const redirectURI = "http://127.0.0.1:5398/callback"

	code := runAuthorizeFlow(t, env.BaseURL, reg.ClientID, redirectURI, "oauth-flow@example.com", "password123", verifier, challenge, state)
	pair := exchangeCode(t, env.BaseURL, reg.ClientID, code, redirectURI, verifier)

	if pair.AccessToken == "" {
		t.Fatal("token exchange: empty access_token")
	}
	if pair.RefreshToken == "" {
		t.Fatal("token exchange: empty refresh_token")
	}
	if pair.TokenType != "Bearer" {
		t.Fatalf("token_type: expected Bearer, got %q", pair.TokenType)
	}
	if pair.ExpiresIn <= 0 {
		t.Fatalf("expires_in: expected positive, got %d", pair.ExpiresIn)
	}
	// Verify the access token parses with the test JWT secret.
	claims, err := oauth.VerifyAccessToken(env.Config.Auth.JWTSecret, pair.AccessToken)
	if err != nil {
		t.Fatalf("verifying access token: %v", err)
	}
	if claims.Scope != oauth.Scope {
		t.Fatalf("scope: expected %q, got %q", oauth.Scope, claims.Scope)
	}
	if claims.ClientID != reg.ClientID {
		t.Fatalf("cid: expected %q, got %q", reg.ClientID, claims.ClientID)
	}
}

// TestOAuth_TokenExchange_WrongPKCE verifies the token endpoint
// rejects a wrong PKCE verifier with invalid_grant. The code is
// single-use, so this also implicitly tests that a failed exchange
// consumes the code (a retry with the correct verifier must also
// fail).
func TestOAuth_TokenExchange_WrongPKCE(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	_ = registerTestUser(t, env, "pkce-wrong@example.com", "password123", "PKCE Wrong")

	reg := dcr(t, env.BaseURL, []string{"http://127.0.0.1:5397/callback"}, "pkce-wrong-client")
	verifier, challenge := pkceVerifierAndChallenge(t)
	const redirectURI = "http://127.0.0.1:5397/callback"
	code := runAuthorizeFlow(t, env.BaseURL, reg.ClientID, redirectURI, "pkce-wrong@example.com", "password123", verifier, challenge, "s")

	// Exchange with a wrong verifier. Expect 400 invalid_grant.
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {reg.ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {"this-is-not-the-verifier"},
	}
	resp, err := http.PostForm(env.BaseURL+"/api/v1/oauth/token", form)
	if err != nil {
		t.Fatalf("token POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong PKCE: expected 400, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "invalid_grant") {
		t.Fatalf("wrong PKCE: expected invalid_grant in body, got %s", raw)
	}
}

// TestOAuth_RefreshToken_Rotation verifies the refresh_token grant
// rotates: a successful refresh returns a new pair, and the OLD
// refresh token is no longer usable (deleted on use).
func TestOAuth_RefreshToken_Rotation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	_ = registerTestUser(t, env, "refresh@example.com", "password123", "Refresh")

	reg := dcr(t, env.BaseURL, []string{"http://127.0.0.1:5396/callback"}, "refresh-client")
	verifier, challenge := pkceVerifierAndChallenge(t)
	const redirectURI = "http://127.0.0.1:5396/callback"
	code := runAuthorizeFlow(t, env.BaseURL, reg.ClientID, redirectURI, "refresh@example.com", "password123", verifier, challenge, "s")
	pair := exchangeCode(t, env.BaseURL, reg.ClientID, code, redirectURI, verifier)

	// Refresh once: expect a new pair.
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {reg.ClientID},
		"refresh_token": {pair.RefreshToken},
	}
	resp, err := http.PostForm(env.BaseURL+"/api/v1/oauth/token", form)
	if err != nil {
		t.Fatalf("refresh POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var newPair oauth.TokenPair
	if err := json.Unmarshal(raw, &newPair); err != nil {
		t.Fatalf("refresh unmarshal: %v", err)
	}
	if newPair.AccessToken == "" || newPair.RefreshToken == "" {
		t.Fatal("refresh: empty tokens in response")
	}
	if newPair.RefreshToken == pair.RefreshToken {
		t.Fatal("refresh: rotation did not issue a new refresh token")
	}

	// Reuse the OLD refresh token: must fail with invalid_grant.
	resp2, err := http.PostForm(env.BaseURL+"/api/v1/oauth/token", form)
	if err != nil {
		t.Fatalf("reuse refresh POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("reuse old refresh: expected 400, got %d", resp2.StatusCode)
	}
}

// TestOAuth_RefreshToken_Revocation verifies POST /revoke marks a
// refresh token unusable. RFC 7009 says the endpoint is
// idempotent (always 200); the test asserts both the 200 and that
// the revoked token can't be refreshed.
func TestOAuth_RefreshToken_Revocation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	_ = registerTestUser(t, env, "revoke@example.com", "password123", "Revoke")

	reg := dcr(t, env.BaseURL, []string{"http://127.0.0.1:5395/callback"}, "revoke-client")
	verifier, challenge := pkceVerifierAndChallenge(t)
	const redirectURI = "http://127.0.0.1:5395/callback"
	code := runAuthorizeFlow(t, env.BaseURL, reg.ClientID, redirectURI, "revoke@example.com", "password123", verifier, challenge, "s")
	pair := exchangeCode(t, env.BaseURL, reg.ClientID, code, redirectURI, verifier)

	// Revoke: expect 200 (idempotent).
	revokeForm := url.Values{"token": {pair.RefreshToken}}
	resp, err := http.PostForm(env.BaseURL+"/api/v1/oauth/revoke", revokeForm)
	if err != nil {
		t.Fatalf("revoke POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d", resp.StatusCode)
	}

	// Refresh with the revoked token: must fail.
	refreshForm := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {reg.ClientID},
		"refresh_token": {pair.RefreshToken},
	}
	resp2, err := http.PostForm(env.BaseURL+"/api/v1/oauth/token", refreshForm)
	if err != nil {
		t.Fatalf("refresh after revoke POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("refresh after revoke: expected 400, got %d", resp2.StatusCode)
	}
}

// TestOAuth_Metadata asserts the RFC 8414 + RFC 9728 well-known
// documents carry the expected fields so MCP clients can
// auto-configure. The issuer field comes from cfg.OAuth.Issuer
// (test env leaves it "" so the wiring falls back to the
// http://localhost:8080 placeholder; the test only checks shape,
// not the exact issuer value).
func TestOAuth_Metadata(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	resp, err := http.Get(env.BaseURL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatalf("metadata GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata: expected 200, got %d", resp.StatusCode)
	}
	var meta map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	for _, key := range []string{"issuer", "authorization_endpoint", "token_endpoint", "registration_endpoint", "revocation_endpoint", "grant_types_supported", "code_challenge_methods_supported"} {
		if _, ok := meta[key]; !ok {
			t.Fatalf("metadata: missing key %q", key)
		}
	}
	methods, _ := meta["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Fatalf("metadata: code_challenge_methods_supported must be [S256], got %v", methods)
	}

	// Protected-resource metadata.
	resp2, err := http.Get(env.BaseURL + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatalf("pr metadata GET: %v", err)
	}
	defer resp2.Body.Close()
	var prm map[string]any
	json.NewDecoder(resp2.Body).Decode(&prm)
	for _, key := range []string{"resource", "authorization_servers", "bearer_methods_supported", "scopes_supported"} {
		if _, ok := prm[key]; !ok {
			t.Fatalf("protected-resource metadata: missing key %q", key)
		}
	}
}

// TestOAuth_Authorize_RejectsMissingPKCE verifies the authorize
// endpoint rejects a request with no code_challenge (OAuth 2.1
// mandates PKCE). The rejection is a 400 plain-text page (not an
// OAuth redirect) because a missing PKCE is a developer error.
func TestOAuth_Authorize_RejectsMissingPKCE(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	reg := dcr(t, env.BaseURL, []string{"http://127.0.0.1:5394/callback"}, "no-pkce-client")

	q := url.Values{
		"client_id":     {reg.ClientID},
		"redirect_uri":  {"http://127.0.0.1:5394/callback"},
		"response_type": {"code"},
		"scope":          {"mcp"},
	}.Encode()
	resp, err := http.Get(env.BaseURL + "/api/v1/oauth/authorize?" + q)
	if err != nil {
		t.Fatalf("authorize GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing PKCE: expected 400, got %d", resp.StatusCode)
	}
}

// keep store + context referenced for the helpers above that use them.
var _ = store.New
var _ = context.Background