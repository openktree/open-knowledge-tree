package handler

import (
	"net/http"

	"github.com/openktree/knowledge-registry/internal/service"
)

// ContextHandler exposes the registry's canonical context
// vocabulary. The list is seeded from the embedded contexts.json
// snapshot at boot and is read-only via the API (mutation is
// edit-file + restart).
type ContextHandler struct {
	svc *service.Registry
}

func NewContextHandler(svc *service.Registry) *ContextHandler {
	return &ContextHandler{svc: svc}
}

// ListContexts handles GET /api/v1/contexts.
//
// Returns the canonical context vocabulary the registry publishes so
// OKT instances can map their local contexts to the registry's set
// on contribute, and translate registry contexts back to local ones
// on pull. The response shape matches the OKT client's
// ListContextsResponse:
//
//	{"contexts": ["Politician","Organization","Place","..."]}
//
// Optional ?q= query filters by case-insensitive substring on the
// label (useful for the settings UI's dropdown).
func (h *ContextHandler) ListContexts(w http.ResponseWriter, r *http.Request) {
	classes, err := h.svc.ListContexts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list contexts")
		return
	}
	q := r.URL.Query().Get("q")
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		if q != "" {
			if !containsCI(c.Label, q) {
				continue
			}
		}
		out = append(out, c.Label)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"contexts": out,
	})
}

func containsCI(s, sub string) bool {
	if sub == "" {
		return true
	}
	ls := len(s)
	lsub := len(sub)
	if lsub > ls {
		return false
	}
	for i := 0; i+lsub <= ls; i++ {
		if equalFold(s[i:i+lsub], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}