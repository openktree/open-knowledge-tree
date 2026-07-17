package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/openktree/knowledge-registry/internal/api/handler"
	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
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

	authMW := auth.NewMiddleware(&cfg.Auth)
	authH := handler.NewAuthHandler(mstore, &cfg.Auth)
	tokenH := handler.NewTokenHandler(mstore, &cfg.Auth)
	adminH := handler.NewAdminHandler(mstore)
	uiH := handler.NewUIHandler(mstore, &cfg.Auth)

	// Exempted from auth
	r.Get("/health", healthH.Health)

	// UI routes
	r.Route("/ui", func(r chi.Router) {
		r.Get("/login", uiH.LoginPage)
		r.Post("/login", uiH.LoginPage)
		r.Get("/register", uiH.RegisterPage)
		r.Post("/register", uiH.RegisterPage)

		// Authenticated UI
		r.Group(func(r chi.Router) {
			r.Use(authMW.AuthRequired)
			r.Get("/dashboard", uiH.Dashboard)
			r.Post("/dashboard", uiH.Dashboard)
			r.Post("/tokens/{id}/revoke", tokenH.Revoke)
			r.Get("/logout", uiH.Logout)
		})

		// Admin UI
		r.Group(func(r chi.Router) {
			r.Use(authMW.AuthRequired)
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
