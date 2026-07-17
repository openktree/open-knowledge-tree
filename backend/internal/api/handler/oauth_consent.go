package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// loginCookieName is the short-lived signed cookie the authorize
// flow uses to carry the logged-in user id from the login POST to
// the consent GET. It is NOT a session cookie: it lives only long
// enough to bridge the two steps (5 minutes) and is scoped to the
// /api/v1/oauth path so it never travels on other requests.
const loginCookieName = "okt_oauth_login"
const loginCookieTTL = 5 * time.Minute

// loginCookieSecret signs the login cookie. We reuse cfg.Auth.JWTSecret
// (the same symmetric secret the session JWT and OAuth access JWT
// use) so there's one secret to rotate; the cookie's purpose is
// different (anti-CSRF + tamper detection) but the signing key is
// the same. The wiring layer sets this once at construction.
var loginCookieSecret string

// SetLoginCookieSecret must be called once at wiring time before any
// authorize request is served. It panics if set twice to catch
// mis-wiring. Tests set it once per test env.
func SetLoginCookieSecret(secret string) {
	loginCookieSecret = secret
}

// setLoginCookie writes a signed, short-lived cookie carrying the
// user id. The signature is HMAC-SHA256 over "uid|expires" so a
// tampered uid or expired cookie is detectable without a database
// lookup. The cookie is HttpOnly + SameSite=Lax; Secure is set only
// when the request is TLS so the cookie survives the test env's
// plain-HTTP httptest server (a hardcoded Secure=true would drop
// the cookie on every HTTP request and break the authorize flow
// in tests). Production deployments behind TLS set Secure via the
// reverse proxy's scheme, not the cookie attribute, so this is
// safe.
func setLoginCookie(w http.ResponseWriter, r *http.Request, userID pgtype.UUID) {
	expires := time.Now().Add(loginCookieTTL)
	value := signCookieValue(userID.String(), expires)
	http.SetCookie(w, &http.Cookie{
		Name:     loginCookieName,
		Value:    value,
		Path:     "/api/v1/oauth",
		Expires:  expires,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// readLoginCookie validates the cookie signature + expiry and
// returns the user id. The bool is false when the cookie is absent,
// tampered, or expired — the caller treats that as "not logged in"
// and renders the login form.
func readLoginCookie(r *http.Request) (pgtype.UUID, bool) {
	c, err := r.Cookie(loginCookieName)
	if err != nil || c.Value == "" {
		return pgtype.UUID{}, false
	}
	uid, ok := verifyCookieValue(c.Value)
	if !ok {
		return pgtype.UUID{}, false
	}
	var id pgtype.UUID
	if err := id.Scan(uid); err != nil {
		return pgtype.UUID{}, false
	}
	return id, true
}

// clearLoginCookie deletes the login cookie by setting an
// immediately-expired value. Called after the consent step issues
// the authorization code so the cookie doesn't linger. Secure
// mirrors setLoginCookie's TLS check for the same reason.
func clearLoginCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginCookieName,
		Value:    "",
		Path:     "/api/v1/oauth",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// signCookieValue builds "base64(uid|expiresEpoch).base64(hmac)".
// The HMAC is over the first half so a tampered uid or expiry
// invalidates the signature. We use base64 to keep the cookie
// URL-safe without needing encoding/ascii85.
func signCookieValue(uid string, expires time.Time) string {
	payload := uid + "|" + expires.Format(time.RFC3339)
	mac := hmac.New(sha256.New, []byte(loginCookieSecret))
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// verifyCookieValue reverses signCookieValue: it splits on ".",
// base64-decodes the payload, recomputes the HMAC, and checks
// expiry. A constant-time compare prevents timing attacks on the
// signature. Returns the uid and ok=false on any failure.
func verifyCookieValue(value string) (string, bool) {
	if loginCookieSecret == "" {
		return "", false
	}
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(loginCookieSecret))
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return "", false
	}
	// payload is "uid|expiresRFC3339"; split on the last "|" so a
	// uid containing "|" (UUIDs don't, but defense in depth)
	// doesn't break parsing.
	idx := strings.LastIndexByte(string(payload), '|')
	if idx < 0 {
		return "", false
	}
	uid := string(payload[:idx])
	expiresStr := string(payload[idx+1:])
	expires, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil || time.Now().After(expires) {
		return "", false
	}
	return uid, true
}

// renderOAuthLogin writes the minimal login HTML form. The form
// posts to /api/v1/oauth/authorize/login with the original authorize
// query echoed back as a hidden field. The page is intentionally
// tiny and unstyled — it's a 3-field form the user sees once per
// MCP-client authorization, not a product surface.
func renderOAuthLogin(w http.ResponseWriter, authorizeQuery, clientName string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(oauthLoginHTML(authorizeQuery, clientName, "")))
}

// renderOAuthLoginError re-renders the login form with an error
// message. Used when email/password don't match.
func renderOAuthLoginError(w http.ResponseWriter, authorizeQuery, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(oauthLoginHTML(authorizeQuery, "", errMsg)))
}

