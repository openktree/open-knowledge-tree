package handler

import (
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/mailer"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/service"
	"github.com/openktree/knowledge-registry/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type UIHandler struct {
	store    store.MetadataStore
	svc      *service.Registry
	authCfg  *config.AuthConfig
	emailCfg *config.EmailValidationConfig
	mailer   mailer.Mailer
	tmpl     *template.Template
}

func NewUIHandler(store store.MetadataStore, svc *service.Registry, cfg *config.AuthConfig, emailCfg *config.EmailValidationConfig, m mailer.Mailer) *UIHandler {
	sub, err := fs.Sub(templateFS, "templates")
	if err != nil {
		panic(err)
	}
	tmpl := template.Must(template.New("").Funcs(template.FuncMap{
		// Pagination math helpers used by the list-page
		// templates. `add` and `sub` are simple int ops;
		// they panic on type mismatch (the templates only
		// ever pass pageData.Limit / pageData.Offset, both
		// ints), so a template bug surfaces immediately at
		// render time rather than producing wrong links.
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}).ParseFS(sub, "*.html"))
	return &UIHandler{store: store, svc: svc, authCfg: cfg, emailCfg: emailCfg, mailer: m, tmpl: tmpl}
}

type pageData struct {
	UserID  string
	IsAdmin bool
	Error   string
	Success string
	// ResendURL, when set, renders a "Resend verification email"
	// link below the error slot on the login page. Used by the
	// email-validation gate so the user can re-trigger the
	// verification flow without leaving the login page. Empty
	// means no link is rendered.
	ResendURL string
	// PrefillEmail pre-fills the email field on the resend-
	// verification form. Set from ?email= on the login page's
	// resend link so the user doesn't retype it.
	PrefillEmail string
	Token        string
	NewToken     string
	Tokens       interface{}
	Users        interface{}
	// Browser pages (sources/graphs/users/tokens) populate these.
	Sources      interface{}
	SourcesTotal int
	Graphs       interface{}
	GraphsTotal  int
	// Detail-page fields. SourceDetail / GraphDetail populate
	// these; list pages leave them nil. The template asserts on
	// the right shape (the SourceMeta / GraphMeta struct).
	Source       *model.SourceMeta
	Graph        *model.GraphMeta
	Decomps      []model.DecompMeta
	PresignedSrc string
	// BackURL is rendered as a "← Back to ..." link at the top
	// of detail pages. Defaults to the list page.
	BackURL   string
	BackLabel string
	// Pagination state for the list pages. Limit/Offset are the
	// values that were used to query the underlying store; Page
	// is the 1-indexed page number for the rendered prev/next
	// links. HasPrev / HasNext gate the link visibility.
	Limit   int
	Offset  int
	Page    int
	HasPrev bool
	HasNext bool
	// ActiveTab is the nav tab the template should render as
	// currently-selected. Empty means no tab is active (used on
	// the dashboard).
	ActiveTab string
	// Query persists the current free-text search back into the
	// form so the user can refine it without retyping.
	Query string
}

func (h *UIHandler) render(w http.ResponseWriter, tmplName string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, tmplName, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h *UIHandler) withUserData(r *http.Request) pageData {
	return pageData{
		UserID:  auth.RequestUser(r.Context()),
		IsAdmin: auth.RequestUserRole(r.Context()) == "admin",
	}
}

