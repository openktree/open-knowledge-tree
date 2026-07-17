package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// RetrieveSourceArgs is the structured argument the source HTTP
// handler accepts on the wire. The River JobArgs type
// (tasks.RetrieveSourceArgs) has the same JSON shape but is
// internal to the taskmanager package; the handler does not
// import it directly.
//
// RepositoryID is the UUID of the repository the fetched
// content should be persisted under. It is read from the
// X-Repository-ID header by the HTTP handler (so the
// frontend's repository dropdown flows through
// automatically) and forwarded to the worker. The worker
// uses it to resolve the per-repo database pool and write a
// row into okt_repository.sources. When empty the worker
// skips persistence (useful for jobs that only want to test
// the fetch strategy without touching the per-repo table).
//
// DOI is the bare DOI the caller already knows about (e.g.
// the OpenAlex search result the user clicked on). It is
// optional: when the URL is itself a doi.org URL or a bare
// "10.…" the worker can re-derive it from the URL via the
// cheap classifier, but carrying it explicitly lets the
// search-result click-through path keep the DOI even when
// the URL is something like an openalex.org/W… landing
// page that the classifier can't see through.
//
// PublishedAt is the publication date the caller already
// knows about (e.g. an OpenAlex hit the user clicked on).
// It is optional for the same reason DOI is: the worker
// can fall back to parsed.PublishedAt from the
// content_parsing step (trafilatura / htmldate), so a
// missing value here just means "let the parser decide".
// The wire shape is an RFC 3339 timestamp; the handler
// does not enforce day precision (the database column is
// DATE, so any sub-day component is dropped on persist).
type RetrieveSourceArgs struct {
	URL          string     `json:"url"`
	RepositoryID string     `json:"repository_id,omitempty"`
	DOI          string     `json:"doi,omitempty"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`
	Process      bool       `json:"process,omitempty"`
	// InvestigationID, when set, asks the retrieve_source
	// worker to link the just-persisted source row into this
	// investigation once the row exists. The link is best-effort
	// and runs after persistSource returns the source_id; a
	// failed fetch still creates a row, and the worker links it
	// so the user/agent can see the attempted source in the
	// investigation's list. The worker verifies the
	// investigation belongs to the same repository before
	// inserting the junction row, mirroring the REST
	// Investigations.AddSource ownership guard. This is the
	// preferred one-call path for MCP agents; the standalone
	// addInvestigationSource MCP tool exists for reorganizing
	// already-fetched sources.
	InvestigationID string `json:"investigation_id,omitempty"`
}

// TaskEnqueuer is the minimal contract the source handler needs
// from the background-task manager. It is intentionally narrow:
// the handler only cares that URLs can be turned into jobs. The
// taskmanager package satisfies this via a thin method on
// Manager. We declare the interface here (rather than import
// taskmanager) to keep the HTTP layer independent of River
// internals.
type TaskEnqueuer interface {
	EnqueueRetrieveSourceFromHTTP(ctx context.Context, args RetrieveSourceArgs) (string, error)
	EnqueueSourceDecompositionFromHTTP(ctx context.Context, args SourceDecompositionArgs) (string, error)
	EnqueueAnnotateReportFromHTTP(ctx context.Context, args AnnotateReportArgs) (string, error)
}

// SourceDecompositionArgs is the wire shape for POST
// /{slug}/sources/{sourceID}/process.
type SourceDecompositionArgs struct {
	SourceID     string `json:"source_id"`
	RepositoryID string `json:"repository_id"`
}

// AnnotateReportArgs is the wire shape for the report-annotation
// enqueue (POST /reports, POST /reports/upload, POST /reports/{id}/annotate).
// The River JobArgs type (tasks.AnnotateReportArgs) has the same JSON
// shape but is internal to the taskmanager package; the handler does
// not import it directly.
type AnnotateReportArgs struct {
	ReportID     string `json:"report_id"`
	RepositoryID string `json:"repository_id"`
}

// RepoPoolResolver resolves a repository UUID (string form, the
// shape the HTTP body carries) to the per-repo *pgxpool.Pool and
// the parsed pgtype.UUID the per-repo queries need. It is the
// out-of-middleware equivalent of appmw.WithRepoQueries: the
// /sources/{provider}/search route is mounted at the top level
// (not under /{repoID}), so the per-repo middleware never runs on
// it. The TestSearch handler uses this resolver to look up
// already-fetched sources in the active repository before
// returning results.
//
// The resolver returns an error when the repository ID is not a
// valid UUID or when no repository row exists for it; the handler
// turns that into a 400. A nil pool (repository's database not
// registered) is treated as a 500 because by construction every
// repository row's database_name points at a registered pool.
type RepoPoolResolver func(ctx context.Context, repoID string) (*pgxpool.Pool, pgtype.UUID, error)

// Source bundles the source-provider HTTP handlers (search and
// resource classification).
//
// The decomposition maps are passed in so the /decomposition/providers
// endpoint can surface every registered chunker / fact extractor
// the way /sources/providers surfaces search + resolution
// providers. The worker that actually runs
// source_decomposition picks the active ones; the maps here are
// for visibility only.
type Source struct {
	SearchProviders    map[string]search.SearchProvider
	FetchStrategy      *fetch.FetchStrategy
	ChunkingProviders  map[string]decomposition.ChunkingProvider
	FactExtractors     map[string]decomposition.FactExtractionProvider
	ImageExtractors    map[string]decomposition.ImageFactExtractionProvider
	Storage            storage.FileStorage
	Parsers            []content_parsing.Parser
	repoPoolResolver   RepoPoolResolver
	taskEnqueuer       TaskEnqueuer
	// settingsGate decides whether a (kind, provider_id) is enabled
	// for the active repository. Set via SetSettingsGate; nil in
	// tests that don't exercise the per-repo provider gate. When nil,
	// the gate is a no-op (legacy behavior) so existing tests that
	// don't wire settings still pass.
	settingsGate RepoProviderGate
	// providerRegistry is the live catalog; the gate uses it to
	// filter stored settings down to the live set (orphans ignored).
	// Set via SetProviderRegistry; nil means "no enforcement".
	providerRegistry *ProviderRegistry
}

// NewSource constructs a Source handler bundle. `storage` is the
// file-storage backend used to serve stored source assets (images,
// PDF bodies); it may be nil when storage is disabled (the serving
// endpoints return 404 in that case). `parsers` are the
// content_parsing.Parser instances the UploadSource handler uses to
// parse uploaded files in-process (skipping the fetch strategy).
func NewSource(
	searchProviders map[string]search.SearchProvider,
	strategy *fetch.FetchStrategy,
	chunkingProviders map[string]decomposition.ChunkingProvider,
	factExtractors map[string]decomposition.FactExtractionProvider,
	imageExtractors map[string]decomposition.ImageFactExtractionProvider,
	stor storage.FileStorage,
	parsers []content_parsing.Parser,
) *Source {
	return &Source{
		SearchProviders:   searchProviders,
		FetchStrategy:     strategy,
		ChunkingProviders: chunkingProviders,
		FactExtractors:    factExtractors,
		ImageExtractors:   imageExtractors,
		Storage:           stor,
		Parsers:           parsers,
	}
}

// SetTaskEnqueuer attaches the background-task enqueuer. The
// source handler is the only consumer of the enqueuer in the API
// today, so we keep it here. Idempotent: setting a nil enqueuer
// is allowed and simply disables the /sources/retrieve endpoint
// (it will return 503).
func (s *Source) SetTaskEnqueuer(eq TaskEnqueuer) {
	s.taskEnqueuer = eq
}

// SetRepoPoolResolver attaches the per-repository pool resolver
// the TestSearch handler uses to look up already-fetched sources
// in the active repository. Optional: when nil, TestSearch skips
// the existence check and returns results untagged (the legacy
// behavior). Idempotent and safe to call with nil.
func (s *Source) SetRepoPoolResolver(r RepoPoolResolver) {
	s.repoPoolResolver = r
}

