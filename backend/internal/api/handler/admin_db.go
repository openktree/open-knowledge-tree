package handler

import (
	"net/http"
	"sort"

	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// AdminDB bundles the database-administration HTTP handlers. The
// handlers here are sys-admin-only; the router wraps them in the
// existing admin auth flow (see internal/api/wiring.go).
type AdminDB struct {
	deps Deps
}

// NewAdminDB constructs an AdminDB handler bundle.
func NewAdminDB(d Deps) *AdminDB {
	return &AdminDB{deps: d}
}

// databaseEntry is one element of the `databases` array in the
// response from ListDatabases. The shape is the union of what
// the picker UI needs (Name, Tier, MaxConns) and what the admin
// health UI needs (the same fields, with the picker metadata
// stripped).
type databaseEntry struct {
	Name             string `json:"name"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	MaxConns         int    `json:"max_conns"`
	IsDefault        bool   `json:"is_default"`
	IsPickerAllowed  bool   `json:"is_picker_allowed"`
	// Tier is the value the server would store in
	// repositories.tier for a new repository created in this
	// database. It is "shared" for the default database and
	// "isolated" for everything else (a future "sovereign" tier
	// can extend this). Computed at request time from
	// cfg.Isolation, never stored on the database itself.
	Tier string `json:"tier"`
}

// databasesResponse is the wire shape of GET /admin/databases.
// It supersedes the older /admin/repository-databases endpoint
// (which only listed the picker allow-list); the picker UI and
// the health UI can both consume this single response.
type databasesResponse struct {
	DefaultDatabase  string          `json:"default_database"`
	Databases        []databaseEntry `json:"databases"`
	PickerAllowedFor []string        `json:"picker_allowed_for"`
}

// ListDatabases handles GET /admin/databases. It returns every
// database the server has open (cfg.Databases), annotated with
// whether each is the default, whether the picker is allowed to
// route new repositories to it, and the tier the server would
// record for a new repository created there.
//
// The endpoint is read-only and does not require any role beyond
// `repository:read` (the same gate the previous picker endpoint
// used) so the picker UI renders for every user. The server still
// enforces the picker policy on POST (CreateRepository) — a
// non-permitted caller gets a silent override to the default,
// not an error.
func (a *AdminDB) ListDatabases(w http.ResponseWriter, r *http.Request) {
	cfg := a.deps.Config

	// Stable ordering: sort the database names so the UI
	// doesn't have to deal with map iteration order. The default
	// is rendered first regardless of the alphabetical order
	// because the picker shows it prominently.
	names := make([]string, 0, len(cfg.Databases))
	for name := range cfg.Databases {
		names = append(names, name)
	}
	sort.Strings(names)

	// Build the picker-allow-list membership as a set for O(1)
	// lookups. The default is implicitly allowed; we add it to
	// the set so the per-row `is_picker_allowed` field reflects
	// the effective UI state.
	allowed := make(map[string]bool, len(cfg.Isolation.AllowedDatabases)+1)
	allowed[cfg.Isolation.DefaultDatabase] = true
	for _, n := range cfg.Isolation.AllowedDatabases {
		allowed[n] = true
	}

	entries := make([]databaseEntry, 0, len(cfg.Databases))
	for _, name := range names {
		db := cfg.Databases[name]
		entries = append(entries, databaseEntry{
			Name:            name,
			Host:            db.Host,
			Port:            db.Port,
			MaxConns:        db.MaxConns,
			IsDefault:       name == cfg.Isolation.DefaultDatabase,
			IsPickerAllowed: allowed[name],
			Tier:            config.TierFor(cfg.Isolation, name),
		})
	}

	// picker_allowed_for is the list the picker UI binds to its
	// <select>. We return it as a separate field (rather than
	// having the UI filter the `databases` array) so the picker
	// and the health view can coexist on the same response.
	pickerList := make([]string, 0, len(allowed))
	for name := range allowed {
		pickerList = append(pickerList, name)
	}
	sort.Strings(pickerList)

	httputil.WriteJSON(w, http.StatusOK, databasesResponse{
		DefaultDatabase:  cfg.Isolation.DefaultDatabase,
		Databases:        entries,
		PickerAllowedFor: pickerList,
	})
}
