package promptset

import "github.com/jackc/pgx/v5/pgtype"

// Resolver chains promptset Providers (built-in first, then DB) and
// exposes a unified Get / List over the union. Built in
// cmd/app/api.go alongside the search/resolution strategies; threaded
// into the task workers (so each job resolves the active promptset
// for its repo) and into the HTTP handlers (so the promptset CRUD
// endpoints can validate hashes against the live catalog).
//
// The empty hash is the "inherit global default" sentinel: Get("")
// returns the built-in Default, so callers that resolve a NULL
// per-repo active_promptset_hash can pass the empty string straight
// through without a special case.
type Resolver struct {
	builtin BuiltinProvider
	db      *DBProvider
}

// NewResolver constructs a Resolver. db may be nil — the Resolver
// then serves only the built-in promptset, which is the correct
// behavior for a deployment that hasn't wired the system pool (e.g.
// a test). The built-in provider is always present.
func NewResolver(db *DBProvider) *Resolver {
	return &Resolver{
		builtin: NewBuiltinProvider(),
		db:      db,
	}
}

// Get returns the Promptset for the given hash, consulting the
// built-in provider first and the DB provider second. An empty hash
// resolves to the built-in Default (the "inherit global default"
// sentinel). Returns (Promptset{}, false) when no provider knows the
// hash — the caller should treat this as "use the global default".
func (r *Resolver) Get(hash string) (Promptset, bool) {
	if ps, ok := r.builtin.Get(hash); ok {
		return ps, true
	}
	if r.db != nil {
		if ps, ok := r.db.Get(hash); ok {
			return ps, true
		}
	}
	return Promptset{}, false
}

// ResolveOrDefault returns the Promptset for the given hash, or the
// built-in Default when the hash is unknown / empty. This is the
// safe helper workers use: a repo pointing at a deleted custom
// promptset falls back to the built-in rather than failing the job.
func (r *Resolver) ResolveOrDefault(hash string) Promptset {
	if ps, ok := r.Get(hash); ok {
		return ps
	}
	return Default
}

// List returns the union of every promptset the chained providers
// know: the built-in first, then every user-defined promptset in the
// DB catalog. The order is stable so the UI can render the built-in
// at the top. Duplicates are impossible (a custom promptset with the
// same 8 phase strings as the built-in has the same hash and is the
// same row in the DB upsert, so it appears once — as the built-in).
func (r *Resolver) List() []Promptset {
	out := r.builtin.List()
	if r.db != nil {
		out = append(out, r.db.List()...)
	}
	return out
}

// ListForOwner returns the built-in promptset plus every custom
// promptset owned by the given user. Used by the per-user
// GET /api/v1/promptsets endpoint so a user sees the built-in
// (always available) and their own custom promptsets. The built-in
// is first so the UI can badge it as non-editable.
func (r *Resolver) ListForOwner(ownerID string) []Promptset {
	out := r.builtin.List()
	if r.db != nil {
		var oid pgtype.UUID
		if ownerID != "" {
			_ = oid.Scan(ownerID)
		}
		out = append(out, r.db.ListByOwner(oid)...)
	}
	return out
}

// Has reports whether the resolver knows the given hash. Used by the
// repository settings handler to validate a repo's
// active_promptset_hash / accepted_promptset_hashes before accepting
// them. The empty hash returns true (it means "inherit default").
func (r *Resolver) Has(hash string) bool {
	_, ok := r.Get(hash)
	return ok
}