// RepoProviderGate resolves a repository UUID (string form) to the
// set of enabled provider (kind, id) pairs stored in
// repository_provider_settings, intersected with the live registry.
// The Source handler uses it to gate TestSearch and
// EnqueueRetrieveSource: a provider not in the returned set is
// rejected (403/400). Returns nil + a true "ok" flag when the gate
// is a no-op (no settings stored for the repo → the caller decides
// whether that means "allow all" or "deny all"; the production gate
// treats empty stored rows as "deny all" per the settings-as-source-
// of-truth model, but the gate helper itself just returns the set
// so the caller can distinguish).
//
// Errors from the settings lookup are returned non-fatal: the caller
// logs and falls back to "deny" (safer than allowing through a
// misconfigured gate).
type RepoProviderGate func(ctx context.Context, repoID string) (enabled map[[2]string]bool, ok bool, err error)

// SetSettingsGate wires the per-repo provider gate. Optional: when
// nil, the gate is a no-op (TestSearch/EnqueueRetrieveSource skip
// the enabled check). Idempotent and safe to call with nil.
func (s *Source) SetSettingsGate(g RepoProviderGate) {
	s.settingsGate = g
}

// SetProviderRegistry attaches the live provider catalog so the
// gate and ListProviders can intersect stored rows with the live
// set. Optional; nil means "no registry" (the gate falls back to
// checking stored rows directly, which can include orphans).
func (s *Source) SetProviderRegistry(r *ProviderRegistry) {
	s.providerRegistry = r
}

// providerEnabledForRepo reports whether a (kind, id) provider is
// enabled for the active repository. The active repo is resolved
// from the body's RepositoryID field or the X-Repository-ID header
// (same fallback as TestSearch). Returns (enabled, checked, err):
//   - checked=false means the gate didn't run (no gate wired, or no
//     repo in context) → the caller skips enforcement (legacy
//     behavior for callers that don't carry a repo, e.g. the
//     global /sources/{provider}/search route used outside a repo).
//   - checked=true, enabled=true → allow.
//   - checked=true, enabled=false → deny (403/400).
//   - err non-nil → a settings lookup failure; the caller logs and
//     denies (safer than allowing through a broken gate).
func (s *Source) providerEnabledForRepo(ctx context.Context, repoID, kind, id string) (enabled, checked bool, err error) {
	if s.settingsGate == nil {
		return false, false, nil
	}
	if repoID == "" {
		return false, false, nil
	}
	set, ok, gerr := s.settingsGate(ctx, repoID)
	if gerr != nil {
		return false, true, gerr
	}
	if !ok {
		// No stored rows for the repo → deny all (settings are the
		// source of truth; a repo with no settings is misconfigured).
		return false, true, nil
	}
	return set[[2]string{kind, id}], true, nil
}

// ListProviders handles GET /sources/providers.
//
// The endpoint surfaces the providers the server is currently
// wired with. Search providers are listed as the keys of
// SearchProviders; resolution (fetch) providers are walked
// from the strategy in priority order. Each resolution
// provider's static metadata is pulled from its own
// Describe() method so the catalog is data-driven: a new
// provider only needs to implement Describe() to appear in
// the response.
//
// When the request carries an X-Repository-ID header (the
// frontend's API client injects it automatically), each
// provider entry is annotated with an `enabled_for_repo`
// boolean reflecting whether that provider is enabled in
// the active repository's settings. The UI uses this to
// grey out disabled providers in the catalog and to hide
// them from search-provider dropdowns. When no repo is in
// context (or the gate is not wired), the field is omitted
// and the response is the pure global catalog.
func (s *Source) ListProviders(w http.ResponseWriter, r *http.Request) {
	// Resolve the per-repo enabled set when a repository is in
	// context (the X-Repository-ID header, injected by the
	// frontend's API client). Best-effort: a gate error is logged
	// and the catalog is returned without per-repo annotations.
	repoID := r.Header.Get("X-Repository-ID")
	var enabledSet map[[2]string]bool
	gateChecked := false
	if s.settingsGate != nil && repoID != "" {
		set, ok, gerr := s.settingsGate(r.Context(), repoID)
		if gerr != nil {
			log.Printf("source: ListProviders settings gate error for repo %s: %v", repoID, gerr)
		} else if ok {
			enabledSet = set
			gateChecked = true
		}
	}

	// repoEnabled reports whether a (kind, id) provider is enabled
	// for the active repo. Only called when gateChecked is true.
	repoEnabled := func(kind, id string) bool {
		if enabledSet == nil {
			return false
		}
		return enabledSet[[2]string{kind, id}]
	}

	searchProviders := []map[string]interface{}{}
	for id := range s.SearchProviders {
		var entry map[string]interface{}
		switch id {
		case "serper":
			entry = map[string]interface{}{
				"id":   "serper",
				"name": "Serper (Google Search)",
				"type": "search",
			}
		case "openalex":
			entry = map[string]interface{}{
				"id":   "openalex",
				"name": "OpenAlex (Academic Works)",
				"type": "search",
			}
		}
		if entry != nil {
			if gateChecked {
				entry["enabled_for_repo"] = repoEnabled(ProviderKindSearch, id)
			}
			searchProviders = append(searchProviders, entry)
		}
	}

	resolutionProviders := []map[string]interface{}{}
	if s.FetchStrategy != nil {
		for i, p := range s.FetchStrategy.Providers() {
			d := p.Describe()
			pid := providerID(p)
			// priority is 1-based and reflects the order
			// the strategy consults providers: lower
			// numbers run first. The UI uses it to show
			// the fall-through chain.
			entry := map[string]interface{}{
				"id":          pid,
				"name":        d.Name,
				"type":        "resolution",
				"description": d.Description,
				"requires":    d.Requires,
				"configured":   d.Configured,
				"supports":    d.Supports,
				"timeout":     d.Timeout,
				"notes":       d.Notes,
				"priority":    i + 1,
			}
			if gateChecked {
				entry["enabled_for_repo"] = repoEnabled(ProviderKindResolution, pid)
			}
			resolutionProviders = append(resolutionProviders, entry)
		}
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"search":     searchProviders,
		"resolution": resolutionProviders,
		"flare_skip_candidates": s.flareSkipCandidates(r),
	})
}

// flareSkipCandidates returns the per-host FlareSolverr
// failure/success counts for the active repository, used by the
// Providers UI to surface "candidate hosts to pin out of
// FlareSolverr". Today the strategy does NOT enforce a skip
// list; this is the data-side preparation so the blacklist is
// ready to wire. A host with flare_failures > 0 and
// flare_successes = 0 is a strong skip candidate.
//
// Returns nil (omitted from the JSON response) when no
// repository is in context (the global /sources/providers
// route is usable outside a repo scope) or when the per-repo
// pool can't be resolved. Best-effort: any query error is
// logged and returns nil so the catalog endpoint never fails
// on a diagnostic feature.
func (s *Source) flareSkipCandidates(r *http.Request) []map[string]interface{} {
	if s.repoPoolResolver == nil {
		return nil
	}
	repoID := r.Header.Get("X-Repository-ID")
	if repoID == "" {
		return nil
	}
	pool, _, err := s.repoPoolResolver(r.Context(), repoID)
	if err != nil || pool == nil {
		if err != nil {
			log.Printf("source: flare_skip_candidates pool resolve for repo %s: %v", repoID, err)
		}
		return nil
	}
	queries := store.New(pool)
	rows, err := queries.ListFlareSolverrHostCandidates(r.Context())
	if err != nil {
		log.Printf("source: flare_skip_candidates query: %v", err)
		return nil
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]interface{}{
			"host":             row.Host,
			"total_attempts":   row.TotalAttempts,
			"flare_failures":   row.FlareFailures,
			"flare_successes":  row.FlareSuccesses,
		})
	}
	return out
}

