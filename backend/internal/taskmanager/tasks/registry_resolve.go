package tasks

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// resolveRepoRegistryClient resolves the per-repo registry client
// from the repo's registry_id + registry_enabled columns. Returns:
//   - (regID, client, nil) when the integration is on and the configured
//     registry has a live client. The caller proceeds.
//   - ("", nil, err) when the integration is off, no registry is
//     configured, or the stored registry_id no longer matches a
//     configured registry. The caller treats the error as "skip
//     this registry operation" (log + return nil from Work).
//
// The regID is returned so the caller can inject it into the context
// (via registry.WithRegistryID) for the ServiceMap / cache provider
// to resolve the right client.
//
// This is the shared defense-in-depth gate for the contribute and
// pull workers: the HTTP layer already refuses to enqueue when the
// integration is off, but a job enqueued before a toggle-off should
// still no-op rather than push to a registry the repo has opted out
// of.
func resolveRepoRegistryClient(
	ctx context.Context,
	systemQueries *store.Queries,
	clients *registry.ClientMap,
	repoID pgtype.UUID,
) (string, *registry.Client, error) {
	if clients == nil || !clients.IsConfigured() {
		return "", nil, fmt.Errorf("registry not configured")
	}
	regCfg, err := systemQueries.GetRepositoryRegistryConfig(ctx, repoID)
	if err != nil {
		return "", nil, fmt.Errorf("reading repository registry config: %w", err)
	}
	if !regCfg.RegistryEnabled {
		return "", nil, fmt.Errorf("registry integration disabled for this repository")
	}
	regID := "default"
	if regCfg.RegistryID != nil && *regCfg.RegistryID != "" {
		regID = *regCfg.RegistryID
	}
	client, _, ok := clients.Client(regID)
	if !ok || !client.IsConfigured() {
		return "", nil, fmt.Errorf("registry_id %q is not configured", regID)
	}
	return regID, client, nil
}

// logSkip is a thin wrapper so the contribute/pull workers can log a
// skip reason at a consistent level without each call site spelling
// out the prefix.
func logSkip(worker string, repoID string, reason string) {
	log.Printf("%s: skipping registry op for repo %s: %s", worker, repoID, reason)
}

// resolveAllowedModels returns the model whitelist to use for a
// registry cache import. The per-repo `allowed_models` column, when
// non-NULL, replaces the global registry config's `allowed_models`
// (per-repo replaces global). When NULL, the global config value
// is used as the fallback. The returned slice is passed to
// registry.IsAllowed by the import loop.
func resolveAllowedModels(
	ctx context.Context,
	systemQueries *store.Queries,
	repoID pgtype.UUID,
	fallback []string,
) []string {
	perRepo, err := systemQueries.GetRepositoryAllowedModels(ctx, repoID)
	if err != nil {
		return fallback
	}
	if perRepo != nil {
		return perRepo
	}
	return fallback
}