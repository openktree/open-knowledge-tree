package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// remoteSourceEnvelope wraps a registry source with local existence info.
type remoteSourceEnvelope struct {
	registry.RemoteSourceMeta
	Exists bool `json:"exists"`
}

// listRemoteSourcesResponse is the response shape for ListSources.
type listRemoteSourcesResponse struct {
	Sources []remoteSourceEnvelope `json:"sources"`
	Total   int                    `json:"total"`
}

// RemoteDedupEnqueuer is the minimal contract the remote handler needs
// from the task manager to enqueue an embed_facts pass (which chains to
// deduplicate_facts → extract_concepts → cleanup) after pulling a
// source from the registry. The pulled facts are created as 'new' and
// need the standard dedup pipeline to avoid local duplicates.
type RemoteDedupEnqueuer interface {
	EnqueueEmbedFacts(ctx context.Context, repositoryID, sourceID string) error
}

// RemotePullBatchEnqueuer is the minimal contract the remote handler
// needs from the task manager to enqueue a pull_remote_batch job
// (the "Pull page" / "Pull all results" buttons on the Remote page).
// Returns the River job id so the UI can poll for completion.
type RemotePullBatchEnqueuer interface {
	EnqueuePullRemoteBatch(ctx context.Context, repositoryID string, remoteSourceIDs []string) (string, error)
}

// Remote provides endpoints for browsing and pulling sources from
// a remote knowledge registry. It is a no-op when no registry is
// configured (the client map is empty) or when the per-repo
// `registry_enabled` flag is false (each handler resolves the repo's
// client + enabled flag and returns 503 when the integration is off
// for that repo).
type Remote struct {
	clients            *registry.ClientMap
	cfg                config.ProvidersConfig
	store              *store.Queries
	dedupEnqueuer     RemoteDedupEnqueuer
	pullBatchEnqueuer RemotePullBatchEnqueuer
}

func NewRemote(clients *registry.ClientMap, cfg config.ProvidersConfig) *Remote {
	return &Remote{clients: clients, cfg: cfg}
}

// SetClientMap wires the per-registry client map. Called by the
// wiring layer (api.Handler.SetRegistryClients) after the map is
// built from config. Nil is safe — every handler treats a nil map
// as "no registries configured" and returns 503.
func (h *Remote) SetClientMap(m *registry.ClientMap) {
	h.clients = m
}

// SetStore wires the system-pool store the remote handler uses to
// look up the per-repo registry_id + registry_enabled flags. Called
// by the wiring layer alongside SetClientMap.
func (h *Remote) SetStore(s *store.Queries) {
	h.store = s
}

// SetDedupEnqueuer wires the task enqueuer used to kick off the
// embed→dedup pipeline after a pull. Called by api.Handler after
// the task manager is constructed. Nil disables the enqueue (pulled
// facts stay 'new' until a periodic sweep picks them up).
func (h *Remote) SetDedupEnqueuer(eq RemoteDedupEnqueuer) {
	h.dedupEnqueuer = eq
}

// SetPullBatchEnqueuer wires the task enqueuer used to kick off a
// pull_remote_batch job (the "Pull page" / "Pull all results"
// buttons). Called by api.Handler after the task manager is
// constructed. Nil disables the batch-pull endpoint (returns 503).
func (h *Remote) SetPullBatchEnqueuer(eq RemotePullBatchEnqueuer) {
	h.pullBatchEnqueuer = eq
}

// resolveClient resolves the per-repo registry client from the
// repo's registry_id column + the registry_enabled flag. Returns:
//   - (client, cfg, true, "")  when the integration is on and the
//     configured registry has a client. The caller proceeds.
//   - (nil, _, false, "remote registry is not configured") when no
//     registry is configured at all (503).
//   - (nil, _, false, "remote registry is disabled for this repository")
//     when the repo has turned the integration off (503).
//   - (nil, _, false, "registry_id %q is not configured") when the
//     repo's registry_id points at a registry that's no longer in
//     the config (503).
//
// The third return value is true only when the caller should
// proceed; the fourth is the error message to surface in the 503.
func (h *Remote) resolveClient(r *http.Request) (*registry.Client, config.RegistryConfig, bool, string) {
	if h.clients == nil || !h.clients.IsConfigured() {
		return nil, config.RegistryConfig{}, false, "remote registry is not configured"
	}
	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		return nil, config.RegistryConfig{}, false, "could not resolve repository ID"
	}
	if h.store == nil {
		return nil, config.RegistryConfig{}, false, "store not configured"
	}
	regCfg, err := h.store.GetRepositoryRegistryConfig(r.Context(), repoID)
	if err != nil {
		return nil, config.RegistryConfig{}, false, "reading repository registry config: " + err.Error()
	}
	if !regCfg.RegistryEnabled {
		return nil, config.RegistryConfig{}, false, "remote registry is disabled for this repository"
	}
	regID := "default"
	if regCfg.RegistryID != nil && *regCfg.RegistryID != "" {
		regID = *regCfg.RegistryID
	}
	client, rcCfg, ok := h.clients.Client(regID)
	if !ok || !client.IsConfigured() {
		return nil, config.RegistryConfig{}, false, fmt.Sprintf("registry_id %q is not configured", regID)
	}
	return client, rcCfg, true, ""
}