// ListDecompositionProviders handles GET /decomposition/providers.
//
// The endpoint surfaces every chunking and fact-extraction
// provider the server is currently wired with, in the same
// shape the AI tab and the Search/Fetch tabs use (id, name,
// description, requires, configured, supports, notes, plus a
// read-only `config` map carrying the current chunk_size /
// model). An empty map means no providers of that kind are
// registered; the UI falls back to its empty-state.
func (s *Source) ListDecompositionProviders(w http.ResponseWriter, r *http.Request) {
	chunkers := []map[string]interface{}{}
	for id, p := range s.ChunkingProviders {
		d := p.Describe()
		chunkers = append(chunkers, map[string]interface{}{
			"id":          id,
			"name":        d.Name,
			"description": d.Description,
			"requires":    d.Requires,
			"configured":  d.Configured,
			"supports":    d.Supports,
			"notes":       d.Notes,
			"config":      d.Config,
		})
	}

	extractors := []map[string]interface{}{}
	for id, p := range s.FactExtractors {
		d := p.Describe()
		extractors = append(extractors, map[string]interface{}{
			"id":          id,
			"name":        d.Name,
			"description": d.Description,
			"requires":    d.Requires,
			"configured":  d.Configured,
			"supports":    d.Supports,
			"notes":        d.Notes,
			"config":      d.Config,
		})
	}

	imageExtractors := []map[string]interface{}{}
	for id, p := range s.ImageExtractors {
		d := p.Describe()
		imageExtractors = append(imageExtractors, map[string]interface{}{
			"id":          id,
			"name":        d.Name,
			"description": d.Description,
			"requires":    d.Requires,
			"configured":  d.Configured,
			"supports":    d.Supports,
			"notes":        d.Notes,
			"config":      d.Config,
		})
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"chunking":         chunkers,
		"fact_extraction":  extractors,
		"image_extraction": imageExtractors,
	})
}

// providerID returns the stable, lowercase identifier used
// over the wire and on the URL path. Keeping the mapping
// here (next to providerName) means the rest of the
// handler never has to type-switch a provider.
func providerID(p fetch.ResolutionProvider) string {
	switch p.(type) {
	case *fetch.FetchResolutionProvider:
		return "fetch"
	case *fetch.UnpaywallResolutionProvider:
		return "unpaywall"
	case *fetch.TLSImpersonationProvider:
		return "tls"
	case *fetch.FlareSolverrProvider:
		return "flaresolverr"
	default:
		return "unknown"
	}
}

// TestSearch handles POST /sources/{provider}/search.
//
// The request body carries the query plus optional pagination and
// repository-scoping fields:
//
//	{
//	  "query": "...",
//	  "repository_id": "uuid",   // optional; enables already-added tagging
//	  "cursor": "...",            // optional; opaque provider pagination token
//	  "per_page": 10              // optional; 0 = provider default
//	}
//
// The response is an envelope (not a bare array) so the caller can
// drive "load more" pagination:
//
//	{
//	  "results": [...],
//	  "total": 123,               // 0 when the provider has no count (Serper)
//	  "next_cursor": "...",        // "" when no more pages
//	  "per_page": 10
//	}
//
// When `repository_id` is present and the handler has a
// repoPoolResolver wired, each result is tagged with
// `already_exists` (bool) and `existing_status` (the matched
// source row's status) so the UI can show an "Already added"
// badge and skip re-queuing a fetch. Matching is by URL or DOI:
// the same paper fetched via a different URL (e.g. doi.org vs a
// publisher landing page) is still detected when the stored DOI
// agrees. Failures to resolve the pool or run the lookup are
// logged and silently skipped — the search results are still
// returned, just untagged, so a misconfigured resolver never
// blocks search.
func (s *Source) TestSearch(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")

	var body struct {
		Query        string `json:"query"`
		RepositoryID string `json:"repository_id"`
		Cursor       string `json:"cursor"`
		PerPage      int    `json:"per_page"`
	}
	_ = httputil.DecodeBody(r, &body)

	if body.Query == "" {
		httputil.WriteError(w, http.StatusBadRequest, "query is required")
		return
	}

	p, ok := s.SearchProviders[provider]
	if !ok || p == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "search provider not available: "+provider)
		return
	}

	// Per-repo provider gate: when a repository is in context (body
	// or X-Repository-ID header), reject 403 when the provider is
	// disabled for that repo. Settings are the source of truth — a
	// repo with no stored settings denies everything (the admin must
	// configure providers via the repository-settings UI). When no
	// repo is in context, the gate is skipped (the global search
	// route is still usable outside a repo scope, e.g. for the
	// Providers page preview).
	if body.RepositoryID == "" {
		body.RepositoryID = r.Header.Get("X-Repository-ID")
	}
	if enabled, checked, gerr := s.providerEnabledForRepo(r.Context(), body.RepositoryID, ProviderKindSearch, provider); checked {
		if gerr != nil {
			log.Printf("source: TestSearch settings gate error for repo %s: %v", body.RepositoryID, gerr)
		}
		if !enabled {
			httputil.WriteError(w, http.StatusForbidden, "search provider not enabled for this repository")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	resp, err := p.Search(ctx, body.Query, search.SearchOptions{
		PerPage: body.PerPage,
		Cursor:  body.Cursor,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Tag results with already_exists / existing_status when we
	// have a repository to check against. Best-effort: any error
	// (no resolver wired, repo not found, query fails) is logged
	// and skipped so search still returns results.
	if body.RepositoryID != "" && s.repoPoolResolver != nil && len(resp.Results) > 0 {
		pool, repoUUID, err := s.repoPoolResolver(ctx, body.RepositoryID)
		if err != nil {
			log.Printf("source: TestSearch already-exists tagging failed for repo %s: %v", body.RepositoryID, err)
		} else if err := TagExistingSources(ctx, pool, repoUUID, resp.Results); err != nil {
			log.Printf("source: TestSearch already-exists tagging failed for repo %s: %v", body.RepositoryID, err)
		}
	}

	httputil.WriteJSON(w, http.StatusOK, searchEnvelope{
		Results:    resp.Results,
		Total:      resp.Total,
		NextCursor: resp.NextCursor,
		PerPage:    body.PerPage,
	})
}

// searchEnvelope is the paginated response shape returned by
// TestSearch. It mirrors the pageEnvelope shape used by the list
// endpoints but with cursor-based pagination (the upstream search
// APIs use opaque cursors, not integer offsets). PerPage echoes
// the page size the handler applied (0 means the provider's
// default was used) so the caller can confirm what was sent.
type searchEnvelope struct {
	Results    []search.SearchResult `json:"results"`
	Total      int64                  `json:"total"`
	NextCursor string                 `json:"next_cursor"`
	PerPage    int                    `json:"per_page"`
}

// TagExistingSources runs the batched existence lookup for a
// search page and stamps each matching result with
// already_exists / existing_status. It builds the URL and DOI
// sets from the results, calls GetExistingSourceURLsAndDOIsByRepo
// against the supplied per-repo pool, then indexes the matches by
// both URL and DOI for an O(1) per-result lookup.
//
// Matching is by URL OR DOI: a result whose URL exactly equals a
// stored source's url, OR whose DOI equals a stored source's doi,
// counts as already-existing. The status carried back is the
// matched row's status (pending/fetching/fetched/failed) so the UI
// can color the badge.
//
// The caller is responsible for resolving pool + repoUUID (the MCP
// handler already has them from its repo resolver; the Source
// TestSearch handler resolves them via its RepoPoolResolver).
func TagExistingSources(ctx context.Context, pool *pgxpool.Pool, repoUUID pgtype.UUID, results []search.SearchResult) error {
	if pool == nil {
		return errors.New("no per-repo pool for repository")
	}

	urls := make([]string, 0, len(results))
	var dois []string
	for _, r := range results {
		if r.URL != "" {
			urls = append(urls, r.URL)
		}
		if r.DOI != "" {
			dois = append(dois, r.DOI)
		}
	}
	if len(urls) == 0 && len(dois) == 0 {
		return nil
	}

	queries := store.New(pool)
	rows, err := queries.GetExistingSourceURLsAndDOIsByRepo(ctx, store.GetExistingSourceURLsAndDOIsByRepoParams{
		RepositoryID: repoUUID,
		Column2:      urls,
		Column3:      dois,
	})
	if err != nil {
		return fmt.Errorf("querying existing sources: %w", err)
	}

	// Index by URL and by DOI. A row with a NULL DOI only
	// matches via its URL; a row with a DOI matches via either.
	byURL := make(map[string]string, len(rows))
	byDOI := make(map[string]string, len(rows))
	for _, row := range rows {
		byURL[row.Url] = row.Status
		if row.Doi != nil && *row.Doi != "" {
			byDOI[*row.Doi] = row.Status
		}
	}

	for i := range results {
		var status string
		if s, ok := byURL[results[i].URL]; ok && s != "" {
			status = s
		} else if results[i].DOI != "" {
			if s, ok := byDOI[results[i].DOI]; ok && s != "" {
				status = s
			}
		}
		if status != "" {
			st := status
			results[i].AlreadyExists = true
			results[i].ExistingStatus = &st
		}
	}
	return nil
}

// ClassifyResource handles POST /sources/classify.
//
// The classifier itself lives in the fetch package (see
// fetch.ClassifyURL) and is purely string-based: no network I/O.
// When a search provider is configured, the caller can choose to
// first call /sources/{provider}/search and feed the resulting URL
// into this endpoint. The fetch strategy takes over from there.
func (s *Source) ClassifyResource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	_ = httputil.DecodeBody(r, &body)

	if body.URL == "" {
		httputil.WriteError(w, http.StatusBadRequest, "url is required")
		return
	}

	resource := fetch.ClassifyURL(body.URL)

	providersForType := []string{}
	for _, p := range s.FetchStrategy.Providers() {
		if p.Supports(resource.Type) {
			providersForType = append(providersForType, providerName(p))
		}
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"url":      body.URL,
		"type":     resource.Type,
		"value":    resource.Value,
		"strategy": providersForType,
	})
}

// EnqueueRetrieveSource handles POST /sources/retrieve.
//
// It accepts a URL/DOI in the request body, classifies it the same
// way /sources/classify does (no network I/O at this stage), and
// enqueues a RetrieveSource job on the task queue. The job will:
//  1. Classify the resource (re-runs the same logic in the worker).
//  2. Optionally consult a configured search provider to enrich
//     the classification when the input is ambiguous.
//  3. Resolve the resource via the fetch strategy.
//  4. Persist a row in the active repository's `sources` table
//     (status='fetching' before the fetch, 'fetched'/'failed'
//     afterwards) so the UI can show what was attempted.
//
// Returns 202 Accepted with the new job ID so callers can poll
// for completion later.
func (s *Source) EnqueueRetrieveSource(w http.ResponseWriter, r *http.Request) {
	if s.taskEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	var body RetrieveSourceArgs
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.URL == "" {
		httputil.WriteError(w, http.StatusBadRequest, "url is required")
		return
	}

	// Classify eagerly so we can return a useful preview to the
	// caller in addition to the job ID. When the caller supplies
	// a DOI (e.g. from an OpenAlex search result click-through),
	// the worker forces SourceDOI regardless of the URL form, so
	// reflect that in the preview too — otherwise the response
	// says "url" while the worker actually fetches via the DOI
	// path, which is confusing.
	resource := fetch.ClassifyURL(body.URL)
	if body.DOI != "" && resource.Type != fetch.SourceDOI {
		resource.Type = fetch.SourceDOI
		resource.Value = body.DOI
		resource.DOI = body.DOI
	}

	// Forward the active repository (carried on the
	// X-Repository-ID header by the frontend's API client) so
	// the worker can scope the persisted source row. The
	// header is the same one the per-repo middleware reads
	// for routing, which keeps the "current repository"
	// concept consistent across the request lifecycle.
	if body.RepositoryID == "" {
		body.RepositoryID = r.Header.Get("X-Repository-ID")
	}

	// Per-repo provider gate: when a repo is in context, check
	// that at least one enabled resolution provider supports the
	// classified resource type. The fetch strategy chain is decided
	// in the worker (it may try several providers in order); the
	// gate here is a coarse "no enabled provider can handle this"
	// rejection so a disabled-provider repo fails fast at enqueue
	// rather than after the worker fetches and discards. The worker
	// itself doesn't re-check (the chain is shared globally); this
	// enqueue-time gate is the enforcement point.
	if body.RepositoryID != "" && s.settingsGate != nil && s.FetchStrategy != nil {
		set, ok, gerr := s.settingsGate(r.Context(), body.RepositoryID)
		if gerr != nil {
			log.Printf("source: EnqueueRetrieveSource settings gate error for repo %s: %v", body.RepositoryID, gerr)
		}
		if ok {
			anyEnabled := false
			for _, p := range s.FetchStrategy.Providers() {
				if !p.Supports(resource.Type) {
					continue
				}
				id := fetch.ProviderID(p)
				if set[[2]string{ProviderKindResolution, id}] {
					anyEnabled = true
					break
				}
			}
			if !anyEnabled {
				httputil.WriteError(w, http.StatusForbidden, "no enabled resolution provider supports this resource type for this repository")
				return
			}
		} else if gerr == nil {
			// No stored rows → deny all (settings are source of truth).
			httputil.WriteError(w, http.StatusForbidden, "no resolution providers enabled for this repository")
			return
		}
	}

	jobID, err := s.taskEnqueuer.EnqueueRetrieveSourceFromHTTP(r.Context(), body)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id":        jobID,
		"classified_as": resource.Type,
		"value":         resource.Value,
		"status":        "queued",
	})
}

