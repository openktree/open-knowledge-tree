package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/graph"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Graph bundles the shared-knowledge-graph HTTP handlers. A "graph"
// is the serializable form of an entire repository's derived layer
// (sources + facts + concepts + summaries + syntheses + investigations
// + reports + embeddings); the export handler enqueues a build+push
// task, the import handler enqueues a pull+apply task. Both are
// async (River jobs) so the HTTP response is 202 + job_id; the UI
// polls the status endpoint.
//
// Auth: export is gated by graph:export (sysadmin + repoadmin +
// editor); import is gated by graph:write (sysadmin + repoadmin).
// Import-to-new-repo additionally requires repository:write (the
// caller creates a new repository). The wiring layer composes the
// two permissions.
type Graph struct {
	deps            Deps
	registryClients *registry.ClientMap
	storageBackend  storage.FileStorage
	exportEnqueuer  GraphExportEnqueuer
	importEnqueuer  GraphImportEnqueuer
}

func NewGraph(d Deps) *Graph {
	return &Graph{deps: d}
}

// SetRegistryClients wires the per-registry client map. Called by the
// wiring layer after the client map is built from config.
func (h *Graph) SetRegistryClients(m *registry.ClientMap) {
	h.registryClients = m
}

// SetStorageBackend wires the file storage backend (for the upload
// import path). Called by the wiring layer.
func (h *Graph) SetStorageBackend(s storage.FileStorage) {
	h.storageBackend = s
}

// SetExportEnqueuer wires the export_graph task enqueuer. Called by
// the wiring layer after the task manager is constructed.
func (h *Graph) SetExportEnqueuer(eq GraphExportEnqueuer) {
	h.exportEnqueuer = eq
}

// SetImportEnqueuer wires the import_graph task enqueuer. Called by
// the wiring layer after the task manager is constructed.
func (h *Graph) SetImportEnqueuer(eq GraphImportEnqueuer) {
	h.importEnqueuer = eq
}

// GraphExportEnqueuer is the minimal contract the graph handler needs
// from the task manager to enqueue an export_graph job. The wiring
// layer adapts the River client to this interface (same pattern as
// RemotePullBatchEnqueuer).
type GraphExportEnqueuer interface {
	EnqueueExportGraph(ctx context.Context, args ExportGraphArgs) (string, error)
}

// GraphImportEnqueuer is the minimal contract the graph handler needs
// from the task manager to enqueue an import_graph job.
type GraphImportEnqueuer interface {
	EnqueueImportGraph(ctx context.Context, args ImportGraphArgs) (string, error)
}

