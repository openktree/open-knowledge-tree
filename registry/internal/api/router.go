package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/openktree/knowledge-registry/internal/api/handler"
	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/mailer"
	"github.com/openktree/knowledge-registry/internal/service"
	"github.com/openktree/knowledge-registry/internal/store"
)

func NewRouter(svc *service.Registry, mstore store.MetadataStore, cfg *config.Config) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(noRobots)

	srcH := handler.NewSourceHandler(svc)
	healthH := handler.NewHealthHandler(svc)
	ctxH := handler.NewContextHandler(svc)
	graphH := handler.NewGraphHandler(svc)

	authMW := auth.NewMiddleware(&cfg.Auth)
	authMW.SetStore(mstore)
	m := mailer.NewFromConfig(cfg.EmailValidation)
	authH := handler.NewAuthHandler(mstore, &cfg.Auth, &cfg.EmailValidation, m)
	tokenH := handler.NewTokenHandler(mstore, &cfg.Auth)
	adminH := handler.NewAdminHandler(mstore)
	uiH := handler.NewUIHandler(mstore, svc, &cfg.Auth, &cfg.EmailValidation, m)

	// Exempted from auth
	r.Get("/health", healthH.Health)

	// UI routes
	r.Route("/ui", func(r chi.Router) {
		r.Get("/login", uiH.LoginPage)
		r.Post("/login", uiH.LoginPage)
		r.Get("/register", uiH.RegisterPage)
		r.Post("/register", uiH.RegisterPage)
		// Email-validation UI (only functional when
		// email_validation.enable_validation is on; the handlers
		// return 404 otherwise, so registering the routes
		// unconditionally is safe).
		r.Get("/verify-email", uiH.VerifyEmailPage)
		r.Get("/resend-verification", uiH.ResendVerificationPage)
		r.Post("/resend-verification", uiH.ResendVerificationPage)

		// Authenticated UI
		uiAuth := handler.UIAuthGuard(authMW)
		r.Group(func(r chi.Router) {
			r.Use(uiAuth)
			r.Get("/dashboard", uiH.Dashboard)
			r.Post("/dashboard", uiH.Dashboard)
			r.Get("/sources", uiH.SourcesPage)
			r.Get("/sources/{id}", uiH.SourceDetailPage)
			r.Post("/sources/{id}/delete", uiH.SourceDetailPage)
			r.Get("/graphs", uiH.GraphsPage)
			r.Get("/graphs/{id}", uiH.GraphDetailPage)
			r.Post("/graphs/{id}/delete", uiH.GraphDetailPage)
			r.Get("/users", uiH.UsersPage)
			r.Post("/users/{id}/role", uiH.UsersPage)
			r.Get("/tokens", uiH.TokensPage)
			r.Post("/tokens", uiH.TokensPage)
			r.Post("/tokens/{id}/revoke", tokenH.Revoke)
			r.Get("/logout", uiH.Logout)
		})

		// Admin UI — legacy URL kept for backward compat; the
		// handler redirects to /ui/users, which is the new
		// user-management surface.
		r.Group(func(r chi.Router) {
			r.Use(uiAuth)
			r.Use(authMW.RequireRole("admin"))
			r.Get("/admin", uiH.AdminPage)
			r.Post("/admin", uiH.AdminPage)
			r.Post("/admin/users/{id}/role", uiH.AdminPage)
		})
	})

	// API v1
	r.Route("/api/v1", func(r chi.Router) {
		// Auth endpoints (always open)
		r.Post("/auth/register", authH.Register)
		r.Post("/auth/login", authH.Login)
		// Email-validation endpoints. The handlers return 404
		// when email_validation.enable_validation is off, so
		// registering them unconditionally is safe and keeps the
		// route table stable across toggles.
		r.Get("/auth/verify-email", authH.VerifyEmail)
		r.Post("/auth/resend-verification", authH.ResendVerification)

		// Token management (authenticated)
		r.Group(func(r chi.Router) {
			r.Use(authMW.AuthRequired)
			r.Get("/tokens", tokenH.List)
			r.Post("/tokens", tokenH.Create)
			r.Delete("/tokens/{id}", tokenH.Revoke)
		})

		// Admin (admin only)
		r.Group(func(r chi.Router) {
			r.Use(authMW.AuthRequired)
			r.Use(authMW.RequireRole("admin"))
			r.Get("/admin/users", adminH.ListUsers)
			r.Put("/admin/users/{id}/role", adminH.UpdateRole)
			r.Post("/admin/cleanup-uploads", graphH.CleanupUploads)
		})

		// Source endpoints (auth mode gating)
		r.Group(func(r chi.Router) {
			r.Use(authMW.OptionalAuth)

			r.Post("/sources", srcH.Push)
			r.Get("/sources", srcH.ListSources)
			r.Get("/search", srcH.Search)
			r.Get("/contexts", ctxH.ListContexts)

			r.Route("/sources/{sid}", func(r chi.Router) {
				r.Get("/", srcH.PullSource)
				r.Get("/presigned", srcH.PresignedDownloadURL)
				r.Post("/presigned", srcH.PresignedUploadURL)
				r.Post("/decompositions/{model}", srcH.PushDecomposition)
				r.Get("/decompositions", srcH.ListDecompositions)
				r.Get("/decompositions/{model}", srcH.PullDecomposition)
			})

			// Shared knowledge graphs. Browse/list are open under the
			// same auth-mode gating as sources; push/delete follow the
			// auth_mode ("read-open" / "closed" require a JWT for writes).
			// The owner field on a pushed graph is populated from the
			// authenticated user's email.
			r.Route("/graphs", func(r chi.Router) {
				r.Get("/", graphH.List)
				r.Post("/", graphH.Push)
				r.Route("/{id}", func(r chi.Router) {
					r.Get("/", graphH.Get)
					r.Get("/bundle", graphH.PullBundle)
					r.Delete("/", graphH.Delete)
				})
			})
		})
	})

	return r
}

func noRobots(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}