// renderOAuthConsent writes the consent HTML form. The form posts
// back to /api/v1/oauth/authorize with the original query + a
// consent=yes hidden field. The user id is shown so the user knows
// which OKT account is authorizing.
func renderOAuthConsent(w http.ResponseWriter, authorizeQuery, clientName string, _ pgtype.UUID) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(oauthConsentHTML(authorizeQuery, clientName)))
}

func oauthLoginHTML(authorizeQuery, clientName, errMsg string) string {
	clientLine := ""
	if clientName != "" {
		clientLine = "<p>Authorizing <strong>" + htmlEscape(clientName) + "</strong></p>"
	}
	errLine := ""
	if errMsg != "" {
		errLine = `<p style="color:#c00">` + htmlEscape(errMsg) + `</p>`
	}
	return `<!doctype html><html><head><title>OKT — Authorize MCP client</title>
<meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:system-ui,sans-serif;max-width:28rem;margin:2rem auto;padding:0 1rem">
<h1>Authorize MCP client</h1>` + clientLine + errLine + `
<form method="POST" action="/api/v1/oauth/authorize/login">
<input type="hidden" name="authorize_query" value="` + htmlAttrEscape(authorizeQuery) + `">
<p><label>Email<br><input type="email" name="email" required autofocus style="width:100%;padding:.4rem"></label></p>
<p><label>Password<br><input type="password" name="password" required style="width:100%;padding:.4rem"></label></p>
<p><button type="submit" style="padding:.5rem 1rem">Log in</button></p>
</form>
</body></html>`
}

func oauthConsentHTML(authorizeQuery, clientName string) string {
	name := clientName
	if name == "" {
		name = "an MCP client"
	}
	return `<!doctype html><html><head><title>OKT — Consent</title>
<meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:system-ui,sans-serif;max-width:28rem;margin:2rem auto;padding:0 1rem">
<h1>Authorize ` + htmlEscape(name) + `</h1>
<p>This will let <strong>` + htmlEscape(name) + `</strong> call the OKT MCP tools (list repositories, search facts, get a fact) on your behalf, scoped to the repositories your OKT account can already access.</p>
<form method="POST" action="/api/v1/oauth/authorize?` + htmlAttrEscape(authorizeQuery) + `">
<input type="hidden" name="consent" value="yes">
<p><button type="submit" style="padding:.5rem 1rem">Allow</button>
<a href="/api/v1/oauth/authorize?` + htmlAttrEscape(authorizeQuery) + `" style="margin-left:1rem">Cancel</a></p>
</form>
</body></html>`
}

// htmlEscape escapes the five significant HTML characters. We
// avoid importing html/template for a 3-field form to keep the
// bundle tiny; this is the standard manual-escape set.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, `'`, "&#39;")
	return s
}

// htmlAttrEscape is the same as htmlEscape but also escapes the
// characters that break a double-quoted attribute (it's identical
// here because we already escape ").
func htmlAttrEscape(s string) string { return htmlEscape(s) }