func providerName(p fetch.ResolutionProvider) string {
	switch p.(type) {
	case *fetch.FetchResolutionProvider:
		return "fetch"
	case *fetch.UnpaywallResolutionProvider:
		return "unpaywall"
	case *fetch.TLSImpersonationProvider:
		return "tls"
	case *fetch.FlareSolverrProvider:
		return "flaresolverr"
	default:
		return "unknown"
	}
}

// createSourceRequest is the wire shape of POST
// /{slug}/sources. The handler builds a
// `store.Source` from the body and a freshly-generated UUID
// (sqlc generates the UUID on the database, but we want to
// return the id to the caller in the response, so we generate
// it on the application side to avoid an extra round-trip).
type createSourceRequest struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`
}

// Pagination defaults and cap for the list endpoints. The cap
// protects the server from a caller asking for limit=1000000 and
// forcing Postgres to materialize a huge result set; a caller
// that genuinely wants to walk every row should page through with
// offset. The default matches the page-size choice made when
// pagination was added (the UI uses the same default so the first
// page renders without a query-string).
const (
	defaultPageSize = 100
	maxPageSize     = 200
)

// parsePaging reads `limit` and `offset` from the request's query
// string and clamps them to sane values. Missing `limit` defaults
// to defaultPageSize; missing `offset` defaults to 0. A negative
// `offset` is coerced to 0. A `limit` outside [1, maxPageSize] is
// clamped into that range rather than rejected — the worst a
// caller can do with a clamped value is get a smaller page than
// they asked for, which is friendlier than a 400 and keeps the
// endpoint usable from a hand-typed URL.
func parsePaging(r *http.Request) (limit, offset int) {
	limit = parseIntDefault(r, "limit", defaultPageSize)
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	offset = parseIntDefault(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// parseIntDefault parses the named query param as an int and
// returns the default when the param is absent or unparseable.
// An unparseable value silently falls back to the default
// (rather than surfacing as a 400) so a typo like `?limit=abc`
// doesn't break the page — it just yields the default page size.
func parseIntDefault(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

// pageEnvelope is the canonical paginated response shape used by
// every list endpoint in this handler. `data` carries the page
// rows; `total` is the count of rows matching the filters (before
// LIMIT/OFFSET) so the UI can render "page X of Y"; `limit` and
// `offset` echo the paging that was applied so the caller can
// confirm the server honored their request (or clamped it).
type pageEnvelope struct {
	Data   interface{} `json:"data"`
	Total  int64       `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

