package handler

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type UIHandler struct {
	store   store.MetadataStore
	authCfg *config.AuthConfig
	tmpl    *template.Template
}

func NewUIHandler(store store.MetadataStore, cfg *config.AuthConfig) *UIHandler {
	sub, err := fs.Sub(templateFS, "templates")
	if err != nil {
		panic(err)
	}
	tmpl := template.Must(template.ParseFS(sub, "*.html"))
	return &UIHandler{store: store, authCfg: cfg, tmpl: tmpl}
}

type pageData struct {
	UserID   string
	IsAdmin  bool
	Error    string
	Success  string
	Token    string
	NewToken string
	Tokens   interface{}
	Users    interface{}
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
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/ui/dashboard", http.StatusFound)
		return
	}

	d := h.withUserData(r)
	if r.URL.Query().Get("error") != "" {
		d.Error = r.URL.Query().Get("error")
	}
	h.render(w, "login.html", d)
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

		http.Redirect(w, r, "/ui/login?registered=1", http.StatusFound)
		return
	}

	d := h.withUserData(r)
	h.render(w, "register.html", d)
}

func (h *UIHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	userID := auth.RequestUser(r.Context())
	if userID == "" {
		http.Redirect(w, r, "/ui/login", http.StatusFound)
		return
	}

	if r.Method == http.MethodPost {
		name := r.FormValue("name")
		scope := r.FormValue("scope")
		if name == "" || scope == "" {
			d := h.withUserData(r)
			d.Error = "Name and scope are required"
			h.render(w, "dashboard.html", d)
			return
		}

		th := NewTokenHandler(h.store, h.authCfg)
		th.Create(w, r)
		return
	}

	tokens, err := h.store.ListAPITokens(r.Context(), userID)
	if err != nil {
		d := h.withUserData(r)
		d.Error = "Failed to load tokens"
		h.render(w, "dashboard.html", d)
		return
	}

	d := h.withUserData(r)
	d.Tokens = tokens
	h.render(w, "dashboard.html", d)
}

func (h *UIHandler) AdminPage(w http.ResponseWriter, r *http.Request) {
	role := auth.RequestUserRole(r.Context())
	if role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodPost {
		path := strings.TrimPrefix(r.URL.Path, "/ui/admin/users/")
		path = strings.TrimSuffix(path, "/role")
		userID := path
		newRole := r.FormValue("role")
		if userID != "" && newRole != "" {
			if err := h.store.UpdateUserRole(r.Context(), userID, newRole); err != nil {
				d := h.withUserData(r)
				d.Error = "Failed to update role"
				h.render(w, "admin.html", d)
				return
			}
		}
		http.Redirect(w, r, "/ui/admin", http.StatusFound)
		return
	}

	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		d := h.withUserData(r)
		d.Error = "Failed to load users"
		h.render(w, "admin.html", d)
		return
	}
	d := h.withUserData(r)
	d.Users = users
	h.render(w, "admin.html", d)
}

func (h *UIHandler) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "token",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	http.Redirect(w, r, "/ui/login", http.StatusFound)
}