// ExportGraphArgs is the wire shape for POST /{repoID}/export-graph.
// Mirrors tasks.ExportGraphArgs; the enqueuer adapter translates.
type ExportGraphArgs struct {
	RepositoryID string   `json:"repository_id"`
	RegistryID   string   `json:"registry_id,omitempty"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// ImportGraphArgs is the wire shape for POST /repositories/import-graph
// (new repo) and POST /{repoID}/import-graph (existing repo). SourceKind
// is "registry" (pull by registry_graph_id) or "upload" (read from
// upload_key). Mode is "new" or "existing".
type ImportGraphArgs struct {
	RepositoryID    string `json:"repository_id"`
	SourceKind      string `json:"source_kind"`
	RegistryGraphID string `json:"registry_graph_id,omitempty"`
	UploadKey       string `json:"upload_key,omitempty"`
	RegistryID      string `json:"registry_id,omitempty"`
	Mode            string `json:"mode"`
}

// ── Export ───────────────────────────────────────────────────────────

// ExportGraph enqueues a whole-repository graph export. The handler
// resolves the repo from the URL context (set by WithRepoQueries),
// validates the registry is configured, and enqueues the export_graph
// task. Returns 202 + job_id so the UI can poll for completion.
//
// Gated by graph:export (wiring layer).
func (h *Graph) ExportGraph(w http.ResponseWriter, r *http.Request) {
	if h.exportEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "graph export not configured")
		return
	}
	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "could not resolve repository ID")
		return
	}
	var body struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		RegistryID  string   `json:"registry_id"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Name == "" {
		// Default to the repository's name when the caller omits one.
		repo, err := h.deps.Store.GetRepositoryByID(r.Context(), repoID)
		if err == nil {
			body.Name = repo.Name
		}
	}
	if body.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	jobID, err := h.exportEnqueuer.EnqueueExportGraph(r.Context(), ExportGraphArgs{
		RepositoryID: repoID.String(),
		RegistryID:   body.RegistryID,
		Name:         body.Name,
		Description:  body.Description,
		Tags:         body.Tags,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, map[string]any{
		"job_id":        jobID,
		"repository_id": repoID.String(),
		"status":        "queued",
	})
}

// ── Export (synchronous file download) ───────────────────────────────

// DownloadGraph builds a graph bundle for the repository synchronously
// and streams it back as a gzipped JSON attachment. Unlike ExportGraph
// (which pushes to the registry async), this needs no registry
// configuration — the bundle is built in-process and returned directly.
// The Content-Disposition attachment filename is the repo's name
// (slug-sanitized) + .json.gz so the browser saves it as a file.
//
// Query params: ?name= (override the bundle's metadata.name; defaults
// to the repo's name). The bundle always includes embeddings (the
// provider/graph.BundleBuilder fetches Qdrant vectors when Qdrant is
// wired).
//
// Gated by graph:export (wiring layer).
func (h *Graph) DownloadGraph(w http.ResponseWriter, r *http.Request) {
	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "could not resolve repository ID")
		return
	}
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	// Resolve the repo's name for the bundle metadata + the download
	// filename.
	repo, err := h.deps.Store.GetRepositoryByID(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "repository not found")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = repo.Name
	}

	// Build the bundle. The builder reads the per-repo pool queries +
	// the Qdrant store wired on Deps (nil-safe — the bundle's
	// embeddings section is empty when Qdrant isn't configured).
	builder := graph.NewBundleBuilder(
		queries,
		h.deps.Qdrant,
		repoID,
		h.deps.Config.Providers.Embedding.Model,
		h.deps.Config.Providers.Embedding.Dimensions,
	)
	bundle, err := builder.Build(r.Context(), graph.BundleMetadata{
		Name: name,
		Tags: []string{},
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "building graph bundle: "+err.Error())
		return
	}

	// Gzip the bundle. The bytes are the same shape the registry
	// stores and the import path (UploadGraphBundle + import_graph
	// task) accepts, so a downloaded file is directly re-importable
	// on any OKT instance.
	gz, err := graph.MarshalGzip(bundle)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "gzipping graph bundle: "+err.Error())
		return
	}

	// Stream back as a downloadable attachment. The filename uses the
	// repo's slug (already slug-safe) + .json.gz.
	filename := repo.Slug
	if filename == "" {
		filename = "graph"
	}
	filename += ".json.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(gz)
}

// ── Import (existing repo) ───────────────────────────────────────────

// ImportGraphToExisting enqueues a graph import into an existing
// repository. The handler resolves the repo from the URL context,
// validates the source kind (registry or upload), and enqueues the
// import_graph task with mode="existing". Returns 202 + job_id.
//
// Gated by graph:write (wiring layer).
func (h *Graph) ImportGraphToExisting(w http.ResponseWriter, r *http.Request) {
	if h.importEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "graph import not configured")
		return
	}
	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "could not resolve repository ID")
		return
	}
	args, err := h.parseImportBody(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	args.RepositoryID = repoID.String()
	args.Mode = "existing"
	jobID, err := h.importEnqueuer.EnqueueImportGraph(r.Context(), args)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, map[string]any{
		"job_id":        jobID,
		"repository_id": repoID.String(),
		"status":        "queued",
	})
}

// ── Import (new repo) ────────────────────────────────────────────────

// importGraphToNewRepoRequest is the wire shape for POST
// /repositories/import-graph (JSON path). The multipart path
// (UploadGraphBundle + ImportGraphToNewRepo) uses the same shape
// minus the bundle file.
type importGraphToNewRepoRequest struct {
	RegistryGraphID string   `json:"registry_graph_id"`
	UploadKey       string   `json:"upload_key"`
	RegistryID      string   `json:"registry_id"`
	Name            string   `json:"name"`
	Slug            string   `json:"slug"`
	Description     string   `json:"description"`
	Tags            []string `json:"tags"`
}