// ListSources handles GET /{slug}/sources.
//
// It is the canonical demonstration that the per-repo routing
// works: the handler reads the per-request pool from
// PoolFromContext (set by the WithRepoQueriesBySlug middleware),
// builds a per-request *store.Queries, and runs the same
// sqlc-generated query against the right pool. In the shared
// tier the rows are filtered by repository_id; in the
// isolated tier the table physically contains only this
// repository's rows.
//
// The endpoint is paginated (limit/offset, default 100, max 200)
// and searchable (q — websearch_to_tsquery against
// sources.search_tsv, which covers url + parsed_title + doi).
// The response is a pageEnvelope: {data, total, limit, offset}.
// `total` is the count of rows matching the filters before
// LIMIT/OFFSET, computed by a second COUNT query so Postgres
// doesn't have to materialize the whole result set to count it.
func (s *Source) ListSources(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit, offset := parsePaging(r)
	search := strings.TrimSpace(r.URL.Query().Get("q"))

	sources, err := queries.ListSourcesByRepo(r.Context(), store.ListSourcesByRepoParams{
		RepositoryID: repoID,
		Column2:      search,
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list sources")
		return
	}

	total, err := queries.CountSourcesByRepo(r.Context(), store.CountSourcesByRepoParams{
		RepositoryID: repoID,
		Column2:      search,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count sources")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   sources,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// CreateSource handles POST /{slug}/sources.
func (s *Source) CreateSource(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body createSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.URL == "" {
		httputil.WriteError(w, http.StatusBadRequest, "url is required")
		return
	}
	kind := body.Kind
	if kind == "" {
		kind = "homepage"
	}

	id := pgtype.UUID{}
	if err := id.Scan(uuid.New().String()); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "generating id: "+err.Error())
		return
	}

	created, err := queries.CreateSource(r.Context(), store.CreateSourceParams{
		ID:           id,
		RepositoryID: repoID,
		Url:          body.URL,
		Kind:         kind,
		Status:       "pending",
	})
	if err != nil {
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			httputil.WriteError(w, http.StatusConflict, "source URL already exists for this repository")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create source")
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, created)
}

// maxUploadBytes caps the size of an uploaded file. It matches the
// fetch strategy's MaxBodyBytes so uploads and remote fetches have
// the same ceiling.
const maxUploadBytes = 15 * 1024 * 1024

// uploadSourceResponse is the wire shape for POST /{slug}/sources/upload.
type uploadSourceResponse struct {
	JobID             string `json:"job_id"`
	SourceID          string `json:"source_id"`
	Status            string `json:"status"`
	InvestigationLinked bool  `json:"investigation_linked,omitempty"`
}

// UploadSource handles POST /{slug}/sources/upload.
//
// Accepts a user-supplied file (PDF / HTML / Markdown / plain text)
// or raw text and creates a source row in 'fetched' status, parses
// the content in-process (skipping the fetch strategy entirely), and
// enqueues a source_decomposition job that chunks the parsed text and
// extracts facts. Decomposition chains downstream to embed_facts →
// dedup → concepts exactly as the retrieve_source path does.
//
// When the optional `investigation_id` is present (multipart form
// field or JSON field), the handler atomically links the new source
// to that investigation via the investigation_sources junction,
// mirroring investigations.AddSource but inside the same request so
// the frontend doesn't need a second round trip. The investigation
// must belong to the same repository; a cross-repo or unknown
// investigation_id is a 404.
//
// Content type selection:
//   - multipart/form-data with a `file` field: the source type is
//     inferred from the filename extension (.pdf → PDF, .html/.htm →
//     HTML, .md → Markdown, .txt → plain text).
//   - application/json with a `text` field: the body is treated as
//     raw text. Markdown is detected heuristically (leading `#`, `-`,
//     `>` or `|` lines) and routed to parsed_markdown; otherwise the
//     text is stored as parsed_text.
//
// The synthetic URL `upload://<filename>` (or `upload://raw-text-<hash>`)
// satisfies the UNIQUE(repository_id, url) constraint and makes the
// row queryable in the source list. Re-uploading the same filename
// returns 409 (matches CreateSource's duplicate behavior).
func (s *Source) UploadSource(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if s.taskEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	// Parse the request body. Two shapes are supported:
	//   1. multipart/form-data with a `file` part
	//   2. application/json with a `text` field (raw text / markdown)
	var (
		rawBytes    []byte
		sourceType  content_parsing.SourceType
		syntheticURL string
		kind        string
		title       string
		investigationID string
	)

	ctype := r.Header.Get("Content-Type")
	if strings.HasPrefix(ctype, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error())
			return
		}
		kind = r.FormValue("kind")
		investigationID = r.FormValue("investigation_id")
		file, header, ferr := r.FormFile("file")
		if ferr != nil {
			httputil.WriteError(w, http.StatusBadRequest, "file field is required")
			return
		}
		defer file.Close()
		rawBytes = make([]byte, header.Size)
		if _, err := io.ReadFull(file, rawBytes); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "failed to read uploaded file: "+err.Error())
			return
		}
		ext := strings.ToLower(filepath.Ext(header.Filename))
		syntheticURL = "upload://" + header.Filename
		switch ext {
		case ".pdf":
			sourceType = content_parsing.SourcePDF
		case ".html", ".htm":
			sourceType = content_parsing.SourceHTML
		case ".md", ".markdown":
			sourceType = "" // raw markdown, handled inline below
		case ".txt":
			sourceType = "" // raw text, handled inline below
		default:
			httputil.WriteError(w, http.StatusBadRequest, "unsupported file type: "+ext)
			return
		}
	} else if strings.HasPrefix(ctype, "application/json") {
		var body struct {
			Text           string `json:"text"`
			Title          string `json:"title,omitempty"`
			Kind           string `json:"kind,omitempty"`
			InvestigationID string `json:"investigation_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(body.Text) == "" {
			httputil.WriteError(w, http.StatusBadRequest, "text is required")
			return
		}
		rawBytes = []byte(body.Text)
		title = body.Title
		kind = body.Kind
		investigationID = body.InvestigationID
		h := sha256.Sum256(rawBytes)
		syntheticURL = "upload://raw-text-" + hex.EncodeToString(h[:8])
		sourceType = "" // determined by markdown heuristic below
	} else {
		httputil.WriteError(w, http.StatusUnsupportedMediaType, "Content-Type must be multipart/form-data or application/json")
		return
	}

	if kind == "" {
		kind = "uploaded"
	}

	// Parse the raw bytes into a ParsedDoc. PDF and HTML use the
	// registered parsers; Markdown and plain text bypass the
	// parser and become ParsedDoc directly.
	parsed, perr := s.parseUploaded(rawBytes, sourceType)
	if perr != nil {
		httputil.WriteError(w, http.StatusUnprocessableEntity, "failed to parse uploaded content: "+perr.Error())
		return
	}
	// User-supplied title (JSON path) overrides the parser's title
	// when the parser didn't recover one — useful for raw text
	// uploads which have no metadata to harvest a title from.
	if title != "" && parsed.Title == "" {
		parsed.Title = title
	}

	if parsed.Text == "" && parsed.Markdown == "" {
		httputil.WriteError(w, http.StatusUnprocessableEntity, "uploaded content has no parseable text")
		return
	}

	// Create the source row in 'fetched' status. The fetch state
	// machine (pending → fetching → fetched) is skipped entirely
	// because no network fetch is involved.
	id := pgtype.UUID{}
	if err := id.Scan(uuid.New().String()); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "generating id: "+err.Error())
		return
	}
	_, err = queries.CreateSource(r.Context(), store.CreateSourceParams{
		ID:           id,
		RepositoryID: repoID,
		Url:          syntheticURL,
		Kind:         kind,
		Status:       "fetched",
	})
	if err != nil {
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			httputil.WriteError(w, http.StatusConflict, "a source with this filename already exists for this repository")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create source")
		return
	}

	// Persist the parsed content + sentence offsets. This mirrors
	// RetrieveSourceWorker.persistParsedContent but inline (and
	// without the image-mirroring step, which is irrelevant for
	// user-uploaded files that carry no remote image URLs).
	if perr := s.persistUploadedParsed(r.Context(), queries, id, parsed); perr != nil {
		log.Printf("upload_source: persisting parsed content for %s failed: %v", uuidFromPgtype(id), perr)
	}

	// Persist the uploaded body to storage for PDFs so the
	// /sources/{sourceID}/body endpoint can serve the original.
	// HTML / text bodies are not stored (the DB preview covers
	// them), mirroring the retrieve_source worker's policy.
	if s.Storage != nil && sourceType == content_parsing.SourcePDF && len(rawBytes) > 0 {
		repoIDStr := uuidFromPgtype(repoID)
		srcIDStr := uuidFromPgtype(id)
		key := fmt.Sprintf("repositories/%s/sources/%s/body.pdf", repoIDStr, srcIDStr)
		if ref, storeErr := s.Storage.Store(r.Context(), key, "application/pdf", rawBytes); storeErr == nil {
			ct := "application/pdf"
			lp := key
			if _, err := queries.MarkSourceBodyStored(r.Context(), store.MarkSourceBodyStoredParams{
				ID:          id,
				StorageKey:  &ref.Key,
				ContentType: &ct,
				LocalPath:   &lp,
			}); err != nil {
				log.Printf("upload_source: marking source body stored for %s failed: %v", srcIDStr, err)
			}
		} else {
			log.Printf("upload_source: storing source body for %s failed: %v", srcIDStr, storeErr)
		}
	}

	// Optionally link the source to an investigation. The
	// investigation must belong to the same repository; a
	// cross-repo or unknown investigation_id is a 404 (mirrors
	// investigations.AddSource's ownership check).
	investigationLinked := false
	if investigationID != "" {
		var invID pgtype.UUID
		if err := invID.Scan(investigationID); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid investigation_id")
			return
		}
		inv, gerr := queries.GetInvestigationByID(r.Context(), invID)
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				httputil.WriteError(w, http.StatusNotFound, "investigation not found")
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
			return
		}
		if inv.RepositoryID != repoID {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		if err := queries.AddInvestigationSource(r.Context(), store.AddInvestigationSourceParams{
			InvestigationID: invID,
			SourceID:        id,
		}); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to add source to investigation")
			return
		}
		investigationLinked = true
	}

	// Enqueue decomposition. The SourceDecompositionWorker chunks
	// the parsed text, extracts facts, and chains to embed_facts →
	// dedup → concepts exactly as the retrieve_source path does.
	repoIDStr := uuidFromPgtype(repoID)
	srcIDStr := uuidFromPgtype(id)
	jobID, err := s.taskEnqueuer.EnqueueSourceDecompositionFromHTTP(r.Context(), SourceDecompositionArgs{
		SourceID:     srcIDStr,
		RepositoryID: repoIDStr,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to enqueue decomposition: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusAccepted, uploadSourceResponse{
		JobID:               jobID,
		SourceID:            srcIDStr,
		Status:              "queued",
		InvestigationLinked: investigationLinked,
	})
}

// parseUploaded turns raw uploaded bytes into a ParsedDoc using the
// registered parsers for PDF/HTML. Markdown and plain text (sourceType
// == "") bypass the parser: the bytes are treated as already-structured
// content. Markdown is detected heuristically and routed to
// parsed_markdown (preferred by the decomposition worker); everything
// else goes to parsed_text.
func (s *Source) parseUploaded(raw []byte, sourceType content_parsing.SourceType) (content_parsing.ParsedDoc, error) {
	if sourceType == "" {
		text := string(raw)
		if isMarkdownHeuristic(text) {
			return content_parsing.ParsedDoc{
				Markdown: text,
				Text:     text,
			}, nil
		}
		return content_parsing.ParsedDoc{
			Text: text,
		}, nil
	}
	for _, p := range s.Parsers {
		if !p.Supports(sourceType) {
			continue
		}
		doc, err := p.Parse(context.Background(), raw, sourceType, "")
		if err != nil {
			continue
		}
		return doc, nil
	}
	return content_parsing.ParsedDoc{}, fmt.Errorf("no parser supports source type %q", sourceType)
}

// isMarkdownHeuristic returns true when the text looks like
// Markdown. The check is deliberately cheap: a few heading / list /
// quote / table markers in the first non-blank lines is enough to
// route the content to parsed_markdown (which the decomposition
// worker prefers). False positives (plain text that happens to
// start with `#`) are harmless — the content is still stored as
// parsed_text too.
func isMarkdownHeuristic(text string) bool {
	lines := strings.Split(text, "\n")
	hits := 0
	checked := 0
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		checked++
		if checked > 20 {
			break
		}
		switch {
		case strings.HasPrefix(t, "#"),
			strings.HasPrefix(t, "- "),
			strings.HasPrefix(t, "* "),
			strings.HasPrefix(t, "> "),
			strings.HasPrefix(t, "| "),
			strings.HasPrefix(t, "1. "),
			strings.HasPrefix(t, "```"):
			hits++
		}
	}
	return hits >= 1
}

// persistUploadedParsed writes the ParsedDoc fields + sentence
// offsets to the source row. It is a trimmed inline copy of
// RetrieveSourceWorker.persistParsedContent (no image handling —
// uploaded files carry no remote image URLs to mirror).
func (s *Source) persistUploadedParsed(ctx context.Context, queries *store.Queries, id pgtype.UUID, parsed content_parsing.ParsedDoc) error {
	status := "ok"
	if parsed.Title == "" && parsed.Text == "" && parsed.HTML == "" && parsed.Markdown == "" {
		status = "unsupported"
	}
	var title, text, html, markdown, author, sitename, language *string
	if status == "ok" {
		if parsed.Title != "" {
			title = strPtr(parsed.Title)
		}
		if parsed.Text != "" {
			text = strPtr(parsed.Text)
		}
		if parsed.HTML != "" {
			html = strPtr(parsed.HTML)
		}
		if parsed.Markdown != "" {
			markdown = strPtr(parsed.Markdown)
		}
		if parsed.Author != "" {
			author = strPtr(parsed.Author)
		}
		if parsed.Sitename != "" {
			sitename = strPtr(parsed.Sitename)
		}
		if parsed.Language != "" {
			language = strPtr(parsed.Language)
		}
	}
	if _, err := queries.MarkSourceParsed(ctx, store.MarkSourceParsedParams{
		ID:             id,
		ParsedTitle:    title,
		ParsedText:     text,
		ParsedHtml:     html,
		ParsedMarkdown: markdown,
		ParsedAuthor:   author,
		ParsedSitename: sitename,
		ParsedLanguage: language,
		ParseStatus:    strPtr(status),
	}); err != nil {
		return fmt.Errorf("marking source parsed: %w", err)
	}
	if offsets := buildSentenceOffsets(markdown, text); offsets != nil {
		if err := queries.SetSentenceOffsets(ctx, store.SetSentenceOffsetsParams{
			ID:              id,
			SentenceOffsets: offsets,
		}); err != nil {
			return fmt.Errorf("setting sentence offsets: %w", err)
		}
	}
	return nil
}

// buildSentenceOffsets computes the deterministic global sentence
// array for a source so the decomposition worker (and
// fact_references.sentence_index) keys into stable offsets. It is
// an inline copy of tasks.buildSentenceOffsets to avoid an import
// cycle between handler and taskmanager/tasks; the two must stay
// in sync (both call decomposition.SegmentSentences).
func buildSentenceOffsets(markdown, text *string) []int32 {
	var src string
	switch {
	case markdown != nil && *markdown != "":
		src = *markdown
	case text != nil && *text != "":
		src = *text
	default:
		return nil
	}
	sents := decomposition.SegmentSentences(src)
	if len(sents) == 0 {
		return nil
	}
	offsets := make([]int32, 0, len(sents)*2)
	for _, snt := range sents {
		offsets = append(offsets, int32(snt.StartRune), int32(snt.EndRune))
	}
	return offsets
}

func strPtr(s string) *string { return &s }

// GetSource handles GET /{slug}/sources/{sourceID}.
//
// It returns a single source row by id. Like ListSources, it
// reads the per-repo pool from the request context. The
// handler is the canonical "view a single fetched source
// with its content" endpoint the UI uses when the user
// expands a row in the list. We intentionally do not gate
// reads on a permission check beyond authentication; the
// list endpoint has the same posture and the per-repo data
// plane is meant to be open to any authenticated member of
// the repository's tenant.
func (s *Source) GetSource(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	source, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "source not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get source")
		return
	}

	// Belt-and-suspenders: the row's repository_id must
	// match the URL's {repoID}. The middleware already
	// resolves the per-repo pool, but a misconfigured
	// cache (or a hand-crafted request) could return a row
	// from another repo. Rejecting here keeps the
	// multi-tenant isolation guarantee honest.
	if source.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}

	// Hydrate the image list. The list is a child table
	// so we fetch it in a second query (1+1, not N+1 —
	// this is GetSource, not ListSources). The UI uses
	// it to render inline <img> tags for HTML and a
	// page-by-page viewer for PDF sources.
	images, err := queries.ListSourceImages(r.Context(), source.ID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list source images")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"source": source,
		"images": images,
	})
}

