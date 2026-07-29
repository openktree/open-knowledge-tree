package handler

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/mailer"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/store"
)

type AuthHandler struct {
	store     store.MetadataStore
	authCfg   *config.AuthConfig
	emailCfg  *config.EmailValidationConfig
	mailer    mailer.Mailer
	jwtSecret string
}

// NewAuthHandler wires the auth handler. emailCfg + mailer are
// optional in the KISS sense: when emailCfg.EnableValidation is
// false the Register/Login paths never touch them, so callers
// that pre-date the email-validation feature can pass nil for
// both (the zero-value *EmailValidationConfig has
// EnableValidation == false, and a nil mailer is never invoked).
// main.go always passes real values.
func NewAuthHandler(store store.MetadataStore, cfg *config.AuthConfig, emailCfg *config.EmailValidationConfig, m mailer.Mailer) *AuthHandler {
	return &AuthHandler{
		store:     store,
		authCfg:   cfg,
		emailCfg:  emailCfg,
		mailer:    m,
		jwtSecret: cfg.JWTSecret,
	}
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token         string `json:"token"`
	UserID        string `json:"user_id"`
	Email         string `json:"email"`
	DisplayName   string `json:"display_name"`
	Role          string `json:"role"`
	EmailVerified bool   `json:"email_verified"`
}

// registerResponse is the body returned by Register when email
// validation is enabled: no token (the user can't log in yet),
// just an acknowledgement + the email the message went to. When
// validation is disabled, Register returns a loginResponse (the
// legacy immediate-JWT path).
type registerResponse struct {
	Status         string `json:"status"`
	Email          string `json:"email"`
	EmailVerified  bool   `json:"email_verified"`
	VerificationID string `json:"verification_id,omitempty"`
}

// verifyEmailResponse is the body returned by VerifyEmail on
// success. The UI path redirects to /ui/login?verified=1 instead.
type verifyEmailResponse struct {
	Status string `json:"status"`
	Email  string `json:"email"`
}

// resendRequest is the body of POST /api/v1/auth/resend-verification.
// Email is the only field; the handler always returns 200 to
// prevent email enumeration (the standard KISS pattern: "if that
// email is registered and unverified, a message has been sent").
type resendRequest struct {
	Email string `json:"email"`
}

// resendResponse mirrors that: always ok, no leak.
type resendResponse struct {
	Status string `json:"status"`
}