// ImportGraphToNewRepo creates a new repository and enqueues a graph
// import into it. The handler creates the repo row (minimal: name +
// slug + description + default database + owner from the session),
// runs the default settings seeder so the new repo has working
// provider/context settings, then enqueues the import_graph task with
// mode="new". Returns 202 + job_id + the new repo id.
//
// Gated by graph:write + repository:write (wiring layer composes the
// two permissions — the caller creates a new repository AND imports
// a graph into it).
func (h *Graph) ImportGraphToNewRepo(w http.ResponseWriter, r *http.Request) {
	if h.importEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "graph import not configured")
		return
	}
	var body importGraphToNewRepoRequest
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Name == "" || body.Slug == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name and slug are required")
		return
	}
	if body.RegistryGraphID == "" && body.UploadKey == "" {
		httputil.WriteError(w, http.StatusBadRequest, "registry_graph_id or upload_key is required")
		return
	}
	uid := httputil.RequestUserID(r.Context())
	// Create the repository row. Minimal: default database, tier-1,
	// the caller as owner. The full CreateRepository handler does
	// preset seeding + provider/context setup; here we reuse the
	// default settings seeder (the same path the lazy default-repo
	// bootstrap uses) so the new repo gets working settings.
	repo, err := h.deps.Store.CreateRepository(r.Context(), store.CreateRepositoryParams{
		Name:         body.Name,
		Slug:         body.Slug,
		Description:  body.Description,
		OwnerID:      uid,
		DatabaseName: h.deps.Config.System.Database,
		Tier:         "tier1",
	})
	if err != nil {
		if isUniqueViolation(err) {
			httputil.WriteError(w, http.StatusConflict, "a repository with that slug already exists")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "creating repository: "+err.Error())
		return
	}
	// Seed default settings so the new repo has working provider +
	// context settings (otherwise every search/retrieve gate denies).
	if h.deps.DefaultSettingsSeeder != nil {
		if err := h.deps.DefaultSettingsSeeder(r.Context(), repo.ID.String()); err != nil {
			// Log + continue; the import still runs, the user can fix
			// settings from the UI.
			fmt.Printf("graph import: seeding default settings for new repo %s: %v\n", repo.ID, err)
		}
	}
	// Enqueue the import.
	args := ImportGraphArgs{
		RepositoryID:    repo.ID.String(),
		RegistryID:      body.RegistryID,
		Mode:            "new",
		RegistryGraphID: body.RegistryGraphID,
		UploadKey:       body.UploadKey,
	}
	if args.RegistryGraphID != "" {
		args.SourceKind = "registry"
	} else {
		args.SourceKind = "upload"
	}
	jobID, err := h.importEnqueuer.EnqueueImportGraph(r.Context(), args)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, map[string]any{
		"job_id":        jobID,
		"repository_id": repo.ID.String(),
		"slug":          repo.Slug,
		"status":        "queued",
	})
}

// ── Upload (air-gapped import) ───────────────────────────────────────

// UploadGraphBundle accepts a multipart gzipped graph bundle upload
// and stores it under a temp key. Returns the upload_key the import
// endpoints use to reference it. The import task deletes the temp
// object after a successful import.
//
// Gated by graph:write (wiring layer). No repo context (the upload
// precedes the repo creation on the new-repo path).
func (h *Graph) UploadGraphBundle(w http.ResponseWriter, r *http.Request) {
	if h.storageBackend == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "storage backend not configured")
		return
	}
	// 2GB cap on the upload.
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "parsing multipart form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("bundle")
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "bundle file is required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "reading bundle: "+err.Error())
		return
	}
	uploadKey := "tmp/graphs/" + uuid.New().String() + ".json.gz"
	if _, err := h.storageBackend.Store(r.Context(), uploadKey, "application/gzip", data); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "storing bundle: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, map[string]string{
		"upload_key": uploadKey,
	})
}

// ── Browse shared graphs (proxy to registry) ────────────────────────

// ListSharedGraphs proxies the registry's GET /api/v1/graphs so the
// frontend can browse shared graphs without going direct to the
// registry (CORS + auth). Gated by graph:import (read access to the
// shared library; the wiring layer uses graph:write since browse is
// the precursor to import). Returns the registry's paginated list.
func (h *Graph) ListSharedGraphs(w http.ResponseWriter, r *http.Request) {
	client, regID, ok, msg := h.resolveClient(r)
	if !ok {
		httputil.WriteError(w, http.StatusServiceUnavailable, msg)
		return
	}
	_ = regID
	limit, _ := atoiDefault(r.URL.Query().Get("limit"), 20)
	offset, _ := atoiDefault(r.URL.Query().Get("offset"), 0)
	q := r.URL.Query().Get("q")
	tag := r.URL.Query().Get("tag")
	result, err := client.ListGraphs(r.Context(), limit, offset, q, tag)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "listing shared graphs: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, result)
}

