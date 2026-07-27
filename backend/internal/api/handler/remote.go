package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/audit"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
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

// RemoteAcceptedHashesResolver is the minimal contract the remote
// handler needs to resolve the set of REGISTRY-compatibility
// promptset hashes a repo will admit on pull (the active hash plus
// every accepted_promptset_hashes entry, mapped to compatibility
// hashes). Injected from the task manager's PromptsetResolver so
// the handler doesn't import the tasks package (which would cycle).
// Nil is safe — the pull falls back to promptset.DefaultRegistryHashes
// only (the built-in philosophy is always pullable).
type RemoteAcceptedHashesResolver interface {
	AcceptedRegistryHashes(ctx context.Context, repoID pgtype.UUID) []string
}

// RemoteInboundMapperFactory builds an inbound context mapper for a
// repo so the sync pull path honors the repo's unmapped_context_policy
// (skip | auto_add | catch_all), mirroring the batch pull worker.
// Injected from the task manager so the handler doesn't import tasks.
// Nil is safe — the pull imports contexts verbatim (the legacy
// behavior before context mapping shipped).
type RemoteInboundMapperFactory interface {
	NewInboundMapper(ctx context.Context, repoID pgtype.UUID) (registry.InboundContextMapper, error)
}

// Remote provides endpoints for browsing and pulling sources from
// a remote knowledge registry. It is a no-op when no registry is
// configured (the client map is empty) or when the per-repo
// `registry_enabled` flag is false (each handler resolves the repo's
// client + enabled flag and returns 503 when the integration is off
// for that repo).
type Remote struct {
	clients           *registry.ClientMap
	cfg               config.ProvidersConfig
	store             *store.Queries
	dedupEnqueuer     RemoteDedupEnqueuer
	pullBatchEnqueuer RemotePullBatchEnqueuer
	// registryServices is the per-registry Service map the sync
	// PullSource handler uses to take the filter-aware pull path
	// (the same path the batch worker takes). Nil falls back to
	// the legacy Client.IsAllowedModel path (preserved for tests
	// and deployments that haven't wired the ServiceMap).
	registryServices *registry.ServiceMap
	// acceptedHashesResolver resolves the repo's accepted
	// REGISTRY-compatibility promptset hashes for the sync pull's
	// RelevanceFilter. Nil = accept only the default philosophy.
	acceptedHashesResolver RemoteAcceptedHashesResolver
	// inboundMapperFactory builds the inbound context mapper for
	// the sync pull so it honors the repo's unmapped_context_policy.
	// Nil = import contexts verbatim (legacy behavior).
	inboundMapperFactory RemoteInboundMapperFactory
	// audit records ingestion_start events for remote pulls. Set
	// via SetAuditRecorder; nil in tests that don't exercise the
	// audit pipeline. When nil, PullSource/PullBatch skip the
	// audit write (best-effort, never blocks the request).
	audit audit.Recorder
}

func NewRemote(clients *registry.ClientMap, cfg config.ProvidersConfig) *Remote {
	return &Remote{clients: clients, cfg: cfg}
}

// SetAuditRecorder wires the audit recorder the PullSource/PullBatch
// handlers use to emit ingestion_start events. Optional: when nil,
// the audit write is skipped (best-effort). Idempotent.
func (h *Remote) SetAuditRecorder(r audit.Recorder) {
	h.audit = r
}