func (h *UIHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		email := r.FormValue("email")
		password := r.FormValue("password")
		if email == "" || password == "" {
			h.render(w, "login.html", pageData{Error: "Email and password are required"})
			return
		}

		user, err := h.store.GetUserByEmail(r.Context(), email)
		if err != nil || !auth.CheckPassword(user.PasswordHash, password) {
			h.render(w, "login.html", pageData{Error: "Invalid email or password"})
			return
		}

		// Email-validation gate: render the login page with a
		// resend link instead of issuing a session. The
		// /ui/resend-verification form is pre-filled via the
		// ?email= query param so the user doesn't retype it.
		// Mirrors the API's 403 email_not_verified response.
		if h.emailCfg != nil && h.emailCfg.EnableValidation && !user.EmailVerified {
			h.render(w, "login.html", pageData{
				Error:     "Please verify your email before logging in.",
				ResendURL: "/ui/resend-verification?email=" + url.QueryEscape(user.Email),
			})
			return
		}

		token, err := auth.GenerateToken(h.authCfg.JWTSecret, h.authCfg.TokenTTL, user.ID, user.Email, user.Role)
		if err != nil {
			h.render(w, "login.html", pageData{Error: "Failed to generate token"})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "token",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteLaxMode,
		})
		// Land on the sources browser by default — that's the
		// first thing a new contributor wants to see. The four
		// browser pages (sources, graphs, users, tokens) are all
		// reachable from the nav.
		http.Redirect(w, r, "/ui/sources", http.StatusFound)
		return
	}

	d := h.withUserData(r)
	q := r.URL.Query()
	if q.Get("error") != "" {
		d.Error = friendlyLoginError(q.Get("error"))
	}
	// Email-validation feedback. ?verified=1 is set by
	// VerifyEmailPage after a successful click; ?verify=1 is set
	// by RegisterPage when validation is on, prompting the user
	// to check their inbox. Both render in the success slot.
	if q.Get("verified") == "1" {
		d.Success = "Your email is verified. You can log in now."
	} else if q.Get("verify") == "1" {
		d.Success = "Account created. Check your email for a verification link before logging in."
	}
	h.render(w, "login.html", d)
}

// friendlyLoginError maps machine-friendly error codes that arrive
// as ?error=... onto human-readable messages for the login form.
func friendlyLoginError(code string) string {
	switch code {
	case "session_expired":
		return "Your session has expired. Please log in again."
	default:
		return code
	}
}

func (h *UIHandler) RegisterPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		email := r.FormValue("email")
		password := r.FormValue("password")
		displayName := r.FormValue("display_name")
		if email == "" || password == "" {
			h.render(w, "register.html", pageData{Error: "Email and password are required"})
			return
		}

		hash, err := auth.HashPassword(password)
		if err != nil {
			h.render(w, "register.html", pageData{Error: "Failed to process password"})
			return
		}

		user := &model.User{
			ID:           uuid.New().String(),
			Email:        email,
			PasswordHash: hash,
			DisplayName:  displayName,
			Role:         "viewer",
		}
		for _, admin := range h.authCfg.BootstrapAdmins {
			if admin == email {
				user.Role = "admin"
				break
			}
		}
		user.CreatedAt = time.Now()
		user.UpdatedAt = user.CreatedAt

		if err := h.store.CreateUser(r.Context(), user); err != nil {
			h.render(w, "register.html", pageData{Error: "Email already registered"})
			return
		}

		// Email-validation path: mint + email the verification
		// token, then land on the login page with a "check your
		// email" success message instead of auto-logging-in. The
		// API path returns a JSON status; the UI path redirects
		// to /ui/login so the user sees the standard login form
		// with the verification prompt.
		if h.emailCfg != nil && h.emailCfg.EnableValidation {
			if err := issueEmailVerification(r.Context(), h.store, h.mailer, h.emailCfg, user); err != nil {
				log.Printf("register(ui): issueVerification for %s: %v", user.Email, err)
				h.render(w, "register.html", pageData{Error: "Account created but failed to send verification email. Use the resend link on the login page."})
				return
			}
			http.Redirect(w, r, "/ui/login?registered=1&verify=1", http.StatusFound)
			return
		}

		http.Redirect(w, r, "/ui/login?registered=1", http.StatusFound)
		return
	}

	d := h.withUserData(r)
	h.render(w, "register.html", d)
}

