package tasks

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// PromptsetResolver resolves a repository's effective promptset —
// the philosophy its decompositions run under — by consulting the
// per-repo active_promptset_hash column, then the global config
// default (cfg.Providers.PromptsetDefault), then the built-in
// promptset.Default. It mirrors ModelResolver's shape (system-pool
// Queries + config + a resolver dependency) so workers can call it
// once at Work() start and thread the resolved promptset into every
// phase provider.
//
// The resolver exposes two hash surfaces:
//
//   - EffectiveHash / Effective: the FULL catalog hash (8 phases).
//     Used to tag LOCAL DB rows (facts/concepts/links.promptset_hash)
//     so local queries can group by full philosophy.
//
//   - EffectiveRegistryHash / AcceptedRegistryHashes: the
//     REGISTRY-compatibility hash (4 shared phases only). Used to
//     tag decompositions pushed to the registry (contribute_source)
//     and to filter decompositions on pull (the RelevanceFilter).
//     Two promptsets that differ only in synthesis/summarization/
//     posture/image_picker share a RegistryHash and can exchange
//     decompositions.
//
// AcceptedRegistryHashes is the set of registry hashes a repo will
// admit on pull: the active hash plus every entry in
// accepted_promptset_hashes, each mapped from the stored full hash
// to its compatibility hash via the resolver. A remote decomposition
// whose registry hash is not in this set (and not in
// promptset.DefaultRegistryHashes) is skipped, which is what keeps
// decompositions from incompatible promptsets from mixing at the
// registry level.
type PromptsetResolver struct {
	cfg          *config.Config
	systemQueries *store.Queries
	resolver     *promptset.Resolver
}

// NewPromptsetResolver constructs a resolver. systemQueries is the
// default-pool *store.Queries (the same one ModelResolver holds);
// resolver is the promptset.Resolver built in cmd/app/api.go. Nil
// systemQueries / resolver is safe — Effective returns the built-in
// Default, EffectiveHash returns DefaultHash, EffectiveRegistryHash
// returns DefaultRegistryHash, and AcceptedRegistryHashes returns
// just the effective registry hash, so a deployment that hasn't
// wired the system pool still runs (every repo inherits the
// built-in philosophy).
func NewPromptsetResolver(cfg *config.Config, systemQueries *store.Queries, resolver *promptset.Resolver) *PromptsetResolver {
	return &PromptsetResolver{cfg: cfg, systemQueries: systemQueries, resolver: resolver}
}

// Effective returns the Promptset the given repo should run under.
// Resolution order: repo.active_promptset_hash →
// cfg.Providers.PromptsetDefault → promptset.Default. A NULL or
// unknown hash falls back to the global default, then to the
// built-in, so a repo pointing at a deleted custom promptset never
// fails a job — it silently uses the built-in philosophy (which is
// the same behavior as a fresh repo with no override).
func (r *PromptsetResolver) Effective(ctx context.Context, repoID pgtype.UUID) promptset.Promptset {
	hash := r.EffectiveHash(ctx, repoID)
	if r.resolver != nil {
		return r.resolver.ResolveOrDefault(hash)
	}
	return promptset.Default
}

// EffectiveHash returns the effective FULL catalog hash for a repo
// as a non-empty string (the built-in hash when no override is set).
// Used by workers to tag LOCAL fact/concept inserts with
// promptset_hash without needing the full Promptset struct. The
// local DB stores full hashes so per-philosophy queries (synthesis,
// posture grouping) keep their full granularity.
func (r *PromptsetResolver) EffectiveHash(ctx context.Context, repoID pgtype.UUID) string {
	if r.systemQueries != nil {
		row, err := r.systemQueries.GetRepositoryPromptset(ctx, repoID)
		if err == nil && row.ActivePromptsetHash != nil && *row.ActivePromptsetHash != "" {
			return *row.ActivePromptsetHash
		}
	}
	if r.cfg != nil && r.cfg.Providers.PromptsetDefault != "" {
		return r.cfg.Providers.PromptsetDefault
	}
	return promptset.DefaultHash
}