// recordPullAudit emits an ingestion_start audit event for a remote
// registry pull. Best-effort: a nil recorder is a no-op, and the
// write happens on a background goroutine (RecordAsync) so it never
// blocks the request. The actor + repo are read from the request
// context (set by AuthRequired + WithRepoQueries).
func (h *Remote) recordPullAudit(r *http.Request, repoID pgtype.UUID, action, target string, detail map[string]any) {
	if h.audit == nil {
		return
	}
	uid := httputil.RequestUserID(r.Context())
	h.audit.RecordAsync(audit.Event{
		UserID:       uid,
		Action:       action,
		Object:       rbac.Objects.Sources,
		RepositoryID: repoID,
		Target:       target,
		Detail:       detail,
	})
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

// SetRegistryServices wires the per-registry Service map the sync
// PullSource handler uses to take the filter-aware pull path (the
// same path the pull_remote_batch worker takes). Called by
// api.Handler.SetRegistryServices after the ServiceMap is built.
// Nil is safe — the pull falls back to the legacy
// Client.IsAllowedModel path (which uses the global allowed_models
// config only, ignoring the per-repo override).
func (h *Remote) SetRegistryServices(svc *registry.ServiceMap) {
	h.registryServices = svc
}

// SetAcceptedHashesResolver wires the resolver that returns the
// repo's accepted REGISTRY-compatibility promptset hashes for the
// sync pull's RelevanceFilter. Called by api.Handler after the task
// manager constructs the PromptsetResolver. Nil is safe — the pull
// admits only promptset.DefaultRegistryHashes (the built-in
// philosophy is always pullable; custom promptsets are not).
func (h *Remote) SetAcceptedHashesResolver(r RemoteAcceptedHashesResolver) {
	h.acceptedHashesResolver = r
}

// SetInboundMapperFactory wires the factory that builds the inbound
// context mapper for the sync pull so it honors the repo's
// unmapped_context_policy. Called by api.Handler after the task
// manager is constructed. Nil is safe — the pull imports contexts
// verbatim (the legacy behavior before context mapping shipped).
func (h *Remote) SetInboundMapperFactory(f RemoteInboundMapperFactory) {
	h.inboundMapperFactory = f
}

// resolveClient resolves the per-repo registry client from the
// repo's registry_id column + the registry_enabled flag. Returns:
//   - (client, regID, cfg, true, "")  when the integration is on and
//     the configured registry has a client. The caller proceeds.
//     regID is the registry id ("default" when the column is NULL)
//     the caller uses to look up the Service via the ServiceMap.
//   - (nil, "", _, false, "remote registry is not configured") when no
//     registry is configured at all (503).
//   - (nil, "", _, false, "remote registry is disabled for this repository")
//     when the repo has turned the integration off (503).
//   - (nil, "", _, false, "registry_id %q is not configured") when the
//     repo's registry_id points at a registry that's no longer in
//     the config (503).
//
// The fourth return value is true only when the caller should
// proceed; the fifth is the error message to surface in the 503.
func (h *Remote) resolveClient(r *http.Request) (*registry.Client, string, config.RegistryConfig, bool, string) {
	if h.clients == nil || !h.clients.IsConfigured() {
		return nil, "", config.RegistryConfig{}, false, "remote registry is not configured"
	}
	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		return nil, "", config.RegistryConfig{}, false, "could not resolve repository ID"
	}
	if h.store == nil {
		return nil, "", config.RegistryConfig{}, false, "store not configured"
	}
	regCfg, err := h.store.GetRepositoryRegistryConfig(r.Context(), repoID)
	if err != nil {
		return nil, "", config.RegistryConfig{}, false, "reading repository registry config: " + err.Error()
	}
	if !regCfg.RegistryEnabled {
		return nil, "", config.RegistryConfig{}, false, "remote registry is disabled for this repository"
	}
	regID := "default"
	if regCfg.RegistryID != nil && *regCfg.RegistryID != "" {
		regID = *regCfg.RegistryID
	}
	client, rcCfg, ok := h.clients.Client(regID)
	if !ok || !client.IsConfigured() {
		return nil, "", config.RegistryConfig{}, false, fmt.Sprintf("registry_id %q is not configured", regID)
	}
	return client, regID, rcCfg, true, ""
}