// VerifyEmailPage handles GET /ui/verify-email?token=<raw>. It's the
// landing page the verification email links to. On success it
// redirects to /ui/login?verified=1; on a bad/expired token it
// renders the login page with an error so the user can request a
// resend. The token-validation logic is shared with the API path
// via AuthHandler.verifyEmail; this UI variant passes
// redirectOnSuccess=true so the user lands on the login form
// instead of getting a JSON body.
func (h *UIHandler) VerifyEmailPage(w http.ResponseWriter, r *http.Request) {
	if h.emailCfg == nil || !h.emailCfg.EnableValidation {
		http.NotFound(w, r)
		return
	}
	tokenStr := strings.TrimSpace(r.URL.Query().Get("token"))
	if tokenStr == "" {
		h.render(w, "login.html", pageData{Error: "Missing verification token."})
		return
	}
	// Reuse the shared verify core. We build a temporary
	// AuthHandler to access verifyEmail; it only needs the store,
	// emailCfg, and jwtSecret (the latter unused on the redirect
	// path). This avoids duplicating the verify logic across the
	// two handler types.
	ah := &AuthHandler{store: h.store, authCfg: h.authCfg, emailCfg: h.emailCfg, mailer: h.mailer, jwtSecret: h.authCfg.JWTSecret}
	ah.verifyEmail(w, r, tokenStr, true)
}

// ResendVerificationPage handles GET (show form) and POST (trigger
// resend) for /ui/resend-verification. GET pre-fills the email
// field from ?email= (set by the login page's resend link). POST
// always renders the form with a success message ("if that email
// is registered and unverified, a message has been sent") to match
// the API's enumeration-safe 200.
func (h *UIHandler) ResendVerificationPage(w http.ResponseWriter, r *http.Request) {
	if h.emailCfg == nil || !h.emailCfg.EnableValidation {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		email := strings.TrimSpace(r.FormValue("email"))
		if email == "" {
			h.render(w, "resend_verification.html", pageData{Error: "Email is required.", PrefillEmail: email})
			return
		}
		// Reuse the API handler's resend logic so the
		// enumeration-safe behavior is identical. We build a
		// temporary AuthHandler the same way VerifyEmailPage does.
		ah := &AuthHandler{store: h.store, authCfg: h.authCfg, emailCfg: h.emailCfg, mailer: h.mailer, jwtSecret: h.authCfg.JWTSecret}
		rec := newBufferedResponseWriter(w)
		ah.ResendVerification(rec, r)
		if rec.status == http.StatusOK {
			h.render(w, "resend_verification.html", pageData{
				Success:      "If that email is registered and unverified, a verification link has been sent.",
				PrefillEmail: email,
			})
			return
		}
		h.render(w, "resend_verification.html", pageData{Error: "Failed to send verification email. Please try again.", PrefillEmail: email})
		return
	}
	// GET: render the form with the email pre-filled from ?email=
	// (the login page's resend link sets it).
	h.render(w, "resend_verification.html", pageData{PrefillEmail: r.URL.Query().Get("email")})
}

func (h *UIHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	// /ui/dashboard is kept as a legacy alias for /ui/tokens
	// (the tokens-page surface is the same — same data, same
	// template). The new layout puts "Tokens" in the nav;
	// /ui/dashboard still resolves for any user who bookmarked
	// the old URL.
	http.Redirect(w, r, "/ui/tokens", http.StatusFound)
}

// UIAuthGuard wraps authMW.AuthRequired so that browser sessions on
// /ui/* get a friendly redirect to the login page on auth failure
// instead of the raw JSON 401 body. The JSON 401 path is preserved
// for API clients on /api/v1/* by mounting this guard only in the
// /ui group.
//
// Implementation: buffer the inner middleware's response so that a
// 401 (with body already written) can be discarded and replaced with
// a 302 redirect. The buffer also captures the status code so the
// guard can branch on it. Anything other than 401 is replayed to the
// real ResponseWriter unchanged.
func UIAuthGuard(mw *auth.Middleware) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := newBufferedResponseWriter(w)
			mw.AuthRequired(next).ServeHTTP(rec, r)
			if !rec.headerWritten() {
				rec.WriteHeader(http.StatusOK)
			}
			if rec.status == http.StatusUnauthorized {
				// Discard the JSON 401 and send a redirect
				// instead. The friendlyLoginError helper in
				// LoginPage maps the error code to a human
				// message.
				http.Redirect(w, r, "/ui/login?error=session_expired", http.StatusFound)
				return
			}
			rec.flushTo(w)
		})
	}
}