// EffectiveRegistryHash returns the REGISTRY-compatibility hash for
// the repo's effective promptset — the SHA-256 over the 4 shared
// phases (see promptset.RegistryHashPromptset). Used by
// contribute_source to tag decompositions pushed to the registry
// and by pull workers to seed the active entry of the accepted set.
// Falls back to promptset.DefaultRegistryHash when the resolver is
// nil or the effective promptset is unknown (which can only happen
// when a repo points at a deleted custom promptset AND the global
// default config is also unset — both fall back to the built-in).
func (r *PromptsetResolver) EffectiveRegistryHash(ctx context.Context, repoID pgtype.UUID) string {
	fullHash := r.EffectiveHash(ctx, repoID)
	if r.resolver == nil {
		return promptset.DefaultRegistryHash
	}
	ps := r.resolver.ResolveOrDefault(fullHash)
	if ps.RegistryHash != "" {
		return ps.RegistryHash
	}
	return promptset.RegistryHashPromptset(ps)
}

// AcceptedRegistryHashes returns the set of REGISTRY-compatibility
// hashes a repo will admit on registry pull: the effective hash plus
// every entry in accepted_promptset_hashes, each mapped from the
// stored FULL catalog hash to its compatibility hash via the
// resolver. The pull worker feeds this into RelevanceFilter.AcceptedPromptsets.
// When the system pool is nil, returns just the effective registry
// hash. Unknown full hashes (a repo pointing at a deleted custom
// promptset) are dropped rather than expanded to the default — the
// active hash already covers the "default" case via
// EffectiveRegistryHash, and a stale accepted entry should not
// silently broaden the admit set.
func (r *PromptsetResolver) AcceptedRegistryHashes(ctx context.Context, repoID pgtype.UUID) []string {
	effective := r.EffectiveRegistryHash(ctx, repoID)
	if r.systemQueries == nil || r.resolver == nil {
		return []string{effective}
	}
	row, err := r.systemQueries.GetRepositoryPromptset(ctx, repoID)
	if err != nil {
		return []string{effective}
	}
	set := map[string]bool{effective: true}
	for _, fullHash := range row.AcceptedPromptsetHashes {
		if fullHash == "" {
			continue
		}
		ps, ok := r.resolver.Get(fullHash)
		if !ok {
			continue
		}
		rh := ps.RegistryHash
		if rh == "" {
			rh = promptset.RegistryHashPromptset(ps)
		}
		if rh != "" {
			set[rh] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	return out
}

// AcceptedHashes is a legacy alias kept for backward compatibility
// with callers that have not been migrated to the registry-hash
// model. New callers should use AcceptedRegistryHashes. Returns the
// FULL catalog hashes (active + accepted), NOT the compatibility
// hashes — do NOT feed this into RelevanceFilter.AcceptedPromptsets.
//
// Deprecated: use AcceptedRegistryHashes.
func (r *PromptsetResolver) AcceptedHashes(ctx context.Context, repoID pgtype.UUID) []string {
	effective := r.EffectiveHash(ctx, repoID)
	if r.systemQueries == nil {
		return []string{effective}
	}
	row, err := r.systemQueries.GetRepositoryPromptset(ctx, repoID)
	if err != nil {
		return []string{effective}
	}
	set := map[string]bool{effective: true}
	for _, h := range row.AcceptedPromptsetHashes {
		if h != "" {
			set[h] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	return out
}

// LogEffective logs the resolved promptset for a job so an operator
// can see which philosophy a repo ran under. Called once at Work()
// start by each AI-using worker. No-op when the resolved promptset
// is the built-in (the common case) to keep logs quiet.
func (r *PromptsetResolver) LogEffective(ctx context.Context, repoID pgtype.UUID, task string) {
	if r == nil {
		return
	}
	h := r.EffectiveHash(ctx, repoID)
	if h != promptset.DefaultHash {
		log.Printf("promptset_resolver: task %s repo %s running under promptset %s", task, repoID.String(), h)
	}
}

// containsString reports whether s is in list. Small helper used by
// the registry pull filter to check a decomposition's promptset_hash
// against the repo's accepted set.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}