// GetSource proxies the registry's GET /api/v1/sources/{id} so the
// frontend can fetch the full SourcePackage (metadata + decomposition
// model list) for a remote source without going direct to the
// registry (which would expose CORS and auth issues). Returns the
// raw *SourcePackage as JSON. Gated on remote:read.
func (h *Remote) GetSource(w http.ResponseWriter, r *http.Request) {
	client, _, ok, msg := h.resolveClient(r)
	if !ok {
		httputil.WriteError(w, http.StatusServiceUnavailable, msg)
		return
	}
	remoteID := chi.URLParam(r, "sourceID")
	if remoteID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "sourceID is required")
		return
	}
	pkg, err := client.PullSource(r.Context(), remoteID)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "pulling source from registry: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, pkg)
}

// GetDecomposition proxies the registry's
// GET /api/v1/sources/{id}/decompositions/{model} so the frontend
// can browse a single model's facts/concepts on demand without
// going direct to the registry. Returns the raw
// *DecompositionPackage as JSON. Gated on remote:read.
func (h *Remote) GetDecomposition(w http.ResponseWriter, r *http.Request) {
	client, _, ok, msg := h.resolveClient(r)
	if !ok {
		httputil.WriteError(w, http.StatusServiceUnavailable, msg)
		return
	}
	remoteID := chi.URLParam(r, "sourceID")
	if remoteID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "sourceID is required")
		return
	}
	modelID := chi.URLParam(r, "modelID")
	if modelID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "modelID is required")
		return
	}
	// Chi v5 does not URL-decode %2F (/) in path params, so
	// "google%2Fgemma-4-31b-it" arrives still-encoded. Decode it
	// before passing to the registry client (which re-encodes
	// with url.PathEscape for the outbound HTTP call).
	if decoded, err := url.PathUnescape(modelID); err == nil {
		modelID = decoded
	}
	pkg, err := client.PullDecomposition(r.Context(), remoteID, modelID)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "pulling decomposition from registry: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, pkg)
}