// bufferedResponseWriter captures a downstream ResponseWriter's
// status, headers, and body in-memory so the caller can inspect or
// discard them. Used by UIAuthGuard to substitute a friendlier
// response on 401 without leaking the original JSON body.
type bufferedResponseWriter struct {
	w       http.ResponseWriter
	hdr     http.Header
	body    []byte
	status  int
	written bool
	hdrSent bool
}

func newBufferedResponseWriter(w http.ResponseWriter) *bufferedResponseWriter {
	return &bufferedResponseWriter{
		w:      w,
		hdr:    make(http.Header),
		status: http.StatusOK,
	}
}

func (b *bufferedResponseWriter) Header() http.Header { return b.hdr }

func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	if !b.hdrSent {
		b.hdrSent = true
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

func (b *bufferedResponseWriter) WriteHeader(code int) {
	if b.hdrSent {
		return
	}
	b.status = code
	b.hdrSent = true
	b.written = true
}

func (b *bufferedResponseWriter) headerWritten() bool { return b.hdrSent }

func (b *bufferedResponseWriter) flushTo(w http.ResponseWriter) {
	dst := w.Header()
	for k, vs := range b.hdr {
		dst[k] = vs
	}
	if !b.written {
		w.WriteHeader(b.status)
	} else {
		w.WriteHeader(b.status)
	}
	if len(b.body) > 0 {
		_, _ = w.Write(b.body)
	}
}

func (h *UIHandler) AdminPage(w http.ResponseWriter, r *http.Request) {
	// /ui/admin is kept as a legacy URL; the user-management
	// surface now lives at /ui/users. Redirect so existing
	// bookmarks keep working.
	http.Redirect(w, r, "/ui/users", http.StatusFound)
}

// SourcesPage renders /ui/sources: a browser for the sources
// stored in the registry. Supports a free-text `q` filter (passed
// to SearchSourcesText) and ?limit/?offset pagination. POSTs from
// the admin delete button are handled here too (the form posts to
// /ui/sources/{id}/delete).
func (h *UIHandler) SourcesPage(w http.ResponseWriter, r *http.Request) {
	d := h.withUserData(r)
	d.ActiveTab = "sources"
	d.Query = strings.TrimSpace(r.URL.Query().Get("q"))

	limit, offset := parsePagination(r)
	d.Limit = limit
	d.Offset = offset

	// Admin delete: POST /ui/sources/{id}/delete.
	if r.Method == http.MethodPost {
		if !d.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/ui/sources/")
		id = strings.TrimSuffix(id, "/delete")
		if id == "" || id == r.URL.Path {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := h.svc.DeleteSource(r.Context(), id); err != nil {
			d.Error = "Failed to delete source: " + err.Error()
			h.render(w, "sources.html", h.sourcesData(r, d))
			return
		}
		http.Redirect(w, r, buildListRedirect("/ui/sources", d.Query, d.Limit, d.Offset), http.StatusFound)
		return
	}

	h.render(w, "sources.html", h.sourcesData(r, d))
}

func (h *UIHandler) sourcesData(r *http.Request, d pageData) pageData {
	limit, offset := d.Limit, d.Offset
	var (
		sources []model.SourceMeta
		total   int
		err     error
	)
	if d.Query != "" {
		sources, total, err = h.svc.SearchSourcesText(r.Context(), d.Query, limit, offset)
	} else {
		sources, total, err = h.svc.ListSources(r.Context(), limit, offset)
	}
	if err != nil {
		d.Error = "Failed to load sources"
	} else {
		d.Sources = sources
		d.SourcesTotal = total
		d.Page, d.HasPrev, d.HasNext = paginationState(limit, offset, total)
	}
	return d
}

// SourceDetailPage renders /ui/sources/{id}: the metadata + S3
// info for a single source, plus the list of decompositions
// (which is the most useful thing to inspect when maintaining
// the registry — "is this source actually being used?"). Admin
// can delete the source from this page; non-admins see the
// metadata only.
//
// Errors:
//   - source not found       → 404 page (not_found.html)
//   - delete by non-admin    → 403
//   - other failures         → render the page with .Error set
func (h *UIHandler) SourceDetailPage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/ui/sources/")
	id = strings.TrimSuffix(id, "/delete")
	if id == "" || id == r.URL.Path {
		http.NotFound(w, r)
		return
	}

	d := h.withUserData(r)
	d.ActiveTab = "sources"
	// Pin the list-page defaults so the back link doesn't echo
	// a stray "limit=0" — the detail page doesn't paginate,
	// but the link needs to land on a valid list URL.
	limit, offset := parsePagination(r)
	d.Limit = limit
	d.Offset = offset
	d.BackURL = buildListRedirect("/ui/sources", d.Query, d.Limit, d.Offset)
	d.BackLabel = "Back to Sources"

	// Admin delete: POST /ui/sources/{id}/delete (from this
	// page's delete button). After delete, redirect back to the
	// list — the user explicitly chose to remove the source.
	if r.Method == http.MethodPost {
		if !d.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := h.svc.DeleteSource(r.Context(), id); err != nil {
			d.Error = "Failed to delete source: " + err.Error()
			h.renderSourceDetail(w, r, d, id)
			return
		}
		http.Redirect(w, r, "/ui/sources", http.StatusFound)
		return
	}

	h.renderSourceDetail(w, r, d, id)
}

func (h *UIHandler) renderSourceDetail(w http.ResponseWriter, r *http.Request, d pageData, id string) {
	src, err := h.svc.GetSourceMeta(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			d.Error = "Source not found"
			h.render(w, "not_found.html", d)
			return
		}
		d.Error = "Failed to load source: " + err.Error()
		h.render(w, "source_detail.html", d)
		return
	}
	d.Source = src

	decomps, err := h.svc.ListDecompositions(r.Context(), id)
	if err != nil {
		// Non-fatal: the metadata is the primary surface, the
		// decompositions list is secondary. Log via the page
		// error and render with an empty list.
		d.Error = "Failed to load decompositions: " + err.Error()
		d.Decomps = []model.DecompMeta{}
	} else {
		d.Decomps = decomps
	}

	// Best-effort presigned download URL for the source bundle.
	// If presigning is disabled (e.g. local dev with no S3
	// public URL), the link is omitted from the template.
	if presigned, perr := h.svc.PresignedDownloadURL(r.Context(), id, "source", ""); perr == nil {
		d.PresignedSrc = presigned
	}

	h.render(w, "source_detail.html", d)
}

