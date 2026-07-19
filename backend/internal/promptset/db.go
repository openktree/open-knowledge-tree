package promptset

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// DBProvider is the Provider that reads user-defined promptsets from
// okt_system.promptsets via *store.Queries. It does NOT return the
// built-in promptset (the BuiltinProvider does); the Resolver chains
// the two so a hash lookup checks built-in first, then DB.
//
// The provider holds no cache: promptsets are small, lookups are
// rare (one per Work() start, plus per HTTP CRUD call), and the
// repository-scoped resolver already memoizes the effective hash.
// A cache can be added later if profiling shows DB pressure.
type DBProvider struct {
	queries *store.Queries
}

// NewDBProvider constructs a DBProvider. queries is the system-pool
// *store.Queries (the same one handler.Deps.Store holds); nil is
// safe — Get returns ok=false and List returns nil, so a deployment
// that hasn't wired the system pool still gets the built-in
// promptset via the Resolver.
func NewDBProvider(queries *store.Queries) *DBProvider {
	return &DBProvider{queries: queries}
}

// Get returns the user-defined Promptset for the given hash, or
// ok=false when not found (or when queries is nil, or when the hash
// is the empty string / built-in hash — those are BuiltinProvider's
// responsibility).
func (p *DBProvider) Get(hash string) (Promptset, bool) {
	if p.queries == nil || hash == "" || hash == DefaultHash {
		return Promptset{}, false
	}
	ctx := context.Background()
	row, err := p.queries.GetPromptsetByHash(ctx, hash)
	if err != nil {
		return Promptset{}, false
	}
	return rowToPromptset(row), true
}

// List returns every user-defined promptset in the catalog. The
// caller (the HTTP handler) filters by owner for the per-user view;
// this raw List is the sysadmin / "all" view. Returns nil when
// queries is nil.
func (p *DBProvider) List() []Promptset {
	if p.queries == nil {
		return nil
	}
	ctx := context.Background()
	rows, err := p.queries.ListAllPromptsets(ctx)
	if err != nil {
		return nil
	}
	out := make([]Promptset, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToPromptset(r))
	}
	return out
}

// ListByOwner returns every promptset owned by the given user. Used
// by the per-user GET /api/v1/promptsets endpoint. Returns nil when
// queries is nil.
func (p *DBProvider) ListByOwner(ownerID pgtype.UUID) []Promptset {
	if p.queries == nil {
		return nil
	}
	ctx := context.Background()
	rows, err := p.queries.ListPromptsetsByOwner(ctx, ownerID)
	if err != nil {
		return nil
	}
	out := make([]Promptset, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToPromptset(r))
	}
	return out
}

// rowToPromptset converts a sqlc-generated OktSystemPromptset row
// into a Promptset. OwnerID is normalized to the empty string when
// NULL (the migration allows owner_id NULL for a future "global"
// promptset; today every custom promptset has an owner). Source is
// set to CustomSource so the UI can badge it. The Hash is read from
// the row (the PK); the RegistryHash is computed from the 4 shared
// phase strings so the UI and the pull filter can compare
// compatibility without an extra stored column (the value is
// deterministic — see RegistryHashPromptset).
func rowToPromptset(r store.OktSystemPromptset) Promptset {
	owner := ""
	if r.OwnerID.Valid {
		owner = r.OwnerID.String()
	}
	ps := Promptset{
		Hash:                r.Hash,
		Name:                r.Name,
		OwnerID:             owner,
		Source:              CustomSource,
		FactExtraction:      r.FactExtraction,
		ImageFactExtraction: r.ImageFactExtraction,
		ConceptExtraction:   r.ConceptExtraction,
		Refinement:          r.Refinement,
		Synthesis:           r.Synthesis,
		ImagePicker:         r.ImagePicker,
		Summarization:       r.Summarization,
		Posture:             r.Posture,
	}
	ps.RegistryHash = RegistryHashPromptset(ps)
	return ps
}