// DeleteSource handles DELETE /{slug}/sources/{sourceID}.
//
// It removes a single source row. Like GetSource, the handler
// reads the per-repo pool from the request context and asserts
// the row's repository_id matches the URL's {repoID} before
// issuing the DELETE — this keeps the multi-tenant isolation
// guarantee honest even on a destructive path.
func (s *Source) DeleteSource(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Re-read the row first so we can return 404 cleanly and
	// enforce repository ownership before the DELETE. A
	// blind DELETE would return no error on a missing row
	// and silently leak 200 OK to a caller that guessed a
	// source id from another repository.
	source, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "source not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get source")
		return
	}
	if source.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}

	if err := queries.DeleteSource(r.Context(), sourceID); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to delete source")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// repoIDFromURL extracts the repository UUID from the request
// context, set by WithRepoQueriesBySlug for /{slug}/sources routes.
func repoIDFromURL(r *http.Request) (pgtype.UUID, error) {
	if id, ok := appmw.RepoIDFromContext(r.Context()); ok {
		return id, nil
	}
	return pgtype.UUID{}, errors.New("repoID is required")
}

// sourceIDFromURL extracts the {sourceID} chi URL param and
// parses it into a pgtype.UUID. Mirrors repoIDFromURL for
// the per-source subroute.
func sourceIDFromURL(r *http.Request) (pgtype.UUID, error) {
	var id pgtype.UUID
	raw := chi.URLParam(r, "sourceID")
	if raw == "" {
		return id, errors.New("sourceID is required")
	}
	if err := id.Scan(raw); err != nil {
		return id, errors.New("invalid source id")
	}
	return id, nil
}