// GraphsPage renders /ui/graphs: a browser for the shared
// knowledge graphs stored in the registry. Supports the same
// `q` and `tag` filters as GET /api/v1/graphs plus pagination.
// POSTs from the admin delete button are handled here too.
func (h *UIHandler) GraphsPage(w http.ResponseWriter, r *http.Request) {
	d := h.withUserData(r)
	d.ActiveTab = "graphs"
	d.Query = strings.TrimSpace(r.URL.Query().Get("q"))

	limit, offset := parsePagination(r)
	d.Limit = limit
	d.Offset = offset

	// Admin delete: POST /ui/graphs/{id}/delete.
	if r.Method == http.MethodPost {
		if !d.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/ui/graphs/")
		id = strings.TrimSuffix(id, "/delete")
		if id == "" || id == r.URL.Path {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := h.svc.DeleteGraph(r.Context(), id); err != nil {
			d.Error = "Failed to delete graph: " + err.Error()
			h.render(w, "graphs.html", h.graphsData(r, d))
			return
		}
		http.Redirect(w, r, buildListRedirect("/ui/graphs", d.Query, d.Limit, d.Offset), http.StatusFound)
		return
	}

	h.render(w, "graphs.html", h.graphsData(r, d))
}

func (h *UIHandler) graphsData(r *http.Request, d pageData) pageData {
	limit, offset := d.Limit, d.Offset
	result, err := h.svc.ListGraphs(r.Context(), model.GraphSearchQuery{
		Query:  d.Query,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		d.Error = "Failed to load graphs"
	} else {
		d.Graphs = result.Graphs
		d.GraphsTotal = result.Total
		d.Page, d.HasPrev, d.HasNext = paginationState(limit, offset, result.Total)
	}
	return d
}

// GraphDetailPage renders /ui/graphs/{id}: the metadata for a
// single graph + a presigned download link to the bundle. Admin
// can delete the graph from this page.
func (h *UIHandler) GraphDetailPage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/ui/graphs/")
	id = strings.TrimSuffix(id, "/delete")
	if id == "" || id == r.URL.Path {
		http.NotFound(w, r)
		return
	}

	d := h.withUserData(r)
	d.ActiveTab = "graphs"
	limit, offset := parsePagination(r)
	d.Limit = limit
	d.Offset = offset
	d.BackURL = buildListRedirect("/ui/graphs", d.Query, d.Limit, d.Offset)
	d.BackLabel = "Back to Graphs"

	if r.Method == http.MethodPost {
		if !d.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := h.svc.DeleteGraph(r.Context(), id); err != nil {
			d.Error = "Failed to delete graph: " + err.Error()
			h.renderGraphDetail(w, r, d, id)
			return
		}
		http.Redirect(w, r, "/ui/graphs", http.StatusFound)
		return
	}

	h.renderGraphDetail(w, r, d, id)
}

func (h *UIHandler) renderGraphDetail(w http.ResponseWriter, r *http.Request, d pageData, id string) {
	graph, err := h.svc.PullGraph(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			d.Error = "Graph not found"
			h.render(w, "not_found.html", d)
			return
		}
		d.Error = "Failed to load graph: " + err.Error()
		h.render(w, "graph_detail.html", d)
		return
	}
	d.Graph = graph
	h.render(w, "graph_detail.html", d)
}