// ListSources returns a paginated, searchable list of sources from
// the remote registry. Supports ?limit=N&offset=N&q=keyword.
// Each source is annotated with an `exists` boolean indicating whether
// a source with the same URL or DOI already exists in the local repo.
func (h *Remote) ListSources(w http.ResponseWriter, r *http.Request) {
	client, _, ok, msg := h.resolveClient(r)
	if !ok {
		httputil.WriteError(w, http.StatusServiceUnavailable, msg)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	q := r.URL.Query().Get("q")

	result, err := client.ListSources(r.Context(), limit, offset, q)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Annotate each source with whether it already exists in the local repo.
	pool := appmw.PoolFromContext(r.Context())
	envelopes := make([]remoteSourceEnvelope, len(result.Sources))
	if pool != nil {
		repoID, ok := appmw.RepoIDFromContext(r.Context())
		if ok {
			queries := store.New(pool)
			urls := make([]string, 0, len(result.Sources))
			dois := make([]string, 0, len(result.Sources))
			for _, src := range result.Sources {
				if src.URL != "" {
					urls = append(urls, src.URL)
				}
				if src.DOI != "" {
					dois = append(dois, src.DOI)
				}
			}
			existing, lookupErr := queries.GetExistingSourceURLsAndDOIsByRepo(r.Context(), store.GetExistingSourceURLsAndDOIsByRepoParams{
				RepositoryID: repoID,
				Column2:      urls,
				Column3:      dois,
			})
			if lookupErr == nil {
				existSet := make(map[string]bool, len(existing))
				for _, row := range existing {
					if row.Url != "" {
						existSet[row.Url] = true
					}
					if row.Doi != nil && *row.Doi != "" {
						existSet["doi:"+*row.Doi] = true
					}
				}
				for i, src := range result.Sources {
					envelopes[i].RemoteSourceMeta = src
					envelopes[i].Exists = existSet[src.URL] || (src.DOI != "" && existSet["doi:"+src.DOI])
				}
				httputil.WriteJSON(w, http.StatusOK, listRemoteSourcesResponse{
					Sources: envelopes,
					Total:   result.Total,
				})
				return
			}
		}
	}

	// Fallback: no local pool or repo — pass through without exists info.
	for i, src := range result.Sources {
		envelopes[i].RemoteSourceMeta = src
	}
	httputil.WriteJSON(w, http.StatusOK, listRemoteSourcesResponse{
		Sources: envelopes,
		Total:   result.Total,
	})
}

// PullSource pulls a source (with its facts and concepts) from the
// remote registry into the local repository. The remote source is
// identified by its registry-side source ID (from ListSources).
//
// The pull core is shared with the async pull_remote_batch worker
// via PullOneRemoteSource. This handler does not apply the inbound
// context mapper (single pulls import contexts verbatim, matching
// the pre-context-mapping behavior); the batch worker builds a
// mapper per repo so bulk pulls honor the repo's unmapped-context
// policy. Passing nil for the mapper keeps the two paths consistent
// for the common case where the repo hasn't configured mappings.
func (h *Remote) PullSource(w http.ResponseWriter, r *http.Request) {
	client, _, ok, msg := h.resolveClient(r)
	if !ok {
		httputil.WriteError(w, http.StatusServiceUnavailable, msg)
		return
	}
	remoteID := chi.URLParam(r, "sourceID")
	if remoteID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "sourceID is required")
		return
	}

	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "could not resolve repository ID")
		return
	}

	// Per-repo pull level (migration 0044). Nil filter (when the
	// read fails) defaults to full "concepts" pull.
	var pullFilter *registry.SyncLevelFilter
	if syncLevels, err := h.store.GetRepositorySyncLevels(r.Context(), repoID); err == nil {
		pullFilter = registry.NewSyncLevelFilter(registry.ParseSyncLevel(syncLevels.RegistryPullLevel))
	}

	result, err := PullOneRemoteSource(r.Context(), RemotePullDeps{
		Client:        client,
		Queries:        queries,
		SystemQueries: h.store,
		RepoID:        repoID,
		Mapper:        nil,
		DedupEnqueuer: h.dedupEnqueuer,
		PullFilter:    pullFilter,
	}, remoteID)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "pulling source from registry: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, result)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// PullBatch handles POST /repositories/{repoID}/remote/pull-batch.
//
// Enqueues a pull_remote_batch job that imports a list of remote
// registry source IDs into the local repo. The body is
// {"remote_source_ids": ["id1", "id2", ...]}. Returns 202 + job_id
// so the UI can poll for completion. The worker pulls each source,
// applies the inbound context mapper, and chains embed_facts per
// source. A per-source error is logged and skipped; the batch
// continues so one bad source doesn't fail the whole job.
//
// Used by the "Pull page" button (the current page's source IDs) and
// the "Pull all results" button (every source ID matching the
// current query — the frontend paginates through /remote and
// collects the IDs before calling this endpoint).
func (h *Remote) PullBatch(w http.ResponseWriter, r *http.Request) {
	if _, _, ok, msg := h.resolveClient(r); !ok {
		httputil.WriteError(w, http.StatusServiceUnavailable, msg)
		return
	}
	if h.pullBatchEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "pull-batch enqueuer not configured")
		return
	}
	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "could not resolve repository ID")
		return
	}
	var body struct {
		RemoteSourceIDs []string `json:"remote_source_ids"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.RemoteSourceIDs) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "remote_source_ids is required")
		return
	}
	// Cap the batch size so a runaway "pull all" doesn't enqueue a
	// 100k-source job. The frontend paginates and collects; the cap
	// is a safety net, not the expected path.
	const maxBatch = 500
	if len(body.RemoteSourceIDs) > maxBatch {
		httputil.WriteError(w, http.StatusBadRequest, fmt.Sprintf("too many source IDs: max %d per batch, got %d", maxBatch, len(body.RemoteSourceIDs)))
		return
	}
	jobID, err := h.pullBatchEnqueuer.EnqueuePullRemoteBatch(r.Context(), uuidFromPgtype(repoID), body.RemoteSourceIDs)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, map[string]any{
		"job_id":             jobID,
		"remote_source_count": len(body.RemoteSourceIDs),
		"status":             "queued",
	})
}
