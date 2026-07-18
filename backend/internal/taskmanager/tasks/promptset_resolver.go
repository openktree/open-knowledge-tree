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
// The resolver also exposes AcceptedHashes for the registry pull
// worker: the set of hashes a repo will admit on pull (active always
// included, plus any in accepted_promptset_hashes). A remote
// decomposition whose promptset_hash is not in this set is skipped,
// which is what keeps decompositions from different promptsets from
// mixing at the registry level.
type PromptsetResolver struct {
	cfg          *config.Config
	systemQueries *store.Queries
	resolver     *promptset.Resolver
}

// NewPromptsetResolver constructs a resolver. systemQueries is the
// default-pool *store.Queries (the same one ModelResolver holds);
// resolver is the promptset.Resolver built in cmd/app/api.go. Nil
// systemQueries / resolver is safe — Effective returns the built-in
// Default and AcceptedHashes returns just the effective hash, so a
// deployment that hasn't wired the system pool still runs (every
// repo inherits the built-in philosophy).
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

// EffectiveHash returns the effective promptset hash for a repo as a
// non-empty string (the built-in hash when no override is set). Used
// by workers to tag fact/concept inserts with promptset_hash without
// needing the full Promptset struct.
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

// AcceptedHashes returns the set of promptset hashes a repo will
// admit on registry pull: the effective hash plus every entry in
// accepted_promptset_hashes. The pull worker filters remote
// decompositions to those whose promptset_hash is in this set. When
// the system pool is nil, returns just the effective hash.
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