// UsersPage renders /ui/users: a list of all users with their
// roles. The role-update form is only shown to admins; non-admins
// see the list read-only.
func (h *UIHandler) UsersPage(w http.ResponseWriter, r *http.Request) {
	d := h.withUserData(r)
	d.ActiveTab = "users"

	// Non-admins can still see the directory; only admins can
	// change roles. The page enforces this both by hiding the
	// form and by returning 403 on the POST handler.
	if r.Method == http.MethodPost {
		if !d.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		userID := strings.TrimPrefix(r.URL.Path, "/ui/users/")
		userID = strings.TrimSuffix(userID, "/role")
		newRole := r.FormValue("role")
		if userID != "" && newRole != "" {
			if err := h.store.UpdateUserRole(r.Context(), userID, newRole); err != nil {
				d.Error = "Failed to update role"
				users, listErr := h.store.ListUsers(r.Context())
				if listErr == nil {
					d.Users = users
				}
				h.render(w, "users.html", d)
				return
			}
		}
		http.Redirect(w, r, "/ui/users", http.StatusFound)
		return
	}

	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		d.Error = "Failed to load users"
		h.render(w, "users.html", d)
		return
	}
	d.Users = users
	h.render(w, "users.html", d)
}

// TokensPage renders /ui/tokens: the user's API tokens, with the
// create form. Identical surface to the legacy /ui/dashboard
// handler — same store, same template, just a new URL so the
// nav layout can put it under "Tokens" instead of "Dashboard".
// The legacy /ui/dashboard URL still works (handled by the
// Dashboard method).
func (h *UIHandler) TokensPage(w http.ResponseWriter, r *http.Request) {
	userID := auth.RequestUser(r.Context())
	if userID == "" {
		http.Redirect(w, r, "/ui/login?error=session_expired", http.StatusFound)
		return
	}
	d := h.withUserData(r)
	d.ActiveTab = "tokens"

	if r.Method == http.MethodPost {
		name := r.FormValue("name")
		scope := r.FormValue("scope")
		if name == "" {
			d.Error = "Name is required"
			h.render(w, "tokens.html", d)
			return
		}
		if scope == "" {
			scope = "read"
		}
		if scope != "read" && scope != "write" && scope != "readwrite" {
			d.Error = "Scope must be read, write, or readwrite"
			h.render(w, "tokens.html", d)
			return
		}

		rawToken, err := auth.GenerateAPIToken()
		if err != nil {
			d.Error = "Failed to generate token"
			h.render(w, "tokens.html", d)
			return
		}

		now := time.Now()
		var expiresAt *time.Time
		if v := r.FormValue("expires_in_days"); v != "" {
			if days, perr := strconv.Atoi(v); perr == nil && days > 0 {
				t := now.Add(time.Duration(days) * 24 * time.Hour)
				expiresAt = &t
			}
		}

		tok := &model.APIToken{
			ID:        uuid.New().String(),
			UserID:    userID,
			Name:      name,
			TokenHash: auth.HashToken(rawToken),
			Scope:     scope,
			ExpiresAt: expiresAt,
			CreatedAt: now,
		}
		if err := h.store.CreateAPIToken(r.Context(), tok); err != nil {
			d.Error = "Failed to create token"
			h.render(w, "tokens.html", d)
			return
		}

		// Re-render the page with the newly created token shown
		// in a copyable box. The token is only ever shown once —
		// the DB stores the hash, not the raw value.
		d.NewToken = rawToken
		// Refresh the token list so the new one appears.
		tokens, _ := h.store.ListAPITokens(r.Context(), userID)
		d.Tokens = tokens
		d.SourcesTotal = len(tokens)
		h.render(w, "tokens.html", d)
		return
	}

	limit, offset := parsePagination(r)
	d.Limit = limit
	d.Offset = offset

	tokens, err := h.store.ListAPITokens(r.Context(), userID)
	if err != nil {
		d.Error = "Failed to load tokens"
		h.render(w, "tokens.html", d)
		return
	}
	// Client-side pagination on the user's own tokens (the
	// store returns them all). Cheap because tokens-per-user is
	// small; the prev/next UI matches the other list pages.
	total := len(tokens)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	d.Tokens = tokens[offset:end]
	d.SourcesTotal = total // reuse the same Total field for the count
	d.Page, d.HasPrev, d.HasNext = paginationState(limit, offset, total)
	h.render(w, "tokens.html", d)
}