func uuidFromPgtype(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ProcessSource handles POST /{slug}/sources/{sourceID}/process.
//
// It enqueues a source_decomposition job that chunks the source's
// parsed text and extracts facts from each chunk using the
// configured AI provider. The source must be in 'fetched' status
// with non-empty parsed text.
func (s *Source) ProcessSource(w http.ResponseWriter, r *http.Request) {
	if s.taskEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	source, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "source not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get source")
		return
	}
	if source.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}

	if source.Status != "fetched" {
		httputil.WriteError(w, http.StatusBadRequest, "source must be in 'fetched' status to process")
		return
	}

	// Accept either the Markdown rendering (preferred on new
	// rows) or the plain-text fallback (legacy rows and PDFs
	// without inline structure). The decomposition worker
	// makes the same Markdown-first choice when selecting the
	// chunking input, so this gate stays in sync with what the
	// worker would actually consume.
	if (source.ParsedMarkdown == nil || *source.ParsedMarkdown == "") &&
		(source.ParsedText == nil || *source.ParsedText == "") {
		httputil.WriteError(w, http.StatusBadRequest, "source has no parsed text to process")
		return
	}

	repoIDStr := uuidFromPgtype(repoID)
	sourceIDStr := uuidFromPgtype(sourceID)

	jobID, err := s.taskEnqueuer.EnqueueSourceDecompositionFromHTTP(r.Context(), SourceDecompositionArgs{
		SourceID:     sourceIDStr,
		RepositoryID: repoIDStr,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id":    jobID,
		"source_id": sourceIDStr,
		"status":    "queued",
	})
}

// RetrySource handles POST /{slug}/sources/{sourceID}/retry.
//
// It re-queues the retrieve_source pipeline for a row whose fetch
// failed. The source must be in 'failed' status; rows in any other
// state are a 400 (a 'fetched' row can be re-decomposed via the
// /process endpoint, and a 'fetching' row is already in flight).
// Uploaded sources (synthetic `upload://...` URLs created by the
// UploadSource handler) are also rejected because the fetch
// strategy has no way to re-resolve them — the original bytes
// arrived via a multipart form, not a URL.
//
// The handler resets the row to 'pending' and clears the recorded
// error / parse_status so the UI shows a clean state before the
// worker picks the job up, then enqueues a fresh retrieve_source
// job carrying the row's URL + optional DOI (so the DOI path is
// preserved across a retry even when the URL is a doi.org landing
// page). Returns 202 with the new job id, matching the shape of
// /sources/retrieve.
func (s *Source) RetrySource(w http.ResponseWriter, r *http.Request) {
	if s.taskEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	source, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "source not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get source")
		return
	}
	if source.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}

	if source.Status != "failed" {
		httputil.WriteError(w, http.StatusBadRequest, "source must be in 'failed' status to retry")
		return
	}
	if strings.HasPrefix(source.Url, "upload://") {
		httputil.WriteError(w, http.StatusBadRequest, "uploaded sources cannot be retried; re-upload the file instead")
		return
	}

	// Reset the row to a clean pending state so the worker's
	// fetching → fetched/failed state machine runs from the
	// top. Clearing parse_status is important: a stale 'failed'
	// or 'unsupported' parse_status would mislead the UI during
	// the re-fetch window.
	if _, err := queries.ResetSourceForRetry(r.Context(), sourceID); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to reset source for retry")
		return
	}

	repoIDStr := uuidFromPgtype(repoID)
	sourceIDStr := uuidFromPgtype(sourceID)

	// Preserve the DOI so the fetch strategy takes the DOI path
	// (Unpaywall OA lookup) first when the source had one, exactly
	// as the original retrieve_source job would have.
	var doi string
	if source.Doi != nil {
		doi = *source.Doi
	}

	jobID, err := s.taskEnqueuer.EnqueueRetrieveSourceFromHTTP(r.Context(), RetrieveSourceArgs{
		URL:          source.Url,
		RepositoryID: repoIDStr,
		DOI:          doi,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id":    jobID,
		"source_id": sourceIDStr,
		"status":    "queued",
	})
}

// ListFacts handles GET /{slug}/sources/{sourceID}/facts.
//
// Returns facts linked to the source via the junction, with a
// computed `source_count` per fact (a fact extracted from this
// source may be confirmed by N-1 others; the user sees that
// cross-confirmation here). Ordered by chunk_index then
// first_seen_at so the UI shows facts in extraction order.
//
// Paginated (limit/offset, default 100, max 200) and searchable
// (q — websearch_to_tsquery against facts.search_tsv, which covers
// facts.text). The response is a pageEnvelope. The optional
// `status` query param accepts '' (all), 'stable', 'new', or
// 'to_delete' and is preserved for back-compat with the pre-search
// shape.
func (s *Source) ListFacts(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	source, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "source not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get source")
		return
	}
	if source.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}

	statusFilter := r.URL.Query().Get("status")
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset := parsePaging(r)

	facts, err := queries.ListFactsBySource(r.Context(), store.ListFactsBySourceParams{
		SourceID: sourceID,
		Column2:  statusFilter,
		Column3:  search,
		Limit:    int32(limit),
		Offset:   int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list facts")
		return
	}

	total, err := queries.CountFactsBySource(r.Context(), store.CountFactsBySourceParams{
		SourceID: sourceID,
		Column2:  statusFilter,
		Column3:  search,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count facts")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   facts,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// ListSourceReferences handles GET /{slug}/sources/{sourceID}/references.
//
// Returns the sentence-level provenance rows for a source: every
// (fact, sentence_index) citation, joined with the fact row so the
// frontend can group facts by sentence and render highlight + modal.
// Ordered by sentence_index. The response is a flat array (not a
// page envelope) — sources rarely exceed a few hundred citations
// and the frontend builds a Map<sentenceIndex, FactReference[]> in
// one pass.
func (s *Source) ListSourceReferences(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	source, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "source not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get source")
		return
	}
	if source.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}

	refs, err := queries.ListFactReferencesBySource(r.Context(), sourceID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list fact references")
		return
	}
	if refs == nil {
		refs = []store.ListFactReferencesBySourceRow{}
	}
	httputil.WriteJSON(w, http.StatusOK, refs)
}