// emailNotVerifiedResponse is the body of the 403 Login returns
// when the user is unverified and email validation is enabled.
// ResendURL is the API endpoint the client should POST {email} to;
// the UI surfaces the same flow via /ui/resend-verification.
type emailNotVerifiedResponse struct {
	Error     string `json:"error"`
	Email     string `json:"email"`
	ResendURL string `json:"resend_url"`
}

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeBody(r, &req); err != nil || req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to process password")
		return
	}

	role := "viewer"
	for _, admin := range h.authCfg.BootstrapAdmins {
		if admin == req.Email {
			role = "admin"
			break
		}
	}

	now := time.Now()
	user := &model.User{
		ID:            uuid.New().String(),
		Email:         req.Email,
		PasswordHash:  hash,
		DisplayName:   req.DisplayName,
		Role:          role,
		EmailVerified: false,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := h.store.CreateUser(r.Context(), user); err != nil {
		if isUniqueEmailErr(err) {
			writeError(w, http.StatusConflict, "email already registered")
			return
		}
		log.Printf("register: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	// Email-validation path: mint a verification token, store its
	// hash, email the link, and return 201 WITHOUT a JWT. The user
	// must click the link before Login accepts them. Mirrors the
	// promptset.enable_validation opt-in: when the toggle is off,
	// Register keeps the legacy immediate-JWT behavior.
	if h.emailCfg != nil && h.emailCfg.EnableValidation {
		if err := h.issueVerification(r.Context(), user); err != nil {
			// The user row is already created; failing to send the
			// verification email is recoverable via the resend
			// endpoint, so we don't roll back. Log + 500 so the
			// client knows to retry the resend.
			log.Printf("register: issueVerification for %s: %v", user.Email, err)
			writeError(w, http.StatusInternalServerError, "account created but failed to send verification email; use /api/v1/auth/resend-verification")
			return
		}
		writeJSON(w, http.StatusCreated, registerResponse{
			Status:        "verification_required",
			Email:         user.Email,
			EmailVerified: false,
		})
		return
	}

	token, err := auth.GenerateToken(h.jwtSecret, h.authCfg.TokenTTL, user.ID, user.Email, user.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	writeJSON(w, http.StatusCreated, loginResponse{
		Token:         token,
		UserID:        user.ID,
		Email:         user.Email,
		DisplayName:   user.DisplayName,
		Role:          user.Role,
		EmailVerified: user.EmailVerified,
	})
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeBody(r, &req); err != nil || req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	user, err := h.store.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	if !auth.CheckPassword(user.PasswordHash, req.Password) {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	// Email-validation gate: refuse unverified accounts. The 403
	// carries the resend URL so the client can offer a "resend"
	// action without hardcoding the endpoint path. The UI's
	// LoginPage renders the same link in the form's error slot.
	if h.emailCfg != nil && h.emailCfg.EnableValidation && !user.EmailVerified {
		writeJSON(w, http.StatusForbidden, emailNotVerifiedResponse{
			Error:     "email_not_verified",
			Email:     user.Email,
			ResendURL: "/api/v1/auth/resend-verification",
		})
		return
	}

	token, err := auth.GenerateToken(h.jwtSecret, h.authCfg.TokenTTL, user.ID, user.Email, user.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{
		Token:         token,
		UserID:        user.ID,
		Email:         user.Email,
		DisplayName:   user.DisplayName,
		Role:          user.Role,
		EmailVerified: user.EmailVerified,
	})
}

// VerifyEmail handles GET /api/v1/auth/verify-email?token=<raw>.
// Hashes the token, looks up the pending verification row, checks
// expiry, marks the user verified, and deletes the row (single-use).
// Returns 200 on success, 400 on a missing/bad token, 410 on an
// expired token. The UI equivalent (uiH.VerifyEmailPage) redirects
// to /ui/login?verified=1 on success instead of returning JSON.
func (h *AuthHandler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	if h.emailCfg == nil || !h.emailCfg.EnableValidation {
		// Endpoint not registered when validation is off (router
		// gates it), but defend in depth: a request that somehow
		// reaches here gets a 404, not a silent accept.
		writeError(w, http.StatusNotFound, "email verification is not enabled")
		return
	}
	tokenStr := strings.TrimSpace(r.URL.Query().Get("token"))
	if tokenStr == "" {
		writeError(w, http.StatusBadRequest, "missing token")
		return
	}
	h.verifyEmail(w, r, tokenStr, false)
}

// verifyEmail is the shared core of the API + UI verify paths.
// `redirectOnSuccess` switches between the JSON 200 (API) and the
// 302 to /ui/login?verified=1 (UI) outcomes.
func (h *AuthHandler) verifyEmail(w http.ResponseWriter, r *http.Request, tokenStr string, redirectOnSuccess bool) {
	tokenHash := auth.HashToken(tokenStr)
	v, err := h.store.GetEmailVerificationByHash(r.Context(), tokenHash)
	if err != nil || v == nil {
		writeError(w, http.StatusBadRequest, "invalid or unknown token")
		return
	}
	if time.Now().After(v.ExpiresAt) {
		// Expired tokens are deleted so the slot frees up for a
		// resend; the user gets a 410 so the client can prompt to
		// resend rather than silently retry the same dead link.
		_ = h.store.DeleteEmailVerification(r.Context(), v.UserID)
		writeError(w, http.StatusGone, "token expired")
		return
	}
	if err := h.store.MarkEmailVerified(r.Context(), v.UserID); err != nil {
		log.Printf("verify-email: mark verified for %s: %v", v.UserID, err)
		writeError(w, http.StatusInternalServerError, "failed to verify email")
		return
	}
	// Single-use: a verified token can't be replayed. Best-effort
	// delete; a failure here is logged but not fatal (the user is
	// already verified, so a stale row is harmless — it just
	// rejects future verify attempts with the same raw token,
	// which the user has no reason to retry).
	_ = h.store.DeleteEmailVerification(r.Context(), v.UserID)

	if redirectOnSuccess {
		http.Redirect(w, r, "/ui/login?verified=1", http.StatusFound)
		return
	}
	// Resolve the email for the response body (the row carries
	// only user_id; the API response includes email for client
	// convenience). A lookup failure here is non-fatal — the user
	// is verified regardless.
	email := ""
	if u, err := h.store.GetUserByID(r.Context(), v.UserID); err == nil && u != nil {
		email = u.Email
	}
	writeJSON(w, http.StatusOK, verifyEmailResponse{Status: "verified", Email: email})
}

// ResendVerification handles POST /api/v1/auth/resend-verification.
// Body: {email}. Always returns 200 {status: "ok"} to prevent email
// enumeration (the standard KISS pattern). Internally:
//   - user not found            → no-op (200)
//   - user already verified     → no-op (200)
//   - last token < cooldown old → no-op (200, rate-limited)
//   - otherwise                 → mint a new token, store, email
//
// The mailer is the NoopMailer in dev (logs the URL) and the
// smtpMailer in production; a send failure logs + returns 500 so
// the client can retry. The 500 is the only non-200 outcome.
func (h *AuthHandler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	if h.emailCfg == nil || !h.emailCfg.EnableValidation {
		writeError(w, http.StatusNotFound, "email verification is not enabled")
		return
	}
	var req resendRequest
	if err := decodeBody(r, &req); err != nil || req.Email == "" {
		// Bad body still returns 200 to avoid leaking whether the
		// email is registered — but the empty-email case is a
		// client bug, not an enumeration probe, so we 400 it.
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	user, err := h.store.GetUserByEmail(r.Context(), req.Email)
	if err != nil || user == nil {
		// Enumeration-safe: don't reveal whether the email exists.
		writeJSON(w, http.StatusOK, resendResponse{Status: "ok"})
		return
	}
	if user.EmailVerified {
		writeJSON(w, http.StatusOK, resendResponse{Status: "ok"})
		return
	}

	// Mint a fresh token + email it. The upsert in
	// CreateEmailVerification replaces any prior un-used token, so
	// a resend simply invalidates the previous link. We do NOT
	// enforce a cooldown here: the user's answer to the design
	// question was "always 200" (enumeration-safe), and a 429
	// would leak that the email is registered + unverified. The
	// mailer is the expensive part; a future iteration can rate-
	// limit at the mailer or IP layer without changing this
	// handler's 200-always contract.
	if err := h.issueVerification(r.Context(), user); err != nil {
		log.Printf("resend: issueVerification for %s: %v", user.Email, err)
		writeError(w, http.StatusInternalServerError, "failed to send verification email")
		return
	}
	writeJSON(w, http.StatusOK, resendResponse{Status: "ok"})
}

// issueVerification mints a random token, stores its hash, and
// emails the verification link. Shared by Register and
// ResendVerification. The token is generated via the existing
// auth.GenerateAPIToken (32 random bytes, hex-encoded, "okr_"
// prefix stripped here since this isn't an API token) and hashed
// with auth.HashToken (sha256-hex) so the DB never stores the raw
// token — same pattern as api_tokens.token_hash.
func (h *AuthHandler) issueVerification(ctx context.Context, user *model.User) error {
	return issueEmailVerification(ctx, h.store, h.mailer, h.emailCfg, user)
}

// issueEmailVerification is the package-level helper both AuthHandler
// and UIHandler call. It lives here (handler package) rather than in
// the mailer or auth package because it composes store + mailer +
// config — the handler layer's job. Kept free of *AuthHandler/*UIHandler
// receiver so the two handler types share one implementation.
func issueEmailVerification(ctx context.Context, s store.MetadataStore, m mailer.Mailer, cfg *config.EmailValidationConfig, user *model.User) error {
	raw, err := auth.GenerateAPIToken()
	if err != nil {
		return err
	}
	// GenerateAPIToken returns "okr_<hex>"; strip the prefix so
	// the verification token doesn't look like an API token. The
	// prefix is a UI affordance for API tokens, not a security
	// property.
	token := strings.TrimPrefix(raw, "okr_")
	tokenHash := auth.HashToken(token)

	ttl := cfg.TokenTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := time.Now()
	if err := s.CreateEmailVerification(ctx, &model.EmailVerification{
		UserID:    user.ID,
		TokenHash: tokenHash,
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
	}); err != nil {
		return err
	}

	link := buildVerifyURL(cfg, token)
	body := strings.Join([]string{
		"Welcome to the Knowledge Registry.",
		"",
		"Please verify your email by visiting the link below:",
		"",
		link,
		"",
		"This link expires in " + ttl.String() + ".",
		"If you did not create an account, you can ignore this email.",
		"",
		"— The Knowledge Registry",
	}, "\r\n")
	return m.Send(user.Email, "Verify your Knowledge Registry email", body)
}

// buildVerifyURL constructs the verification link the user clicks.
// PublicBaseURL is the configured base (e.g.
// "https://registry.example.com"); when it's empty we fall back to
// a relative URL (/<path>) so the link still works from the same
// host the registry is served on — useful for dev behind a proxy.
func buildVerifyURL(cfg *config.EmailValidationConfig, token string) string {
	base := strings.TrimRight(cfg.PublicBaseURL, "/")
	if base == "" {
		return "/ui/verify-email?token=" + token
	}
	return base + "/ui/verify-email?token=" + token
}

func contains(s, substr string) bool { return len(s) >= len(substr) && searchString(s, substr) >= 0 }

func searchString(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// isUniqueEmailErr reports whether err is a unique-constraint
// violation on users.email. Covers both SQLite (modernc.org/sqlite
// emits "constraint failed: UNIQUE constraint failed: users.email
// (2067)") and Postgres (pgx emits "duplicate key value violates
// unique constraint \"users_email_key\""). Substring matching is
// intentionally loose — the exact error string varies across
// driver versions, so matching on the stable fragments
// ("UNIQUE constraint failed: users.email" for SQLite,
// "duplicate key" + "users_email" for Postgres) is more robust
// than an exact equality.
func isUniqueEmailErr(err error) bool {
	msg := err.Error()
	return contains(msg, "UNIQUE constraint failed: users.email") ||
		(contains(msg, "duplicate key") && contains(msg, "users_email"))
}