// GetSource proxies the registry's GET /api/v1/sources/{id} so the
// frontend can fetch the full SourcePackage (metadata + decomposition
// model list) for a remote source without going direct to the
// registry (which would expose CORS and auth issues). Returns the
// raw *SourcePackage as JSON. Gated on remote:read.
func (h *Remote) GetSource(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok, msg := h.resolveClient(r)
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

// GetDecomposition serves a single model's facts/concepts/links
// for the UI's source detail dialog. It prefers the registry's
// presigned R2 URL (fast path: registry issues a tiny presigned
// URL, backend fetches the raw blob from object storage and
// returns it as JSON) and falls back to the registry's
// PullDecomposition (which buffers + re-marshals the blob in
// the registry VM) when no presigned URL is available
// (filesystem backend, dev mode). Gated on remote:read.
func (h *Remote) GetDecomposition(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok, msg := h.resolveClient(r)
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

	// Fast path: registry issues a presigned URL, backend fetches
	// the blob from R2 directly. Avoids the registry's pullSem
	// (bounded at 8) and the S3->registry->backend double-buffer
	// that pinned ~80-100MB in the registry VM per concurrent
	// pull. The response body is the same shape (a
	// *DecompositionPackage JSON), so the frontend is unchanged.
	_, body, err := client.FetchDecompositionPresigned(r.Context(), remoteID, modelID)
	if err == nil && body != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	// Fallback: no presigned URL (filesystem backend, dev, or
	// older registry). Use the registry's PullDecomposition which
	// buffers + re-marshals. Log a warning so the operator knows
	// the slow path is active.
	if err != nil {
		log.Printf("remote: presigned fast path failed for %s/%s, falling back to registry re-marshal: %v", remoteID, modelID, err)
	}
	pkg, pullErr := client.PullDecomposition(r.Context(), remoteID, modelID)
	if pullErr != nil {
		httputil.WriteError(w, http.StatusBadGateway, "pulling decomposition from registry: "+pullErr.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, pkg)
}

// ListSources returns a paginated, searchable list of sources from
// the remote registry. Supports ?limit=N&offset=N&q=keyword.
// Each source is annotated with an `exists` boolean indicating whether
// a source with the same URL or DOI already exists in the local repo.
func (h *Remote) ListSources(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok, msg := h.resolveClient(r)
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
// via PullOneRemoteSource. When the ServiceMap + filter dependencies
// are wired, this handler takes the same filter-aware path as the
// batch worker: it builds a RelevanceFilter (per-repo allowed_models
// override, accepted promptset hashes, sync level, inbound context
// mapper) and threads a *registry.Service into RemotePullDeps so
// PullOneRemoteSource calls Service.PullRelevantDecomposition. When
// the ServiceMap is nil (tests, or a deployment that hasn't wired
// it), it falls back to the legacy Client.IsAllowedModel path so
// callers keep working.
func (h *Remote) PullSource(w http.ResponseWriter, r *http.Request) {
	client, regID, rcCfg, ok, msg := h.resolveClient(r)
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

	deps := RemotePullDeps{
		Client:        client,
		Queries:       queries,
		SystemQueries: h.store,
		RepoID:        repoID,
		DedupEnqueuer: h.dedupEnqueuer,
		PullFilter:    pullFilter,
	}

	// Take the filter-aware path when the ServiceMap is wired. This
	// mirrors pull_remote_batch.go: the Service applies the
	// per-repo allowed_models override, accepted promptset hashes,
	// sync level, and inbound context mapper during the pull, so
	// the sync "Pull" button behaves identically to the batch
	// "Pull page" / "Pull all results" buttons. Without this path
	// the legacy Client.IsAllowedModel branch uses the GLOBAL
	// allowed_models config only and silently ignores the per-repo
	// override — which is why a repo that enabled a model in
	// Settings still imported 0 facts on the sync pull.
	if h.registryServices != nil {
		svcCtx := registry.WithRegistryID(r.Context(), regID)
		svc := h.registryServices.Service(svcCtx)

		// Build the inbound context mapper so the sync pull
		// honors the repo's unmapped_context_policy (skip |
		// auto_add | catch_all), matching the batch worker. Nil
		// factory = import verbatim (legacy behavior).
		var mapper registry.InboundContextMapper
		if h.inboundMapperFactory != nil {
			if m, err := h.inboundMapperFactory.NewInboundMapper(r.Context(), repoID); err != nil {
				log.Printf("remote: building inbound context mapper: %v", err)
			} else {
				mapper = m
			}
		}

		// autoAdd seeds a repository_contexts row for the
		// auto_add policy. The mapper invokes it when the
		// registry label isn't already a local context.
		autoAdd := func(registryLabel string) {
			if _, err := h.store.SeedRepositoryContext(r.Context(), store.SeedRepositoryContextParams{
				RepositoryID: repoID,
				Context:      registryLabel,
				IsCustom:     true,
				Description:  "",
			}); err != nil {
				log.Printf("remote: auto-adding context %q: %v", registryLabel, err)
			}
		}

		// Resolve the repo's accepted REGISTRY-compatibility
		// hashes so the per-decomposition check admits
		// decompositions from compatible promptsets. Nil resolver
		// = accept only the default philosophy
		// (promptset.DefaultRegistryHashes).
		var acceptedHashes []string
		if h.acceptedHashesResolver != nil {
			acceptedHashes = h.acceptedHashesResolver.AcceptedRegistryHashes(r.Context(), repoID)
		}

		deps.Service = svc
		deps.Filter = &registry.RelevanceFilter{
			AllowedModels:      resolveAllowedModels(r.Context(), h.store, repoID, rcCfg.AllowedModels),
			AcceptedPromptsets: acceptedHashes,
			DefaultAccepted:    promptset.DefaultRegistryHashes,
			SyncLevel:          pullFilter,
			ContextMapper:      mapper,
			AutoAdd:            autoAdd,
		}
	}

	result, err := PullOneRemoteSource(r.Context(), deps, remoteID)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "pulling source from registry: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, result)
	h.recordPullAudit(r, repoID, rbac.AuditActionIngestionStart, result.SourceID, map[string]any{
		"remote_source_id":  remoteID,
		"imported_facts":    result.ImportedFacts,
		"imported_concepts": result.ImportedConcepts,
		"source_url":        result.URL,
	})
}

// resolveAllowedModels returns the model whitelist to use for a
// registry pull: the per-repo allowed_models column when non-NULL,
// otherwise the global registry config's allowed_models (the
// fallback). Mirrors tasks.resolveAllowedModels so the handler
// doesn't import the tasks package (which would cycle: tasks
// imports handler for RemotePullDeps).
func resolveAllowedModels(ctx context.Context, systemQueries *store.Queries, repoID pgtype.UUID, fallback []string) []string {
	if systemQueries == nil {
		return fallback
	}
	perRepo, err := systemQueries.GetRepositoryAllowedModels(ctx, repoID)
	if err != nil {
		return fallback
	}
	if perRepo != nil {
		return perRepo
	}
	return fallback
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
	if _, _, _, ok, msg := h.resolveClient(r); !ok {
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
		"job_id":              jobID,
		"remote_source_count": len(body.RemoteSourceIDs),
		"status":              "queued",
	})
	h.recordPullAudit(r, repoID, rbac.AuditActionIngestionStart, "pull_batch", map[string]any{
		"job_id":              jobID,
		"remote_source_count": len(body.RemoteSourceIDs),
	})
}