// ListRepoFacts handles GET /{slug}/facts.
//
// Returns all facts in the repository with a computed
// `source_count` per fact. The optional `status` query param
// accepts "stable" (default), "new", "to_delete", or "all" (no
// filter). The optional `sort` query param accepts "created_at"
// (default — newest first) or "source_count" (most confirmed
// first); the sort is pushed into SQL so pagination stays
// globally consistent across pages (the previous in-memory sort
// only re-ordered the current page and would mis-rank once
// pagination was added).
//
// Paginated (limit/offset, default 100, max 200) and searchable
// (q — websearch_to_tsquery against facts.search_tsv, which
// covers facts.text). The response is a pageEnvelope:
// {data, total, limit, offset}.
func (s *Source) ListRepoFacts(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = "stable"
	} else if statusFilter == "all" {
		statusFilter = ""
	}

	sortParam := r.URL.Query().Get("sort")
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset := parsePaging(r)

	// `concepts` query param: comma-separated list of 2-20 concept
	// UUIDs or canonical names. When set, returns the SHARED facts
	// (intersection) — facts linked to at least one context of EVERY
	// given concept. Mutually exclusive with no other filter here
	// (this endpoint has no single-concept filter; the
	// /concepts/{conceptID}/facts route covers that case).
	conceptsParam := strings.TrimSpace(r.URL.Query().Get("concepts"))
	if conceptsParam != "" {
		parts := strings.Split(conceptsParam, ",")
		seen := make(map[string]struct{}, len(parts))
		deduped := make([]string, 0, len(parts))
		for _, p := range parts {
			key := strings.ToLower(strings.TrimSpace(p))
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			deduped = append(deduped, strings.TrimSpace(p))
		}
		if len(deduped) < 2 {
			httputil.WriteError(w, http.StatusBadRequest, "`concepts` requires at least 2 distinct entries")
			return
		}
		if len(deduped) > 20 {
			httputil.WriteError(w, http.StatusBadRequest, "`concepts` accepts at most 20 entries")
			return
		}
		var allIDs []pgtype.UUID
		var allGroups []int32
		for idx, c := range deduped {
			_, rows, rErr := resolveConceptGroup(r.Context(), queries, repoID, c)
			if rErr != nil {
				httputil.WriteError(w, http.StatusBadRequest, rErr.Error())
				return
			}
			for _, row := range rows {
				allIDs = append(allIDs, row.ID)
				allGroups = append(allGroups, int32(idx))
			}
		}
		if len(allIDs) == 0 {
			httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
				Data:   []store.ListSharedFactsByConceptGroupsRow{},
				Total:  0,
				Limit:  limit,
				Offset: offset,
			})
			return
		}
		facts, err := queries.ListSharedFactsByConceptGroups(r.Context(), store.ListSharedFactsByConceptGroupsParams{
			RepositoryID: repoID,
			Column2:      statusFilter,
			Column3:      search,
			Column4:      allIDs,
			Column5:      allGroups,
			Column6:      sortParam,
			Limit:        int32(limit),
			Offset:       int32(offset),
		})
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to list shared facts")
			return
		}
		total, err := queries.CountSharedFactsByConceptGroups(r.Context(), store.CountSharedFactsByConceptGroupsParams{
			RepositoryID: repoID,
			Column2:      statusFilter,
			Column3:      search,
			Column4:      allIDs,
			Column5:      allGroups,
		})
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to count shared facts")
			return
		}
		httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
			Data:   facts,
			Total:  total,
			Limit:  limit,
			Offset: offset,
		})
		return
	}

	facts, err := queries.ListFactsByRepoWithSourceCount(r.Context(), store.ListFactsByRepoWithSourceCountParams{
		RepositoryID: repoID,
		Column2:      statusFilter,
		Column3:      search,
		Column4:      sortParam,
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list facts")
		return
	}

	total, err := queries.CountFactsByRepo(r.Context(), store.CountFactsByRepoParams{
		RepositoryID: repoID,
		Column2:      statusFilter,
		Column3:      search,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count facts")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   facts,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// resolveConceptGroup resolves a concept identifier (UUID or
// canonical name) into its whole group's per-context concept rows,
// verifying ownership against repoID (cross-repo isolation). Mirrors
// the MCP resolveConcept helper. Returns the canonical name and the
// group rows; on miss/cross-repo returns an error suitable for a 400.
func resolveConceptGroup(ctx context.Context, queries *store.Queries, repoID pgtype.UUID, concept string) (string, []store.ListConceptsByRepoNameRow, error) {
	if concept == "" {
		return "", nil, errors.New("concept is required")
	}
	var conceptID pgtype.UUID
	if err := conceptID.Scan(concept); err == nil {
		c, err := queries.GetConceptByID(ctx, conceptID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", nil, errors.New("concept not found")
			}
			return "", nil, fmt.Errorf("failed to get concept: %w", err)
		}
		if c.RepositoryID != repoID {
			return "", nil, errors.New("concept not found")
		}
		rows, err := queries.ListConceptsByRepoName(ctx, store.ListConceptsByRepoNameParams{
			RepositoryID:  repoID,
			CanonicalName: c.CanonicalName,
		})
		if err != nil {
			return "", nil, fmt.Errorf("failed to load concept group: %w", err)
		}
		if len(rows) == 0 {
			return "", nil, errors.New("concept not found")
		}
		return c.CanonicalName, rows, nil
	}
	rows, err := queries.ListConceptsByRepoName(ctx, store.ListConceptsByRepoNameParams{
		RepositoryID:  repoID,
		CanonicalName: concept,
	})
	if err != nil {
		return "", nil, fmt.Errorf("failed to load concept group: %w", err)
	}
	if len(rows) == 0 {
		return "", nil, errors.New("concept not found")
	}
	return rows[0].CanonicalName, rows, nil
}

// GetFact handles GET /{slug}/facts/{factID}.
//
// Returns the fact row plus the full source list (id, url,
// parsed_title, chunk_index, first_seen_at) so the user can
// validate the fact against every source that supports it. The
// repo ownership check goes through the fact's sources: a fact
// whose sources all belong to a different repository is a 404,
// not a leak.
func (s *Source) GetFact(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	factIDRaw := chi.URLParam(r, "factID")
	if factIDRaw == "" {
		httputil.WriteError(w, http.StatusBadRequest, "factID is required")
		return
	}
	var factID pgtype.UUID
	if err := factID.Scan(factIDRaw); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid fact id")
		return
	}

	fact, err := queries.GetFactByID(r.Context(), factID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "fact not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get fact")
		return
	}

	sources, err := queries.ListFactSources(r.Context(), factID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list fact sources")
		return
	}

	// Enforce repo ownership: every source linked to the fact
	// must belong to the URL's repository. An empty source list
	// is treated as "not found" — a fact with no sources is an
	// orphan and should not be surfaced via this endpoint.
	if len(sources) == 0 {
		httputil.WriteError(w, http.StatusNotFound, "fact not found")
		return
	}
	for _, fs := range sources {
		// Re-resolve the source's repository_id. The
		// ListFactSources query returns url + parsed_title but
		// not repository_id, so we fetch the source row to
		// compare. This is N queries per fact detail, bounded
		// by the source_count (typically single digits); a
		// future query can JOIN repository_id in to collapse
		// to one.
		src, err := queries.GetSourceByID(r.Context(), fs.SourceID)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to verify source ownership")
			return
		}
		if src.RepositoryID != repoID {
			httputil.WriteError(w, http.StatusNotFound, "fact not found")
			return
		}
	}

	// source_count is the length of the source list (computed,
	// not denormalized) so the detail response carries the same
	// signal as the list response.
	//
	// The concepts linked to this fact (the extract_concepts
	// worker's output) are included inline so the fact detail page
	// can render the concept tags without a follow-up call to
	// GET /facts/{factID}/concepts — the same consolidation the
	// MCP getFact tool already uses. The repo ownership check
	// above guarantees these concept rows (repo-scoped via the
	// fact's sources) belong to this repo.
	linkedConcepts, err := queries.ListConceptsByFact(r.Context(), factID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list concepts for fact")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"fact":          fact,
		"sources":       sources,
		"source_count":  len(sources),
		"concepts":      linkedConcepts,
		"concept_count": len(linkedConcepts),
	})
}