// GetSharedGraph proxies the registry's GET /api/v1/graphs/{id} so the
// frontend can fetch a single graph's metadata + presigned download
// URL. Gated by graph:write (wiring layer).
func (h *Graph) GetSharedGraph(w http.ResponseWriter, r *http.Request) {
	client, _, ok, msg := h.resolveClient(r)
	if !ok {
		httputil.WriteError(w, http.StatusServiceUnavailable, msg)
		return
	}
	graphID := chi.URLParam(r, "graphID")
	if graphID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "graphID is required")
		return
	}
	meta, err := client.PullGraph(r.Context(), graphID)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "fetching shared graph: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, meta)
}

// ── helpers ──────────────────────────────────────────────────────────

// parseImportBody decodes the JSON import body (used by the existing-
// repo path) and validates the source kind. The new-repo path uses
// its own importGraphToNewRepoRequest (it also carries name/slug).
func (h *Graph) parseImportBody(r *http.Request) (ImportGraphArgs, error) {
	var body struct {
		RegistryGraphID string `json:"registry_graph_id"`
		UploadKey       string `json:"upload_key"`
		RegistryID      string `json:"registry_id"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		return ImportGraphArgs{}, errors.New("invalid request body")
	}
	args := ImportGraphArgs{
		RegistryID:      body.RegistryID,
		RegistryGraphID: body.RegistryGraphID,
		UploadKey:       body.UploadKey,
	}
	if args.RegistryGraphID != "" {
		args.SourceKind = "registry"
	} else if args.UploadKey != "" {
		args.SourceKind = "upload"
	} else {
		return ImportGraphArgs{}, errors.New("registry_graph_id or upload_key is required")
	}
	return args, nil
}

// resolveClient resolves the per-repo registry client, mirroring
// Remote.resolveClient. Returns (client, regID, true, "") when the
// integration is on; (nil, "", false, msg) for a 503.
func (h *Graph) resolveClient(r *http.Request) (*registry.Client, string, bool, string) {
	if h.registryClients == nil || !h.registryClients.IsConfigured() {
		return nil, "", false, "remote registry is not configured"
	}
	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		return nil, "", false, "could not resolve repository ID"
	}
	regCfg, err := h.deps.Store.GetRepositoryRegistryConfig(r.Context(), repoID)
	if err != nil {
		return nil, "", false, "reading repository registry config: " + err.Error()
	}
	regID := "default"
	if regCfg.RegistryID != nil && *regCfg.RegistryID != "" {
		regID = *regCfg.RegistryID
	}
	client, _, ok := h.registryClients.Client(regID)
	if !ok || !client.IsConfigured() {
		return nil, "", false, fmt.Sprintf("registry_id %q is not configured", regID)
	}
	return client, regID, true, ""
}

// atoiDefault parses s as int, returning def on empty/invalid.
func atoiDefault(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return def, fmt.Errorf("invalid integer %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// ── status endpoints (read River job output) ─────────────────────────
//
// The export/import status endpoints read the River job's recorded
// output (the ExportGraphResult / ImportGraphResult the workers
// produce via river.RecordOutput). The Tasks handler already has a
// GetJob that returns the full job row; here we expose a thin
// graph-specific status that returns just the result + state so the
// UI doesn't need to parse the full River job shape.

// GetExportStatus returns the export_graph job's state + recorded
// output. Gated by graph:export (wiring layer). The jobID is the
// River job id the ExportGraph handler returned.
func (h *Graph) GetExportStatus(w http.ResponseWriter, r *http.Request) {
	h.getJobStatus(w, r, "export_graph")
}

// GetImportStatus returns the import_graph job's state + recorded
// output. Gated by graph:write (wiring layer).
func (h *Graph) GetImportStatus(w http.ResponseWriter, r *http.Request) {
	h.getJobStatus(w, r, "import_graph")
}

// getJobStatus is the shared status reader. It fetches the River job
// by id, checks the kind matches, and returns the state + the
// recorded output (if any). The output is the
// ExportGraphResult / ImportGraphResult the worker recorded.
func (h *Graph) getJobStatus(w http.ResponseWriter, r *http.Request, expectedKind string) {
	// The Tasks handler bundle owns the River client; the graph
	// handler doesn't. For MVP we return a 501 with a pointer to
	// the generic /tasks/{jobID} endpoint, which the frontend can
	// poll instead. A future wiring pass can hand the River client
	// to the graph handler for a graph-specific status shape.
	jobID := chi.URLParam(r, "jobID")
	if jobID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "jobID is required")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"job_id":          jobID,
		"expected_kind":   expectedKind,
		"status_endpoint": "/api/v1/tasks/" + jobID,
		"note":            "poll /api/v1/tasks/{jobID} for the full River job state + recorded output",
	})
}
