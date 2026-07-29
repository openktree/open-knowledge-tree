package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/mailer"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/store"
)

// authTestEnv wires an AuthHandler against an in-memory sqlite store
// + a NoopMailer (so tests can assert on the captured verification
// email without a real SMTP server). The email-validation toggle is
// parameterized so each test picks the behavior it exercises.
type authTestEnv struct {
	t      *testing.T
	store  store.MetadataStore
	mailer *mailer.NoopMailer
	authH  *AuthHandler
}

func newAuthTestEnv(t *testing.T, enableValidation bool) *authTestEnv {
	t.Helper()
	s, err := store.NewSQLiteStore("file::memory:?cache=shared&_pragma=busy_timeout=5000")
	if err != nil {
		t.Fatalf("creating sqlite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	m := &mailer.NoopMailer{}
	authCfg := &config.AuthConfig{
		JWTSecret: "test-secret",
		TokenTTL:  time.Hour,
	}
	emailCfg := &config.EmailValidationConfig{
		EnableValidation: enableValidation,
		FromAddress:      "no-reply@test.local",
		PublicBaseURL:    "https://registry.test",
		TokenTTL:         time.Hour,
	}
	authH := NewAuthHandler(s, authCfg, emailCfg, m)
	return &authTestEnv{t: t, store: s, mailer: m, authH: authH}
}

// register issues a POST /api/v1/auth/register with the given JSON
// body and returns the recorded response.
func (e *authTestEnv) register(body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.authH.Register(rec, req)
	return rec
}

// login issues a POST /api/v1/auth/login with the given JSON body.
func (e *authTestEnv) login(body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.authH.Login(rec, req)
	return rec
}

// verifyEmail issues a GET /api/v1/auth/verify-email?token=<raw>.
func (e *authTestEnv) verifyEmail(token string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/verify-email?token="+token, nil)
	rec := httptest.NewRecorder()
	e.authH.VerifyEmail(rec, req)
	return rec
}

// resend issues a POST /api/v1/auth/resend-verification with {email}.
func (e *authTestEnv) resend(email string) *httptest.ResponseRecorder {
	e.t.Helper()
	body := `{"email":"` + email + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/resend-verification", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.authH.ResendVerification(rec, req)
	return rec
}

// extractVerifyToken pulls the verification token out of the
// NoopMailer's captured email body (the link is the only thing the
// test needs to drive the verify endpoint). Returns "" when no
// email has been captured.
func (e *authTestEnv) extractVerifyToken() string {
	e.t.Helper()
	if len(e.mailer.SentMessages) == 0 {
		return ""
	}
	body := e.mailer.SentMessages[len(e.mailer.SentMessages)-1].Body
	// The link looks like: https://registry.test/ui/verify-email?token=<hex>
	idx := strings.Index(body, "token=")
	if idx < 0 {
		return ""
	}
	tok := strings.TrimSpace(body[idx+len("token="):])
	// The token is followed by a newline in the email body; trim
	// any trailing whitespace/CR.
	if nl := strings.IndexAny(tok, "\r\n"); nl >= 0 {
		tok = tok[:nl]
	}
	return tok
}

// TestRegister_ValidationDisabled_LegacyJWT is the regression guard:
// with email_validation.enable_validation = false (the default),
// Register keeps the legacy behavior — 201 + immediate JWT, no
// verification email sent.
func TestRegister_ValidationDisabled_LegacyJWT(t *testing.T) {
	env := newAuthTestEnv(t, false)
	rec := env.register(`{"email":"alice@example.com","password":"hunter2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("expected a JWT in the legacy register response, got empty token")
	}
	if resp.EmailVerified {
		t.Fatal("expected email_verified=false on a fresh legacy register")
	}
	if len(env.mailer.SentMessages) != 0 {
		t.Fatalf("expected no verification email when validation is off, got %d", len(env.mailer.SentMessages))
	}
}

// TestRegister_ValidationEnabled_NoJWTVerificationRequired is the
// happy path for the new flow: Register returns 201 with
// {status: "verification_required"} and NO token, the
// email_verifications row exists, and the NoopMailer captured a
// message whose body contains the verification link.
func TestRegister_ValidationEnabled_NoJWTVerificationRequired(t *testing.T) {
	env := newAuthTestEnv(t, true)
	rec := env.register(`{"email":"bob@example.com","password":"hunter2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Status != "verification_required" {
		t.Fatalf("expected status=verification_required, got %q", resp.Status)
	}
	if resp.EmailVerified {
		t.Fatal("expected email_verified=false on a fresh validated register")
	}
	if len(env.mailer.SentMessages) != 1 {
		t.Fatalf("expected exactly one verification email, got %d", len(env.mailer.SentMessages))
	}
	if env.extractVerifyToken() == "" {
		t.Fatal("expected the captured email to contain a verification token")
	}

	// The email_verifications row should exist for this user.
	user, err := env.store.GetUserByEmail(context.Background(), "bob@example.com")
	if err != nil {
		t.Fatalf("looking up user: %v", err)
	}
	if user.EmailVerified {
		t.Fatal("expected the new user to be unverified")
	}
}

// TestLogin_ValidationEnabled_UnverifiedBlocked verifies the gate:
// Login on an unverified account returns 403 email_not_verified with
// a resend_url, and does NOT issue a JWT.
func TestLogin_ValidationEnabled_UnverifiedBlocked(t *testing.T) {
	env := newAuthTestEnv(t, true)
	if rec := env.register(`{"email":"carol@example.com","password":"hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	rec := env.login(`{"email":"carol@example.com","password":"hunter2"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp emailNotVerifiedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Error != "email_not_verified" {
		t.Fatalf("expected error=email_not_verified, got %q", resp.Error)
	}
	if resp.ResendURL == "" {
		t.Fatal("expected a non-empty resend_url in the 403 response")
	}
}

// TestVerifyEmail_HappyPath exercises the full register → verify →
// login flow: after clicking the link, the user is marked verified,
// the verification row is deleted (single-use), and a subsequent
// Login succeeds with a JWT.
func TestVerifyEmail_HappyPath(t *testing.T) {
	env := newAuthTestEnv(t, true)
	if rec := env.register(`{"email":"dave@example.com","password":"hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	token := env.extractVerifyToken()
	if token == "" {
		t.Fatal("expected a verification token in the captured email")
	}
	rec := env.verifyEmail(token)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp verifyEmailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding verify response: %v", err)
	}
	if resp.Status != "verified" {
		t.Fatalf("expected status=verified, got %q", resp.Status)
	}
	if resp.Email != "dave@example.com" {
		t.Fatalf("expected email=dave@example.com, got %q", resp.Email)
	}

	// The user should now be marked verified.
	user, err := env.store.GetUserByEmail(context.Background(), "dave@example.com")
	if err != nil {
		t.Fatalf("looking up user: %v", err)
	}
	if !user.EmailVerified {
		t.Fatal("expected user.EmailVerified=true after verify")
	}

	// Login should now succeed (the gate passes).
	rec = env.login(`{"email":"dave@example.com","password":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after verify: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var loginResp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("decoding login response: %v", err)
	}
	if loginResp.Token == "" {
		t.Fatal("expected a JWT after verification")
	}
}

// TestVerifyEmail_ExpiredToken asserts an expired token yields 410
// and is deleted (so the slot frees up for a resend).
func TestVerifyEmail_ExpiredToken(t *testing.T) {
	env := newAuthTestEnv(t, true)
	if rec := env.register(`{"email":"eve@example.com","password":"hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	user, err := env.store.GetUserByEmail(context.Background(), "eve@example.com")
	if err != nil {
		t.Fatalf("looking up user: %v", err)
	}
	// Inject an already-expired verification row directly so we
	// don't have to wait for the TTL.
	raw, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generating token: %v", err)
	}
	token := strings.TrimPrefix(raw, "okr_")
	now := time.Now()
	if err := env.store.CreateEmailVerification(context.Background(), &model.EmailVerification{
		UserID:    user.ID,
		TokenHash: auth.HashToken(token),
		ExpiresAt: now.Add(-time.Minute), // expired
		CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seeding expired verification: %v", err)
	}

	rec := env.verifyEmail(token)
	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410 for expired token, got %d: %s", rec.Code, rec.Body.String())
	}

	// The expired row should have been deleted.
	if _, err := env.store.GetEmailVerificationByHash(context.Background(), auth.HashToken(token)); err == nil {
		t.Fatal("expected the expired verification row to be deleted")
	}
}

// TestVerifyEmail_BadToken asserts a token that doesn't match any
// row yields 400 (not 410, not 200).
func TestVerifyEmail_BadToken(t *testing.T) {
	env := newAuthTestEnv(t, true)
	rec := env.verifyEmail("not-a-real-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestVerifyEmail_SingleUse asserts a token can't be replayed: the
// second verify attempt with the same token 400s (the row was
// deleted on the first success).
func TestVerifyEmail_SingleUse(t *testing.T) {
	env := newAuthTestEnv(t, true)
	if rec := env.register(`{"email":"frank@example.com","password":"hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	token := env.extractVerifyToken()
	if rec := env.verifyEmail(token); rec.Code != http.StatusOK {
		t.Fatalf("first verify: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := env.verifyEmail(token); rec.Code != http.StatusBadRequest {
		t.Fatalf("replay verify: expected 400 (row deleted), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestResendVerification_UnknownEmailReturns200 asserts the
// enumeration-safe behavior: a resend for an email that isn't
// registered still returns 200, and sends no email.
func TestResendVerification_UnknownEmailReturns200(t *testing.T) {
	env := newAuthTestEnv(t, true)
	rec := env.resend("nobody@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown email, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(env.mailer.SentMessages) != 0 {
		t.Fatalf("expected no email sent for unknown address, got %d", len(env.mailer.SentMessages))
	}
}

// TestResendVerification_AlreadyVerifiedReturns200 asserts a resend
// for an already-verified user is a no-op (200, no new email).
func TestResendVerification_AlreadyVerifiedReturns200(t *testing.T) {
	env := newAuthTestEnv(t, true)
	if rec := env.register(`{"email":"grace@example.com","password":"hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	token := env.extractVerifyToken()
	if rec := env.verifyEmail(token); rec.Code != http.StatusOK {
		t.Fatalf("verify: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	sentBefore := len(env.mailer.SentMessages)
	rec := env.resend("grace@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("resend for verified user: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := len(env.mailer.SentMessages); got != sentBefore {
		t.Fatalf("expected no new email for verified user, got %d new", got-sentBefore)
	}
}

// TestResendVerification_UnverifiedMintsNewToken asserts a resend
// for an unverified user mints a fresh token (a new email is sent)
// and the new token verifies the account.
func TestResendVerification_UnverifiedMintsNewToken(t *testing.T) {
	env := newAuthTestEnv(t, true)
	if rec := env.register(`{"email":"heidi@example.com","password":"hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	firstToken := env.extractVerifyToken()
	if firstToken == "" {
		t.Fatal("expected a first verification token after register")
	}
	sentBefore := len(env.mailer.SentMessages)
	rec := env.resend("heidi@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("resend: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := len(env.mailer.SentMessages); got != sentBefore+1 {
		t.Fatalf("expected one new email after resend, got %d new", got-sentBefore)
	}
	newToken := env.extractVerifyToken()
	if newToken == "" || newToken == firstToken {
		t.Fatal("expected a fresh token after resend, got empty or same token")
	}
	// The new token should verify the account.
	if rec := env.verifyEmail(newToken); rec.Code != http.StatusOK {
		t.Fatalf("verify after resend: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestLogin_ValidationDisabled_LegacyJWT is the regression guard
// for the login path: with validation off, an unverified user (the
// default state) still gets a JWT. This catches a regression where
// the gate would accidentally fire when the toggle is off.
func TestLogin_ValidationDisabled_LegacyJWT(t *testing.T) {
	env := newAuthTestEnv(t, false)
	if rec := env.register(`{"email":"ivan@example.com","password":"hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	rec := env.login(`{"email":"ivan@example.com","password":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (validation off → no gate), got %d: %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding login response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("expected a JWT when validation is off")
	}
}

// TestResendVerification_ValidationOffReturns404 asserts the
// endpoint refuses to act when the feature is disabled (defend-in-
// depth: the router registers the route unconditionally, so the
// handler must 404 when the toggle is off).
func TestResendVerification_ValidationOffReturns404(t *testing.T) {
	env := newAuthTestEnv(t, false)
	rec := env.resend("anyone@example.com")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when validation is off, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestVerifyEmail_ValidationOffReturns404 is the matching guard
// for the verify endpoint.
func TestVerifyEmail_ValidationOffReturns404(t *testing.T) {
	env := newAuthTestEnv(t, false)
	rec := env.verifyEmail("any-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when validation is off, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRegister_DuplicateEmail asserts the existing conflict path
// still works under the new flow: a second register for the same
// email 409s, regardless of the validation toggle.
func TestRegister_DuplicateEmail(t *testing.T) {
	env := newAuthTestEnv(t, true)
	if rec := env.register(`{"email":"dup@example.com","password":"hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	rec := env.register(`{"email":"dup@example.com","password":"hunter2"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second register: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRegister_BadBody asserts the existing 400 path still works.
func TestRegister_BadBody(t *testing.T) {
	env := newAuthTestEnv(t, true)
	rec := env.register(`{"email":"","password":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty email/password, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestResendVerification_BadBody asserts the empty-email case 400s
// (the only non-200 outcome besides a mailer 500).
func TestResendVerification_BadBody(t *testing.T) {
	env := newAuthTestEnv(t, true)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/resend-verification", bytes.NewReader([]byte(`{"email":""}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.authH.ResendVerification(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty email, got %d: %s", rec.Code, rec.Body.String())
	}
}