// parsePagination reads ?limit and ?offset query params with
// sensible defaults and bounds: limit defaults to 25 (capped at
// 200), offset defaults to 0. Anything malformed falls back to the
// default silently rather than 400'ing the page.
func parsePagination(r *http.Request) (limit, offset int) {
	limit = 25
	offset = 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		offset = v
	}
	return limit, offset
}

// paginationState returns the 1-indexed page number plus prev/next
// booleans for the rendered link set.
func paginationState(limit, offset, total int) (page int, hasPrev, hasNext bool) {
	if limit <= 0 {
		return 1, false, false
	}
	page = offset/limit + 1
	hasPrev = offset > 0
	hasNext = offset+limit < total
	return page, hasPrev, hasNext
}

// buildListRedirect composes the post-delete redirect URL so the
// user lands back on the same page they were viewing rather than
// page 1.
func buildListRedirect(basePath, query string, limit, offset int) string {
	v := url.Values{}
	if query != "" {
		v.Set("q", query)
	}
	if limit != 25 {
		v.Set("limit", strconv.Itoa(limit))
	}
	if offset != 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	if encoded := v.Encode(); encoded != "" {
		return basePath + "?" + encoded
	}
	return basePath
}

func (h *UIHandler) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "token",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
	})
	http.Redirect(w, r, "/ui/login", http.StatusFound)
}
