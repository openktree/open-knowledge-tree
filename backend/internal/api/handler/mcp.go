package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/concepts"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// MCPMCPMaxFacts caps the number of facts the searchFacts tool
// returns per call. The default is 10 (the LLM context budget); the
// caller can raise it up to MCPMCPMaxFactsCap when it knows it wants
// a larger page (e.g. dumping a concept's full fact set into context
// for a one-shot summary). The cap keeps the response bounded even
// when the LLM picks a large number, matching the REST API's own 200
// upper bound.
const (
	MCPMCPDefaultFacts = 10
	MCPMCPMaxFactsCap  = 200
)

// MCP bundles the MCP server-side handler. It owns the
// mark3labs/mcp-go MCPServer (the JSON-RPC tool registry) and the
// streamable-HTTP server that serves it. The HTTP wiring layer
// mounts the streamable server behind the OAuthBearer middleware
// at POST /api/v1/mcp.
//
// The three tools — getRepositories, searchFacts, getFact — are
// thin wrappers over the same store.Queries the REST handlers
// use, but they run in-process (no HTTP self-call) so they share
// the per-request user id the OAuthBearer middleware placed on the
// context. Each tool enforces the existing Casbin RBAC for the
// authenticated user; a deny is reported as a tool error (not a
// 403) so the LLM can see it and self-correct.
type MCP struct {
	deps       Deps
	mcpServer  *mcpserver.MCPServer
	httpServer *mcpserver.StreamableHTTPServer
	// resolveRepoPool resolves a repository UUID-or-slug to its
	// (UUID, *pgxpool.Pool). It mirrors what appmw.WithRepoQueries
	// does for the per-repo chi routes, but the MCP tools aren't
	// behind that middleware (they sit at /api/v1/mcp, not under
	// /{repoID}), so the resolver runs per tool call. Reusing the
	// same RepoDBCache + SlugCache keeps the resolution consistent
	// with the REST API.
	resolveRepoPool func(ctx context.Context, repoIDOrSlug string) (pgtype.UUID, *pgxpool.Pool, error)
	// taskEnqueuer inserts background jobs (retrieve_source). Set
	// via SetTaskEnqueuer; nil disables the fetchAndProcessSource
	// tool (it returns a tool error instead of 503).
	taskEnqueuer TaskEnqueuer
	// taskClient lists/cancels River jobs. Set via SetTaskClient;
	// nil disables the getSourceTasks tool.
	taskClient TaskClient
	// taskPool is the *pgxpool.Pool pointing at the database
	// River's river_job table lives in (cfg.Task.Database). The
	// summary mode of getSourceTasks/getReportTasks runs a single
	// SQL GROUP BY against river_job through this pool so the
	// agent gets GLOBAL counts (every state, every kind, every
	// page) in one round-trip — no paging through cursors to
	// accumulate a per-page summary. nil falls back to the
	// legacy per-page client-side aggregation over taskClient
	// (kept for misconfigured deployments; both are wired
	// together in production and in tests).
	taskPool *pgxpool.Pool
	// searchProviders backs the searchSources tool. Set via
	// SetSearchProviders; nil or empty disables the tool (it
	// returns a "not configured" tool error). Keys match the
	// REST /sources/{provider}/search path (serper, openalex).
	searchProviders map[string]search.SearchProvider
	// defaultSearchProvider is the provider id used when the
	// caller omits the `provider` argument. Set via
	// SetDefaultSearchProvider from cfg.Providers.Search.Provider
	// (defaults to "serper" in config.default.yaml). When the
	// configured default is not in the live searchProviders map,
	// the handler falls back to the first registered provider.
	defaultSearchProvider string
}

// NewMCP constructs the MCP handler bundle. It builds the
// mark3labs/mcp-go server, registers the three tools, and wraps it
// in a stateless streamable-HTTP server mounted at /api/v1/mcp.
// resolveRepoPool is the per-call repository resolver the wiring
// layer supplies (it closes over the registry + caches).
func NewMCP(deps Deps, resolveRepoPool func(ctx context.Context, repoIDOrSlug string) (pgtype.UUID, *pgxpool.Pool, error)) *MCP {
	srv := mcpserver.NewMCPServer(
		"open-knowledge-tree",
		"1.0.0",
		mcpserver.WithToolCapabilities(false),
	)
	m := &MCP{
		deps:            deps,
		mcpServer:       srv,
		resolveRepoPool: resolveRepoPool,
	}
	m.registerTools()
	// Stateless streamable HTTP: no session management, every
	// request carries its own bearer token. WithEndpointPath is
	// a hint for the embedded /.well-known handler; we mount the
	// httpServer directly in wiring.go so the path is decided by
	// the chi route, not by this option. WithStateLess disables
	// the session-id requirement so a single POST with a fresh
	// bearer token works without an initialize handshake.
	m.httpServer = mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithEndpointPath("/mcp"),
		mcpserver.WithStateLess(true),
	)
	return m
}

// SetTaskEnqueuer attaches the background-task enqueuer the
// fetchAndProcessSource tool uses to insert retrieve_source jobs.
// Optional: when nil, the tool returns a tool error. Idempotent.
func (m *MCP) SetTaskEnqueuer(eq TaskEnqueuer) {
	m.taskEnqueuer = eq
}

// SetTaskClient attaches the River client the getSourceTasks tool
// uses to list jobs filtered by repo + source metadata. Optional:
// when nil, the tool returns a tool error. Idempotent.
func (m *MCP) SetTaskClient(c TaskClient) {
	m.taskClient = c
}

// SetTaskPool attaches the pool pointing at the database River's
// river_job table lives in. The summary (verbose=false) mode of
// getSourceTasks and getReportTasks runs a single SQL GROUP BY
// through this pool so the agent gets global counts in one call
// instead of paging through the per-page summary. Optional: when
// nil, both tools fall back to the legacy per-page client-side
// aggregation over taskClient (slower but still correct). Idempotent.
func (m *MCP) SetTaskPool(p *pgxpool.Pool) {
	m.taskPool = p
}

// SetSearchProviders attaches the search providers the searchSources
// tool uses to discover candidate source URLs via Serper/OpenAlex.
// Optional: when nil or empty, the tool returns a "search providers
// not configured" tool error. Idempotent. Keys are the same
// provider ids the REST /sources/{provider}/search path uses
// (serper, openalex).
func (m *MCP) SetSearchProviders(p map[string]search.SearchProvider) {
	m.searchProviders = p
}

// SetDefaultSearchProvider sets the provider id used when the caller
// omits the `provider` argument. Should be set from
// cfg.Providers.Search.Provider (defaults to "serper" in
// config.default.yaml). When the configured default is not in the
// live searchProviders map, the handler falls back to the first
// registered provider alphabetically.
func (m *MCP) SetDefaultSearchProvider(id string) {
	m.defaultSearchProvider = id
}

// ServeHTTP is the chi-mounted entry point. The wiring layer wraps
// it with OAuthBearer; here we just delegate to the streamable-HTTP
// server which speaks the JSON-RPC protocol.
func (m *MCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.httpServer.ServeHTTP(w, r)
}

// registerTools wires the three MCP tools into the mark3labs server.
// Each tool definition carries a JSON-Schema input description; the
// handler parses the args, runs the store query, and returns a
// structured result (the mark3labs library serializes it into the
// `structuredContent` field of the tool result and a JSON text blob
// into `content` for backward compatibility).
func (m *MCP) registerTools() {
	// getRepositories: list the repositories the authenticated user
	// can see. No input. Mirrors GET /repositories but returns only
	// the fields an LLM needs to pick a repository for searchFacts.
	m.mcpServer.AddTool(
		mcp.NewTool("getRepositories",
			mcp.WithDescription("List the OKT repositories the authenticated user can access. Returns each repository's id, name, slug, description, tier, and roles. Use the id or slug as the `repository` argument to searchFacts and getFact."),
		),
		m.handleGetRepositories,
	)

	// searchFacts: full-text search over a repository's facts. Caps
	// at 200 results (the caller chooses up to that via limit). The
	// repository arg accepts a UUID or a slug; the tool resolves it
	// the same way the REST API's /{repoID} routes do.
	m.mcpServer.AddTool(
		mcp.NewTool("searchFacts",
			mcp.WithDescription("Full-text search over the facts in a repository. Returns up to `limit` facts (default 10, max 200), each with its id, text, status, fact_kind, source_count, and created_at. Use the id with getFact to see the source URLs. The repository argument accepts a UUID or a slug (use getRepositories to list them). The optional `concept` filter (a concept UUID or canonical name) restricts to facts linked to that concept; `context` narrows a canonical-name concept filter to one context. Pass `concepts` (2 to 20 concept UUIDs or canonical names) to get the SHARED facts (intersection) across all of them — facts linked to at least one context of EVERY given concept. `concepts` and `concept` are mutually exclusive."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug (from getRepositories)."),
			),
			mcp.WithString("query",
				mcp.Description("Full-text search query (Postgres websearch_to_tsquery syntax: space-separated words, quoted phrases, OR/AND, negation with -). Empty returns the newest facts."),
			),
			mcp.WithString("concept",
				mcp.Description("Optional single concept filter: a concept UUID or canonical name. When set, only facts linked to that concept (or, for a canonical name, to ANY context sharing that name) are returned. Mutually exclusive with `concepts`."),
			),
			mcp.WithString("context",
				mcp.Description("Optional context filter used with the `concept` argument when it is a canonical name: narrows the matched concept group to the single context whose name matches. Ignored when `concept` is a UUID."),
			),
			mcp.WithArray("concepts",
				mcp.Description("Optional 2-20 concept UUIDs or canonical names. When set, returns the SHARED facts (intersection) — facts linked to at least one context of EVERY given concept. Mutually exclusive with `concept`."),
				mcp.Items(map[string]any{"type": "string"}),
				mcp.MinItems(2),
				mcp.MaxItems(20),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum facts to return (1-200, default 10)."),
			),
			mcp.WithNumber("offset",
				mcp.Description("Number of facts to skip for pagination (default 0)."),
			),
		),
		m.handleSearchFacts,
	)

	// getFact: single fact detail with the full source URL list.
	// This is the only tool that surfaces source URLs; searchFacts
	// returns only the source count.
	m.mcpServer.AddTool(
		mcp.NewTool("getFact",
			mcp.WithDescription("Get a single fact's metadata, the full list of source URLs that support it, and the concepts linked to it. Returns the fact (id, text, status, fact_kind, embedded_model, created_at, image_url), sources (url, parsed_title, first_seen_at), source_count, concepts (id, canonical_name, context, description), and concept_count. The repository argument accepts a UUID or slug; the factId is a UUID from searchFacts."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug the fact belongs to."),
			),
			mcp.WithString("factId",
				mcp.Required(),
				mcp.Description("Fact UUID (from searchFacts)."),
			),
		),
		m.handleGetFact,
	)

	// searchConcepts: list concept groups in a repository.
	m.mcpServer.AddTool(
		mcp.NewTool("searchConcepts",
			mcp.WithDescription("List the concept groups in a repository, optionally filtered by canonical-name substring. Each group carries its canonical name, total fact_count across contexts, and a contexts array (concept_id, context, fact_count, aliases). Use getConcept / getConceptSummaries / getRelatedConcepts with a concept_id or canonical name to drill in. The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug (from getRepositories)."),
			),
			mcp.WithString("query",
				mcp.Description("Optional canonical-name substring filter (case-insensitive). Empty returns all groups."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum groups to return (1-200, default 50)."),
			),
			mcp.WithNumber("offset",
				mcp.Description("Number of groups to skip for pagination (default 0)."),
			),
		),
		m.handleSearchConcepts,
	)

	// getConcept: a concept group's metadata + the synthesis
	// ("definition") the synthesize_concept worker produced for it.
	m.mcpServer.AddTool(
		mcp.NewTool("getConcept",
			mcp.WithDescription("Get a concept's full group (all contexts sharing the canonical name, with per-context aliases and fact_count) plus the authoritative synthesis/definition text when one exists. The `concept` argument accepts a concept UUID or a canonical name. The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
			mcp.WithString("concept",
				mcp.Required(),
				mcp.Description("Concept UUID or canonical name."),
			),
		),
		m.handleGetConcept,
	)

	// getConceptSummaries: the per-context summary slices for a
	// concept group (the summarize_concepts worker's output).
	m.mcpServer.AddTool(
		mcp.NewTool("getConceptSummaries",
			mcp.WithDescription("Get the summary slices the summarize_concepts worker produced for a concept group. Returns one slice per (context, sequence_num), each with its content, model, is_complete flag, and covered fact_count. The `concept` argument accepts a concept UUID or a canonical name. The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
			mcp.WithString("concept",
				mcp.Required(),
				mcp.Description("Concept UUID or canonical name."),
			),
		),
		m.handleGetConceptSummaries,
	)

	// getRelatedConcepts: concepts that share facts with the given
	// concept group, ranked by shared_fact_count.
	m.mcpServer.AddTool(
		mcp.NewTool("getRelatedConcepts",
			mcp.WithDescription("List concepts related to the given concept group, ranked by the number of shared facts. Each entry carries the related concept's canonical_name, a representative concept_id, and shared_fact_count. The `concept` argument accepts a concept UUID or a canonical name. The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
			mcp.WithString("concept",
				mcp.Required(),
				mcp.Description("Concept UUID or canonical name."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum related concepts to return (1-200, default 50)."),
			),
			mcp.WithNumber("offset",
				mcp.Description("Number of entries to skip for pagination (default 0)."),
			),
		),
		m.handleGetRelatedConcepts,
	)

	// getInvestigation: an investigation's metadata + the sources
	// it collects.
	m.mcpServer.AddTool(
		mcp.NewTool("getInvestigation",
			mcp.WithDescription("Get an investigation's metadata (id, title, topic, created_at, updated_at) and the sources it collects (url, parsed_title, doi, created_at, added_at). The repository argument accepts a UUID or slug; the investigationId is a UUID."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug the investigation belongs to."),
			),
			mcp.WithString("investigationId",
				mcp.Required(),
				mcp.Description("Investigation UUID."),
			),
		),
		m.handleGetInvestigation,
	)

	// createInvestigation: create a new investigation in a repo.
	m.mcpServer.AddTool(
		mcp.NewTool("createInvestigation",
			mcp.WithDescription("Create a new investigation in a repository. An investigation collects sources around a topic; after creation, pass the returned id as `investigationId` to fetchAndProcessSource to fetch + organize sources into the investigation in a single call (the preferred flow). Alternatively, use addInvestigationSource to reorganize an already-fetched source. Returns the created investigation (id, title, topic, created_at). The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Investigation title (required)."),
			),
			mcp.WithString("topic",
				mcp.Description("Optional topic/free-text description."),
			),
		),
		m.handleCreateInvestigation,
	)

	// addInvestigationSource: link an already-fetched source to
	// an investigation. This is the reorganize path — the
	// preferred way to fetch + organize in one call is
	// fetchAndProcessSource with its optional investigationId
	// parameter. This tool exists for the case where the source
	// was fetched without an investigationId (or was uploaded
	// through another path) and the agent later decides to
	// collect it into an investigation.
	m.mcpServer.AddTool(
		mcp.NewTool("addInvestigationSource",
			mcp.WithDescription("Link an existing, already-fetched source to an investigation. Idempotent: re-adding a source is a no-op. The source must belong to the same repository as the investigation. This is the reorganize path; the preferred way to fetch + organize in one call is fetchAndProcessSource with its optional `investigationId` parameter. Use this tool when the source was already fetched without an investigationId. The repository argument accepts a UUID or slug; investigationId and sourceId are UUIDs."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug the investigation and source belong to."),
			),
			mcp.WithString("investigationId",
				mcp.Required(),
				mcp.Description("Investigation UUID to link the source into."),
			),
			mcp.WithString("sourceId",
				mcp.Required(),
				mcp.Description("Source UUID to link. Must belong to the same repository as the investigation."),
			),
		),
		m.handleAddInvestigationSource,
	)

	// fetchAndProcessSource: enqueue a URL/DOI for retrieval +
	// processing. When investigationId is supplied the worker
	// also links the resulting source row into that
	// investigation, so an agent can fetch + organize in a
	// single call — the preferred flow. The standalone
	// addInvestigationSource tool is kept for reorganizing
	// already-fetched sources.
	m.mcpServer.AddTool(
		mcp.NewTool("fetchAndProcessSource",
			mcp.WithDescription("Fetch a URL or DOI into a repository: enqueues a background job that downloads the resource, parses it, extracts facts, and links them. When `investigationId` is supplied the worker also links the resulting source into that investigation once the row exists, so this is the preferred single-call way to fetch + organize a source. Returns the job id and the classified resource type. Use getSourceTasks with the returned source_id to track progress. The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug to fetch the source into."),
			),
			mcp.WithString("url",
				mcp.Description("The URL to fetch. Required unless `doi` is given."),
			),
			mcp.WithString("doi",
				mcp.Description("A bare DOI (e.g. 10.1234/example) to fetch. Used instead of `url` when the resource is a DOI."),
			),
			mcp.WithString("investigationId",
				mcp.Description("Optional investigation UUID. When supplied, the worker links the fetched source into this investigation once the source row exists — the preferred one-call fetch + organize flow. The investigation must belong to the same repository. Omit when fetching without organizing, or use addInvestigationSource to reorganize an already-fetched source."),
			),
		),
		m.handleFetchAndProcessSource,
	)

	// getSourceTasks: list the background jobs for a source, an
	// investigation, or the whole repo; with a compact progress
	// summary mode and pagination so the agent can poll the full
	// 7-stage ingestion pipeline until it drains.
	m.mcpServer.AddTool(
		mcp.NewTool("getSourceTasks",
				mcp.WithDescription("Track ingestion progress for a repository, a single source, or an investigation's sources. The ingestion pipeline is 7 stages deep — retrieve_source → source_decomposition → embed_facts → deduplicate_facts → extract_concepts → {embed_concepts, summarize_concepts → synthesize_concept, refresh_concept_relations} — and a source is only fully ingested once ALL of them finalize. A single source fans out into MANY jobs (one per fact for embed/dedup/extract, one per concept for summarize/synthesize), so 100 sources can produce THOUSANDS of jobs and take an HOUR or more to fully drain. By default (verbose omitted/false) returns a COMPACT summary in ONE call: counts_by_state, pending_count (non-finalized jobs across the WHOLE scope), running_count, total, and a `complete` boolean. `complete=true` means pending_count==0 globally — the whole pipeline has drained and it is safe to synthesize; NO paging or drain protocol is needed. Set byKind=true to also include counts_by_kind and counts_by_kind_and_state (kind → state → count) when you need the per-kind breakdown. `state`/`kind` filters are IGNORED in summary mode (they are inspection-only); the global query already reports every state and, under byKind, every kind. Set verbose=true for the per-job row list (paginated via cursor/limit) — the verbose path is per-page only, so use the default summary (verbose=false) to confirm drain, never the verbose row list. Wait proportionally to source count — 1 source ≈ minutes, 10 sources ≈ 10-20 min, 100 sources ≈ 1 hour; sleep 15-30s between polls. NEVER synthesize while pending_count > 0. The repository argument accepts a UUID or slug. `sourceId` and `investigationId` are mutually exclusive. When `investigationId` is set, the tool resolves the investigation's source_ids and returns jobs for those sources (retrieve_source jobs are dropped because they lack source_id in metadata). To track a source's full pipeline INCLUDING retrieve_source, poll with sourceId instead (the getInvestigation tool returns each source's id). Use `state` (available|running|retryable|pending|scheduled|completed|cancelled|discarded) and `kind` for INSPECTION only, never for drain confirmation. Use `limit`/`cursor` only with verbose=true. `byKind` is ignored when verbose=true."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
			mcp.WithString("sourceId",
				mcp.Description("Optional source UUID filter. When set, only jobs carrying this source_id in their metadata are returned (all pipeline stages carry source_id EXCEPT retrieve_source, which carries only repo_id — so a sourceId poll shows every downstream stage but not the retrieve_source job itself; that is visible only in a repo-wide, unscoped poll). Mutually exclusive with investigationId."),
			),
			mcp.WithString("investigationId",
				mcp.Description("Optional investigation UUID. When set, the tool resolves the investigation's source_ids and returns jobs for those sources (a source may belong to multiple investigations; this filters to the sources currently linked to this one). retrieve_source jobs are NOT included (they lack source_id in metadata, so they can't be attributed to an investigation); to track a source's full pipeline use sourceId instead. Mutually exclusive with sourceId."),
			),
			mcp.WithString("state",
				mcp.Description("Optional River job state filter: available|running|retryable|pending|scheduled|completed|cancelled|discarded. Omit for all states. For INSPECTION only — ignored in summary mode (the global query reports every state)."),
			),
			mcp.WithString("kind",
				mcp.Description("Optional job kind filter (e.g. retrieve_source, source_decomposition, embed_facts, deduplicate_facts, extract_concepts, synthesize_concept). For INSPECTION only — ignored in summary mode (the global query reports every kind under byKind)."),
			),
			mcp.WithBoolean("byKind",
				mcp.Description("When true (summary mode only), also include counts_by_kind and counts_by_kind_and_state in the response. Omit for the compact state-only summary. Ignored when verbose=true."),
			),
			mcp.WithBoolean("verbose",
				mcp.Description("When true, return the per-job row list (paginated). When omitted/false, return a GLOBAL progress summary in one call: counts_by_state, pending_count, running_count, total, and a `complete` boolean (true when pending_count==0 globally — no paging needed). Set byKind=true to add counts_by_kind and counts_by_kind_and_state."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum jobs to return per page (1-200, default 50). Verbose mode only; the summary mode runs a single un-paged SQL aggregate."),
			),
			mcp.WithString("cursor",
				mcp.Description("Opaque pagination cursor from a previous response's next_cursor. Empty/omitted = first page. Verbose mode only."),
			),
		),
		m.handleGetSourceTasks,
	)

	// createReport: create a new report and enqueue autofact
	// annotation. The report body is raw markdown; the worker chunks
	// it into sentences, embeds each, and searches the facts
	// collection for supporting citations above the configured
	// threshold. Use getReport to read the annotations once the job
	// completes.
	m.mcpServer.AddTool(
		mcp.NewTool("createReport",
			mcp.WithDescription("Create a new report in a repository from raw markdown text and enqueue an autofact-annotation job. The job chunks the report into sentences, embeds each, and searches the repository's facts for similar ones above the configured similarity threshold; matches are stored as report_annotations. Use getReport with the returned report_id to read the annotated body (each sentence with its auto-cited facts). Reports may be nested as sub-reports: pass parentId to place this report under an existing report, or pass childrenIds to reparent existing reports under this new one (the meta-synthesis case where the parent is created after its children). All ids must belong to the same repository. The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Report title (required)."),
			),
			mcp.WithString("text",
				mcp.Required(),
				mcp.Description("The report body as raw markdown text (required)."),
			),
			mcp.WithString("topic",
				mcp.Description("Optional topic/free-text description."),
			),
			mcp.WithString("parentId",
				mcp.Description("Optional UUID of an existing report to set as the parent of this new report (must belong to the same repo)."),
			),
			mcp.WithArray("childrenIds",
				mcp.WithStringItems(),
				mcp.Description("Optional list of existing report UUIDs to reparent under this new report (the meta-synthesis case where the parent is created after its children). All ids must belong to the same repo."),
			),
		),
		m.handleCreateReport,
	)

	// getReport: a report's metadata + its annotations grouped by
	// sentence_index. Each annotation carries the matched fact's id,
	// text, score (cosine similarity, 0..1), and source_count so the
	// caller can judge how well-supported each sentence is.
	m.mcpServer.AddTool(
		mcp.NewTool("getReport",
			mcp.WithDescription("Get a report's metadata (id, title, topic, status, body_md, sentence_count, similarity_threshold, embedded_model, created_at) and its annotations. Each annotation carries the sentence_index, sentence_text, the matched fact (id, text, status, fact_kind, source_count, created_at), the score (cosine similarity 0..1, higher = stronger match), and the posture (related|supports|contradicts; empty when the autocite posture classifier was not configured — irrelevant matches are dropped before persistence). The repository argument accepts a UUID or slug; the reportId is a UUID from createReport."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug the report belongs to."),
			),
			mcp.WithString("reportId",
				mcp.Required(),
				mcp.Description("Report UUID (from createReport)."),
			),
		),
		m.handleGetReport,
	)

	// getReportTasks: list the background annotation jobs in a
	// repository, optionally filtered by report. Mirrors
	// getSourceTasks (same pagination, summary/verbose modes, state
	// and kind filters, and complete/complete_unreliable drain
	// signals) so an agent can poll an annotate_report job to
	// completion with the same drain protocol.
	m.mcpServer.AddTool(
		mcp.NewTool("getReportTasks",
			mcp.WithDescription("List the background annotation jobs in a repository, optionally filtered by report, so the caller can track annotation progress. By default (verbose omitted/false) returns a COMPACT summary in ONE call: counts_by_state, pending_count (non-finalized jobs across the WHOLE scope), running_count, total, and a `complete` boolean. `complete=true` means pending_count==0 globally — the annotation has drained and getReport can be called to read the annotated body; NO paging or drain protocol is needed. Set byKind=true to also include counts_by_kind and counts_by_kind_and_state (kind → state → count). `state`/`kind` filters are IGNORED in summary mode (they are inspection-only). Set verbose=true for the per-job row list (paginated via cursor/limit) — the verbose path is per-page only, so use the default summary to confirm drain, never the verbose row list. Sleep 15-30s between polls. When `reportId` is omitted, all repo jobs are listed. The repository argument accepts a UUID or slug. `byKind` is ignored when verbose=true."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
			mcp.WithString("reportId",
				mcp.Description("Optional report UUID filter. When set, only jobs carrying this report_id in their metadata are returned."),
			),
			mcp.WithString("state",
				mcp.Description("Optional River job state filter: available|running|retryable|pending|scheduled|completed|cancelled|discarded. Omit for all states. For INSPECTION only — ignored in summary mode (the global query reports every state)."),
			),
			mcp.WithString("kind",
				mcp.Description("Optional job kind filter (e.g. annotate_report). For INSPECTION only — ignored in summary mode (the global query reports every kind under byKind)."),
			),
			mcp.WithBoolean("byKind",
				mcp.Description("When true (summary mode only), also include counts_by_kind and counts_by_kind_and_state in the response. Omit for the compact state-only summary. Ignored when verbose=true."),
			),
			mcp.WithBoolean("verbose",
				mcp.Description("When true, return the per-job row list (paginated). When omitted/false, return a GLOBAL progress summary in one call: counts_by_state, pending_count, running_count, total, and a `complete` boolean (true when pending_count==0 globally — no paging needed). Set byKind=true to add counts_by_kind and counts_by_kind_and_state."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum jobs to return per page (1-200, default 50). Verbose mode only; the summary mode runs a single un-paged SQL aggregate."),
			),
			mcp.WithString("cursor",
				mcp.Description("Opaque pagination cursor from a previous response's next_cursor. Empty/omitted = first page. Verbose mode only."),
			),
		),
		m.handleGetReportTasks,
	)

	// searchSources: discover candidate source URLs via a search
	// provider (Serper for web, OpenAlex for academic works) so the
	// agent can find sources to feed into fetchAndProcessSource.
	m.mcpServer.AddTool(
		mcp.NewTool("searchSources",
			mcp.WithDescription("Search for candidate source URLs via a registered search provider (serper = Google web search, openalex = academic works, registry = OKT Knowledge Registry cached sources). Returns each hit's title, url, snippet, and (for OpenAlex) doi, openalex_id, and published_at, plus already_exists/existing_status flags for hits already ingested in this repository. Feed the returned url or doi straight into fetchAndProcessSource; skip hits where already_exists is true. Omit `provider` to use the configured default (typically serper); use `cursor` for pagination. Call listSearchProviders first to see which providers are available for this repository — repos can disable individual providers (e.g. a strict scientific repo may disable Serper). The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug (used for the already-exists tagging and the facts:read permission check)."),
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Search query."),
			),
			mcp.WithString("provider",
				mcp.Description("Search provider id: \"serper\" (web), \"openalex\" (academic), or \"registry\" (OKT Knowledge Registry cached sources). Omit to use the configured default (typically serper). Call listSearchProviders to see which providers are available for this repository."),
			),
			mcp.WithNumber("per_page",
				mcp.Description("Page size (0 = provider default)."),
			),
			mcp.WithString("cursor",
				mcp.Description("Opaque pagination cursor from a previous response's next_cursor. Empty/omitted = first page."),
			),
		),
		m.handleSearchSources,
	)

	// listSearchProviders: lists the search providers available
	// in this deployment AND enabled for the given repository. An
	// agent calls this before searchSources when it needs to know
	// which providers it can use (e.g. a "scientific" repo may have
	// Serper disabled so only OpenAlex is available).
	m.mcpServer.AddTool(
		mcp.NewTool("listSearchProviders",
			mcp.WithDescription("List the search providers available in this deployment and enabled for the given repository. Returns each provider's id (e.g. \"serper\", \"openalex\"), human-readable name, whether it is enabled for the repository, and whether it is the configured default. An agent should call this before searchSources when it needs to know which providers are available — repos can disable individual providers (e.g. a strict scientific repo may disable Serper web search and keep only OpenAlex). The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
		),
		m.handleListSearchProviders,
	)

	// listReports: list reports in a repository with optional
	// search and status filtering. Paginated. Use getReport with a
	// returned id to read the full annotated report body.
	m.mcpServer.AddTool(
		mcp.NewTool("listReports",
			mcp.WithDescription("List reports in a repository, optionally filtered by title/topic search text and/or annotation status. Returns each report's id, title, topic, status, sentence_count, created_at, and updated_at (not the full body — use getReport with a returned id to read the annotated body). Paginated via limit/offset. The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug."),
			),
			mcp.WithString("search",
				mcp.Description("Optional search text — matches title or topic via ILIKE. Empty returns all reports."),
			),
			mcp.WithString("status",
				mcp.Description("Optional status filter: pending, processing, annotated, or failed. Empty returns all statuses."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum reports to return (1-200, default 50)."),
			),
			mcp.WithNumber("offset",
				mcp.Description("Number of reports to skip for pagination (default 0)."),
			),
		),
		m.handleListReports,
	)

	// updateReport: update a report's title/topic/body_md and/or
	// reparent it / set its children. Re-annotates automatically when
	// the body changes. Mirrors the REST PUT /reports/{reportID}.
	m.mcpServer.AddTool(
		mcp.NewTool("updateReport",
			mcp.WithDescription("Update an existing report's title, topic, body_md, and/or parentage. When body_md differs from the stored value an autofact-annotation job is enqueued (auto-re-annotation) and status resets to pending; reparenting alone does NOT re-annotate. Optional parentId reparents this report under another report (an explicit empty string clears the parent, returning it to top-level); optional childrenIds reparents those existing reports under this one. Cycle prevention: parentId must not be this report or any of its descendants. All ids must belong to the same repository. The repository argument accepts a UUID or slug."),
			mcp.WithString("repository",
				mcp.Required(),
				mcp.Description("Repository UUID or slug the report belongs to."),
			),
			mcp.WithString("reportId",
				mcp.Required(),
				mcp.Description("Report UUID to update."),
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("New title (required)."),
			),
			mcp.WithString("text",
				mcp.Required(),
				mcp.Description("New report body as raw markdown text (required)."),
			),
			mcp.WithString("topic",
				mcp.Description("Optional new topic/free-text description."),
			),
			mcp.WithString("parentId",
				mcp.Description("Optional UUID of a report to set as the parent. An explicit empty string clears the parent (top-level). Omit to leave parentage unchanged."),
			),
			mcp.WithArray("childrenIds",
				mcp.WithStringItems(),
				mcp.Description("Optional list of existing report UUIDs to reparent under this report. All ids must belong to the same repo."),
			),
		),
		m.handleUpdateReport,
	)
}

// handleGetRepositories is the getRepositories tool handler. It
// reuses the same ListAllRepositories / ListRepositoriesByOwner +
// GetRolesForUser logic the REST ListRepositories handler uses, so
// the visibility rules are identical. The result is a JSON array
// the LLM can read to pick a repository.
func (m *MCP) handleGetRepositories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	isSysAdmin, err := m.deps.RBAC.EnforceSystemAdmin(uid.String())
	if err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	}
	var repos []store.Repository
	if isSysAdmin {
		repos, err = m.deps.Store.ListAllRepositories(ctx)
	} else {
		repos, err = m.deps.Store.ListRepositoriesByOwner(ctx, uid)
	}
	if err != nil {
		return mcp.NewToolResultError("failed to list repositories"), nil
	}
	type repoOut struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Slug        string   `json:"slug"`
		Description string   `json:"description"`
		Tier        string   `json:"tier"`
		Roles       []string `json:"roles"`
	}
	out := make([]repoOut, 0, len(repos))
	for _, repo := range repos {
		roles, _ := m.deps.RBAC.GetRolesForUser(uid.String(), repo.ID.String())
		if roles == nil {
			roles = []string{}
		}
		if repo.OwnerID.String() == uid.String() && len(roles) == 0 {
			roles = []string{rbac.RoleRepoAdmin}
		}
		out = append(out, repoOut{
			ID:          repo.ID.String(),
			Name:        repo.Name,
			Slug:        repo.Slug,
			Description: repo.Description,
			Tier:        repo.Tier,
			Roles:       roles,
		})
	}
	return structuredResult(map[string]any{"repositories": out})
}

// handleSearchFacts is the searchFacts tool handler. It resolves
// the repository (UUID or slug), checks the user has facts:read on
// it (sysadmins pass), then runs the same
// ListFactsByRepoWithSourceCount query the REST ListRepoFacts uses
// when no concept filter is present, or the
// ListFactsByRepoWithSourceCountForConcept variant when the caller
// passed a `concept` argument (resolved to an array of concept_ids
// via the resolveConceptIDs helper). The result carries each fact's
// source_count so the LLM can decide which facts are well-supported
// without a follow-up getFact call.
func (m *MCP) handleSearchFacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	query := req.GetString("query", "")
	concept := req.GetString("concept", "")
	conceptCtx := req.GetString("context", "")
	concepts := req.GetStringSlice("concepts", nil)
	limit := req.GetInt("limit", MCPMCPDefaultFacts)
	if limit < 1 {
		limit = 1
	}
	if limit > MCPMCPMaxFactsCap {
		limit = MCPMCPMaxFactsCap
	}
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}

	if concept != "" && len(concepts) > 0 {
		return mcp.NewToolResultError("`concept` and `concepts` are mutually exclusive"), nil
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Facts, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read facts in this repository"), nil
	}

	queries := store.New(pool)
	type factOut struct {
		ID          string `json:"id"`
		Text        string `json:"text"`
		Status      string `json:"status"`
		FactKind    string `json:"fact_kind"`
		SourceCount int64  `json:"source_count"`
		CreatedAt   string `json:"created_at"`
	}
	toFactOut := func(r store.ListFactsByRepoWithSourceCountRow) factOut {
		return factOut{
			ID:          r.ID.String(),
			Text:        r.Text,
			Status:      r.Status,
			FactKind:    r.FactKind,
			SourceCount: r.SourceCount,
			CreatedAt:   pgTimeToString(r.CreatedAt),
		}
	}

	if len(concepts) > 0 {
		// Intersection path: facts linked to ALL given concept groups.
		if len(concepts) < 2 {
			return mcp.NewToolResultError("`concepts` requires at least 2 entries"), nil
		}
		if len(concepts) > 20 {
			return mcp.NewToolResultError("`concepts` accepts at most 20 entries"), nil
		}
		// Dedup entries by lowercased name (UUIDs are canonical strings).
		seen := make(map[string]struct{}, len(concepts))
		deduped := make([]string, 0, len(concepts))
		for _, c := range concepts {
			key := strings.ToLower(strings.TrimSpace(c))
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			deduped = append(deduped, c)
		}
		if len(deduped) < 2 {
			return mcp.NewToolResultError("`concepts` requires at least 2 distinct entries after dedup"), nil
		}
		// Resolve each concept to its group's concept_ids; assign a
		// distinct group_idx per distinct canonical name (duplicates
		// would otherwise collapse via dedup above, so each entry is
		// its own group).
		var allIDs []pgtype.UUID
		var allGroups []int32
		groupNames := make([]string, 0, len(deduped))
		for idx, c := range deduped {
			_, rows, rErr := m.resolveConcept(ctx, queries, repoID, c)
			if rErr != nil {
				return mcp.NewToolResultError(rErr.Error()), nil
			}
			for _, r := range rows {
				allIDs = append(allIDs, r.ID)
				allGroups = append(allGroups, int32(idx))
			}
			groupNames = append(groupNames, c)
		}
		if len(allIDs) == 0 {
			return structuredResult(map[string]any{
				"facts":    []factOut{},
				"total":    0,
				"limit":    limit,
				"offset":   offset,
				"concepts": groupNames,
			})
		}
		facts, err := queries.ListSharedFactsByConceptGroups(ctx, store.ListSharedFactsByConceptGroupsParams{
			RepositoryID: repoID,
			Column2:      "stable",
			Column3:      query,
			Column4:      allIDs,
			Column5:      allGroups,
			Column6:      "created_at",
			Limit:        int32(limit),
			Offset:       int32(offset),
		})
		if err != nil {
			return mcp.NewToolResultError("failed to search shared facts"), nil
		}
		total, err := queries.CountSharedFactsByConceptGroups(ctx, store.CountSharedFactsByConceptGroupsParams{
			RepositoryID: repoID,
			Column2:      "stable",
			Column3:      query,
			Column4:      allIDs,
			Column5:      allGroups,
		})
		if err != nil {
			return mcp.NewToolResultError("failed to count shared facts"), nil
		}
		out := make([]factOut, 0, len(facts))
		for _, f := range facts {
			out = append(out, factOut{
				ID:          f.ID.String(),
				Text:        f.Text,
				Status:      f.Status,
				FactKind:    f.FactKind,
				SourceCount: f.SourceCount,
				CreatedAt:   pgTimeToString(f.CreatedAt),
			})
		}
		return structuredResult(map[string]any{
			"facts":    out,
			"total":    total,
			"limit":    limit,
			"offset":   offset,
			"concepts": groupNames,
		})
	}

	if concept == "" {
		facts, err := queries.ListFactsByRepoWithSourceCount(ctx, store.ListFactsByRepoWithSourceCountParams{
			RepositoryID: repoID,
			Column2:      "stable", // mirror the REST default
			Column3:      query,
			Column4:      "created_at",
			Limit:        int32(limit),
			Offset:       int32(offset),
		})
		if err != nil {
			return mcp.NewToolResultError("failed to search facts"), nil
		}
		total, err := queries.CountFactsByRepo(ctx, store.CountFactsByRepoParams{
			RepositoryID: repoID,
			Column2:      "stable",
			Column3:      query,
		})
		if err != nil {
			return mcp.NewToolResultError("failed to count facts"), nil
		}
		out := make([]factOut, 0, len(facts))
		for _, f := range facts {
			out = append(out, toFactOut(f))
		}
		return structuredResult(map[string]any{
			"facts":  out,
			"total":  total,
			"limit":  limit,
			"offset": offset,
		})
	}

	// Concept filter present: resolve the concept arg to an array of
	// concept_ids (the whole group for a canonical name, or one id
	// for a UUID), then run the concept-filtered queries.
	conceptIDs, cErr := m.resolveConceptIDs(ctx, queries, repoID, concept, conceptCtx)
	if cErr != nil {
		return mcp.NewToolResultError(cErr.Error()), nil
	}
	if len(conceptIDs) == 0 {
		return structuredResult(map[string]any{
			"facts":  []factOut{},
			"total":  0,
			"limit":  limit,
			"offset": offset,
		})
	}
	facts, err := queries.ListFactsByRepoWithSourceCountForConcept(ctx, store.ListFactsByRepoWithSourceCountForConceptParams{
		RepositoryID: repoID,
		Column2:      "stable",
		Column3:      query,
		Column4:      conceptIDs,
		Column5:      "created_at",
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		return mcp.NewToolResultError("failed to search facts"), nil
	}
	total, err := queries.CountFactsByRepoForConcept(ctx, store.CountFactsByRepoForConceptParams{
		RepositoryID: repoID,
		Column2:     "stable",
		Column3:     query,
		Column4:     conceptIDs,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to count facts"), nil
	}
	out := make([]factOut, 0, len(facts))
	for _, f := range facts {
		out = append(out, factOut{
			ID:          f.ID.String(),
			Text:        f.Text,
			Status:      f.Status,
			FactKind:    f.FactKind,
			SourceCount: f.SourceCount,
			CreatedAt:   pgTimeToString(f.CreatedAt),
		})
	}
	return structuredResult(map[string]any{
		"facts":  out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// handleGetFact is the getFact tool handler. It resolves the
// repository, checks facts:read, fetches the fact row + the full
// source list (url + parsed_title + first_seen_at), and enforces
// repo ownership the same way the REST GetFact handler does: a
// fact whose sources all belong to a different repository is a
// 404-equivalent tool error, not a leak.
func (m *MCP) handleGetFact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	factIDStr, err := req.RequireString("factId")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Facts, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read facts in this repository"), nil
	}

	var factID pgtype.UUID
	if err := factID.Scan(factIDStr); err != nil {
		return mcp.NewToolResultError("invalid factId: " + err.Error()), nil
	}

	queries := store.New(pool)
	fact, err := queries.GetFactByID(ctx, factID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mcp.NewToolResultError("fact not found"), nil
		}
		return mcp.NewToolResultError("failed to get fact"), nil
	}
	sources, err := queries.ListFactSources(ctx, factID)
	if err != nil {
		return mcp.NewToolResultError("failed to list fact sources"), nil
	}
	if len(sources) == 0 {
		return mcp.NewToolResultError("fact not found"), nil
	}
	// Enforce repo ownership: every source must belong to the
	// resolved repository. This mirrors the REST handler's
	// check; an orphaned fact or a fact whose sources live in a
	// different repo is a 404, not a leak.
	for _, fs := range sources {
		src, err := queries.GetSourceByID(ctx, fs.SourceID)
		if err != nil {
			return mcp.NewToolResultError("failed to verify source ownership"), nil
		}
		if src.RepositoryID != repoID {
			return mcp.NewToolResultError("fact not found"), nil
		}
	}
	// Fetch the concepts linked to this fact (the
	// extract_concepts worker's output). The repo ownership check
	// already ran on the sources above, so the concept rows (which
	// are repo-scoped) are safe to return directly — a concept
	// linked to a fact whose sources belong to this repo is, by
	// construction, a concept in this repo. We surface the
	// canonical name, context, slug, and description so the LLM
	// can name the concept and link to its definition without a
	// follow-up call.
	linkedConcepts, err := queries.ListConceptsByFact(ctx, factID)
	if err != nil {
		return mcp.NewToolResultError("failed to list concepts for fact"), nil
	}
	type sourceOut struct {
		URL         string `json:"url"`
		ParsedTitle string `json:"parsed_title,omitempty"`
		FirstSeenAt string `json:"first_seen_at"`
	}
	type conceptOut struct {
		ID            string `json:"id"`
		CanonicalName string `json:"canonical_name"`
		Context       string `json:"context"`
		Description   string `json:"description,omitempty"`
	}
	type factOut struct {
		ID            string      `json:"id"`
		Text          string      `json:"text"`
		Status        string      `json:"status"`
		FactKind      string      `json:"fact_kind"`
		EmbeddedModel string      `json:"embedded_model,omitempty"`
		CreatedAt     string      `json:"created_at"`
		ImageURL      string      `json:"image_url,omitempty"`
	}
	type resultOut struct {
		Fact         factOut      `json:"fact"`
		Sources      []sourceOut  `json:"sources"`
		SourceCount  int          `json:"source_count"`
		Concepts     []conceptOut `json:"concepts"`
		ConceptCount int          `json:"concept_count"`
	}
	srcOut := make([]sourceOut, 0, len(sources))
	for _, s := range sources {
		title := ""
		if s.ParsedTitle != nil {
			title = *s.ParsedTitle
		}
		srcOut = append(srcOut, sourceOut{
			URL:         s.Url,
			ParsedTitle: title,
			FirstSeenAt: pgTimeToString(s.FirstSeenAt),
		})
	}
	imgURL := ""
	if fact.ImageUrl != nil {
		imgURL = *fact.ImageUrl
	}
	embModel := ""
	if fact.EmbeddedModel != nil {
		embModel = *fact.EmbeddedModel
	}
	conceptOuts := make([]conceptOut, 0, len(linkedConcepts))
	for _, c := range linkedConcepts {
		desc := ""
		if c.Description != nil {
			desc = *c.Description
		}
		conceptOuts = append(conceptOuts, conceptOut{
			ID:            c.ID.String(),
			CanonicalName: c.CanonicalName,
			Context:       c.Context,
			Description:   desc,
		})
	}
	return structuredResult(resultOut{
		Fact: factOut{
			ID:            fact.ID.String(),
			Text:          fact.Text,
			Status:        fact.Status,
			FactKind:      fact.FactKind,
			EmbeddedModel: embModel,
			CreatedAt:     pgTimeToString(fact.CreatedAt),
			ImageURL:      imgURL,
		},
		Sources:      srcOut,
		SourceCount:  len(sources),
		Concepts:     conceptOuts,
		ConceptCount: len(linkedConcepts),
	})
}

// resolveConcept resolves a `concept` tool argument (a concept UUID or
// a canonical name) in a repository to its canonical name and the
// list of per-context concept rows for that group. A UUID is looked
// up directly and verified to belong to repoID; a non-UUID is treated
// as a canonical name and the whole group is loaded. Returns the
// canonical name (the group's display representative) and the group
// rows. Returns ok=false and a non-nil error when the concept is not
// found or belongs to a different repo.
func (m *MCP) resolveConcept(ctx context.Context, queries *store.Queries, repoID pgtype.UUID, concept string) (string, []store.ListConceptsByRepoNameRow, error) {
	if concept == "" {
		return "", nil, errors.New("concept is required")
	}
	var conceptID pgtype.UUID
	if err := conceptID.Scan(concept); err == nil {
		// UUID path: load the concept, verify ownership, then load
		// its whole group by canonical name.
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
	// Canonical-name path.
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

// resolveConceptIDs resolves the `concept` + optional `context`
// arguments to an array of concept_ids suitable for the
// ListFactsByRepoWithSourceCountForConcept query. For a UUID, the
// whole group sharing that concept's canonical name is returned
// (unless `context` narrows it, in which case only the matching
// context's id is kept). For a canonical name, the whole group is
// returned, optionally narrowed by `context`.
func (m *MCP) resolveConceptIDs(ctx context.Context, queries *store.Queries, repoID pgtype.UUID, concept, conceptCtx string) ([]pgtype.UUID, error) {
	_, rows, err := m.resolveConcept(ctx, queries, repoID, concept)
	if err != nil {
		return nil, err
	}
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, r := range rows {
		if conceptCtx != "" && r.Context != conceptCtx {
			continue
		}
		ids = append(ids, r.ID)
	}
	if len(ids) == 0 {
		return nil, errors.New("no concept contexts matched the `context` filter")
	}
	return ids, nil
}

// handleSearchConcepts is the searchConcepts tool handler. It lists
// concept groups in a repository, optionally filtered by canonical-
// name substring, paginated by group.
func (m *MCP) handleSearchConcepts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	q := req.GetString("query", "")
	limit := req.GetInt("limit", 50)
	if limit < 1 {
		limit = 1
	}
	if limit > MCPMCPMaxFactsCap {
		limit = MCPMCPMaxFactsCap
	}
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Concepts, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read concepts in this repository"), nil
	}

	queries := store.New(pool)
	rows, err := queries.ListGroupedConceptsByRepo(ctx, store.ListGroupedConceptsByRepoParams{
		RepositoryID: repoID,
		Q:            q,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to list concepts"), nil
	}
	total, err := queries.CountGroupedConceptsByRepo(ctx, store.CountGroupedConceptsByRepoParams{
		RepositoryID: repoID,
		Q:            q,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to count concepts"), nil
	}
	groupRows := make([]concepts.GroupRow, 0, len(rows))
	for _, r := range rows {
		groupRows = append(groupRows, concepts.FromListGroupedConceptsByRepoRow(r))
	}
	groups := concepts.BuildGroups(groupRows, nil)
	page := concepts.Paginate(groups, offset, limit)
	return structuredResult(map[string]any{
		"concepts": page,
		"total":    total,
		"limit":    limit,
		"offset":    offset,
	})
}

// handleGetConcept is the getConcept tool handler. It resolves the
// concept (UUID or canonical name), returns the whole group (every
// context with aliases + fact_count), and the synthesis/definition
// row when one exists.
func (m *MCP) handleGetConcept(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	concept, err := req.RequireString("concept")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Concepts, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read concepts in this repository"), nil
	}

	queries := store.New(pool)
	canonicalName, rows, err := m.resolveConcept(ctx, queries, repoID, concept)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	groupRows := make([]concepts.GroupRow, 0, len(rows))
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, r := range rows {
		gr := concepts.FromListConceptsByRepoNameRow(r)
		groupRows = append(groupRows, gr)
		ids = append(ids, gr.ID)
	}
	aliases, err := concepts.LoadAliasesByConceptID(ctx, queries, ids)
	if err != nil {
		return mcp.NewToolResultError("failed to load concept aliases"), nil
	}
	groups := concepts.BuildGroups(groupRows, aliases)
	if len(groups) == 0 {
		return mcp.NewToolResultError("concept not found"), nil
	}
	group := groups[0]

	// The synthesis/definition is optional; a concept with no
	// synthesis (the worker hasn't run yet) returns null.
	var synthesis *synthesisOut
	syn, err := queries.GetSynthesisByGroup(ctx, store.GetSynthesisByGroupParams{
		RepositoryID:  repoID,
		CanonicalName: canonicalName,
	})
	if err == nil {
		synthesis = &synthesisOut{
			Content: syn.Content,
			Model:   ptrStr(syn.Model),
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return mcp.NewToolResultError("failed to load synthesis"), nil
	}
	return structuredResult(map[string]any{
		"concept":    group,
		"synthesis":  synthesis,
	})
}

// handleGetConceptSummaries is the getConceptSummaries tool handler.
// It resolves the concept (UUID or canonical name), then returns the
// summary slices for every context in the group. For a UUID the
// summaries are fetched directly off that concept_id; for a
// canonical name the whole group's summaries are returned (joined via
// ListSummariesByCanonicalNameGroup) so the LLM sees every context's
// slices.
func (m *MCP) handleGetConceptSummaries(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	concept, err := req.RequireString("concept")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Concepts, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read concepts in this repository"), nil
	}

	queries := store.New(pool)
	canonicalName, _, err := m.resolveConcept(ctx, queries, repoID, concept)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	summaries, err := queries.ListSummariesByCanonicalNameGroup(ctx, store.ListSummariesByCanonicalNameGroupParams{
		RepositoryID:  repoID,
		CanonicalName: canonicalName,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to list summaries"), nil
	}
	out := make([]summaryOut, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, summaryOut{
			ConceptID:   s.ConceptID.String(),
			Context:      s.Context,
			SequenceNum:  s.SequenceNum,
			IsComplete:   s.IsComplete,
			FactCount:    s.FactCount,
			Content:      s.Content,
			Model:        ptrStr(s.Model),
			UpdatedAt:    pgTimeToString(s.UpdatedAt),
		})
	}
	return structuredResult(map[string]any{
		"summaries": out,
		"count":     len(out),
	})
}

// handleGetRelatedConcepts is the getRelatedConcepts tool handler. It
// resolves the concept (UUID or canonical name), then queries the
// concept_relations materialized view for related concept groups
// ranked by shared_fact_count.
func (m *MCP) handleGetRelatedConcepts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	concept, err := req.RequireString("concept")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	limit := req.GetInt("limit", 50)
	if limit < 1 {
		limit = 1
	}
	if limit > MCPMCPMaxFactsCap {
		limit = MCPMCPMaxFactsCap
	}
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Concepts, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read concepts in this repository"), nil
	}

	queries := store.New(pool)
	canonicalName, _, err := m.resolveConcept(ctx, queries, repoID, concept)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rows, err := queries.ListConceptRelationsByConceptName(ctx, store.ListConceptRelationsByConceptNameParams{
		RepositoryID: repoID,
		Lower:        canonicalName,
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		return mcp.NewToolResultError("failed to list related concepts"), nil
	}
	total, err := queries.CountConceptRelationsByConceptName(ctx, store.CountConceptRelationsByConceptNameParams{
		RepositoryID: repoID,
		Lower:        canonicalName,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to count related concepts"), nil
	}
	out := make([]relationOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, relationOut{
			ConceptID:       pgUUIDInterfaceToString(r.ConceptID),
			CanonicalName:   pgUUIDInterfaceToString(r.CanonicalName),
			SharedFactCount: r.SharedFactCount,
		})
	}
	return structuredResult(map[string]any{
		"related":  out,
		"total":    total,
		"limit":    limit,
		"offset":    offset,
	})
}

// handleGetInvestigation is the getInvestigation tool handler. It
// resolves the investigation by UUID, verifies it belongs to the
// resolved repo, and returns the metadata + the sources it
// collects.
func (m *MCP) handleGetInvestigation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	invIDStr, err := req.RequireString("investigationId")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Investigations, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read investigations in this repository"), nil
	}

	var invID pgtype.UUID
	if err := invID.Scan(invIDStr); err != nil {
		return mcp.NewToolResultError("invalid investigationId: " + err.Error()), nil
	}

	queries := store.New(pool)
	inv, err := queries.GetInvestigationByID(ctx, invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mcp.NewToolResultError("investigation not found"), nil
		}
		return mcp.NewToolResultError("failed to get investigation"), nil
	}
	if inv.RepositoryID != repoID {
		return mcp.NewToolResultError("investigation not found"), nil
	}
	sources, err := queries.ListInvestigationSources(ctx, store.ListInvestigationSourcesParams{
		InvestigationID: invID,
		Column2:         "",
		Limit:           200,
		Offset:          0,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to list investigation sources"), nil
	}
	type invSourceOut struct {
		ID          string `json:"id"`
		URL         string `json:"url"`
		ParsedTitle string `json:"parsed_title,omitempty"`
		DOI         string `json:"doi,omitempty"`
		CreatedAt   string `json:"created_at"`
		AddedAt     string `json:"added_at"`
	}
	type invOut struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Topic     string `json:"topic,omitempty"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	srcOut := make([]invSourceOut, 0, len(sources))
	for _, s := range sources {
		title := ""
		if s.ParsedTitle != nil {
			title = *s.ParsedTitle
		}
		doi := ""
		if s.Doi != nil {
			doi = *s.Doi
		}
		srcOut = append(srcOut, invSourceOut{
			ID:          s.ID.String(),
			URL:         s.Url,
			ParsedTitle: title,
			DOI:         doi,
			CreatedAt:   pgTimeToString(s.CreatedAt),
			AddedAt:     pgTimeToString(s.AddedAt),
		})
	}
	topic := ""
	if inv.Topic != nil {
		topic = *inv.Topic
	}
	return structuredResult(map[string]any{
		"investigation": invOut{
			ID:        inv.ID.String(),
			Title:     inv.Title,
			Topic:     topic,
			CreatedAt: pgTimeToString(inv.CreatedAt),
			UpdatedAt: pgTimeToString(inv.UpdatedAt),
		},
		"sources": srcOut,
		"count":   len(srcOut),
	})
}

// handleCreateInvestigation is the createInvestigation tool handler.
// It resolves the repo, checks investigation:create, generates a UUID,
// and inserts the investigation row.
func (m *MCP) handleCreateInvestigation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	topic := req.GetString("topic", "")

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Investigations, rbac.Actions.Write); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to create investigations in this repository"), nil
	}

	var id pgtype.UUID
	if err := id.Scan(uuid.NewString()); err != nil {
		return mcp.NewToolResultError("generating investigation id: " + err.Error()), nil
	}
	var topicPtr *string
	if strings.TrimSpace(topic) != "" {
		t := strings.TrimSpace(topic)
		topicPtr = &t
	}
	queries := store.New(pool)
	inv, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
		ID:           id,
		RepositoryID: repoID,
		Title:        strings.TrimSpace(title),
		Topic:        topicPtr,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to create investigation"), nil
	}
	outTopic := ""
	if inv.Topic != nil {
		outTopic = *inv.Topic
	}
	return structuredResult(map[string]any{
		"investigation": map[string]any{
			"id":         inv.ID.String(),
			"title":      inv.Title,
			"topic":      outTopic,
			"created_at": pgTimeToString(inv.CreatedAt),
			"updated_at": pgTimeToString(inv.UpdatedAt),
		},
	})
}

// handleAddInvestigationSource is the addInvestigationSource tool
// handler. It mirrors the REST Investigations.AddSource endpoint:
// resolves the repo, checks investigation:write, verifies both
// the investigation and the source belong to the resolved repo,
// then inserts the investigation_sources junction row (idempotent
// via ON CONFLICT DO NOTHING). The source must already exist —
// the agent enqueues a fetch with fetchAndProcessSource, polls
// getSourceTasks until the job is complete, then calls this tool
// with the source_id from the job output.
func (m *MCP) handleAddInvestigationSource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	invIDStr, err := req.RequireString("investigationId")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	sourceIDStr, err := req.RequireString("sourceId")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Investigations, rbac.Actions.Write); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to update investigations in this repository"), nil
	}

	var invID pgtype.UUID
	if err := invID.Scan(invIDStr); err != nil {
		return mcp.NewToolResultError("invalid investigationId: " + err.Error()), nil
	}
	var sourceID pgtype.UUID
	if err := sourceID.Scan(sourceIDStr); err != nil {
		return mcp.NewToolResultError("invalid sourceId: " + err.Error()), nil
	}

	queries := store.New(pool)
	// Verify investigation ownership: a cross-repo investigation_id
	// is a 404, not a silent link into another repo's collection.
	inv, err := queries.GetInvestigationByID(ctx, invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mcp.NewToolResultError("investigation not found"), nil
		}
		return mcp.NewToolResultError("failed to get investigation"), nil
	}
	if inv.RepositoryID != repoID {
		return mcp.NewToolResultError("investigation not found"), nil
	}
	// Verify the source belongs to the same repo before linking.
	src, err := queries.GetSourceByID(ctx, sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mcp.NewToolResultError("source not found"), nil
		}
		return mcp.NewToolResultError("failed to get source"), nil
	}
	if src.RepositoryID != repoID {
		return mcp.NewToolResultError("source not found"), nil
	}

	if err := queries.AddInvestigationSource(ctx, store.AddInvestigationSourceParams{
		InvestigationID: invID,
		SourceID:         sourceID,
	}); err != nil {
		return mcp.NewToolResultError("failed to add source to investigation"), nil
	}
	return structuredResult(map[string]any{
		"investigation_id": invIDStr,
		"source_id":        sourceIDStr,
		"linked":           true,
	})
}

// handleFetchAndProcessSource is the fetchAndProcessSource tool
// handler. It resolves the repo, checks source:write, classifies the
// URL/DOI, enqueues a retrieve_source job, and returns the job id +
// classified type. The worker creates the source row and (when
// configured) chains source_decomposition.
func (m *MCP) handleFetchAndProcessSource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	if m.taskEnqueuer == nil {
		return mcp.NewToolResultError("task manager not configured"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	urlArg := req.GetString("url", "")
	doi := req.GetString("doi", "")
	if urlArg == "" && doi == "" {
		return mcp.NewToolResultError("either url or doi is required"), nil
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Sources, rbac.Actions.Write); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to create sources in this repository"), nil
	}

	// Optional investigationId: when supplied, the worker links
	// the fetched source into this investigation once the row
	// exists. Validate it up front so a bad id fails the tool
	// call immediately (the agent gets a synchronous error
	// instead of discovering a silently-skipped link after
	// polling the fetch job). The ownership check mirrors the
	// REST Investigations.AddSource guard.
	investigationID := req.GetString("investigationId", "")
	if investigationID != "" {
		var invID pgtype.UUID
		if err := invID.Scan(investigationID); err != nil {
			return mcp.NewToolResultError("invalid investigationId: " + err.Error()), nil
		}
		queries := store.New(pool)
		inv, gerr := queries.GetInvestigationByID(ctx, invID)
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return mcp.NewToolResultError("investigation not found"), nil
			}
			return mcp.NewToolResultError("failed to get investigation"), nil
		}
		if inv.RepositoryID != repoID {
			return mcp.NewToolResultError("investigation not found"), nil
		}
	}

	resource := fetch.ClassifyURL(urlArg)
	if doi != "" && resource.Type != fetch.SourceDOI {
		resource.Type = fetch.SourceDOI
		resource.Value = doi
		resource.DOI = doi
	}
	jobID, err := m.taskEnqueuer.EnqueueRetrieveSourceFromHTTP(ctx, RetrieveSourceArgs{
		URL:          urlArg,
		RepositoryID: repoID.String(),
		DOI:          doi,
		// Process=true chains the retrieve_source job into
		// source_decomposition once the fetch lands with
		// parseable text, so a single MCP call runs the full
		// retrieve → decompose → embed pipeline. The tool is
		// named fetchAndProcessSource and its description
		// promises "extracts facts, and links them"; without
		// this flag the worker only fetches and the agent would
		// have to call a separate decompose step that does not
		// exist as an MCP tool.
		Process: true,
		// Forward the optional investigationId so the worker
		// links the fetched source into this investigation
		// once persistSource returns the source_id. The
		// investigation was validated above, so the worker's
		// best-effort link is expected to succeed; a race
		// (investigation deleted between validate and link)
		// is logged and swallowed rather than failing the
		// fetch.
		InvestigationID: investigationID,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to enqueue fetch: " + err.Error()), nil
	}
	return structuredResult(map[string]any{
		"job_id":        jobID,
		"classified_as": resource.Type,
		"value":         resource.Value,
		"status":        "queued",
	})
}

// handleGetSourceTasks is the getSourceTasks tool handler. It resolves
// the repo, checks task:read, then lists River jobs filtered by the
// repo's metadata. Scope is one of: repo-wide (no filter), a single
// source (sourceId → metadata {repo_id, source_id}), or an
// investigation's sources (investigationId → resolve source_ids via
// ListInvestigationSources, then filter the repo-wide job page to
// those sources client-side — jobs carry source_id in metadata, not
// investigation_id, because a source may belong to multiple
// investigations).
//
// The tool has two output modes:
//   - verbose=false (default): a compact progress summary —
//     counts_by_state, counts_by_kind, pending_count, running_count,
//     and `complete` (true when no non-finalized jobs are on the page
//     AND the page isn't full, which would imply more jobs beyond
//     it). This is the drain signal agents poll until complete=true.
//   - verbose=true: the per-job row list (paginated via cursor/limit).
//
// State and kind filters narrow both modes. Pagination (limit/cursor)
// applies to both; the 50-row default cap is raised to a 200 max so a
// busy repo's job list isn't silently truncated.
func (m *MCP) handleGetSourceTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	// The summary path needs taskPool (SQL GROUP BY); the verbose
	// path needs taskClient (River JobList). Require whichever
	// matches the requested mode, falling back to taskClient for
	// the legacy nil-pool summary path.
	verbose := req.GetBool("verbose", false)
	byKind := req.GetBool("byKind", false)
	if !verbose && m.taskPool != nil {
		// global summary path — taskPool is enough
	} else if m.taskClient == nil {
		return mcp.NewToolResultError("task manager not configured"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	sourceID := req.GetString("sourceId", "")
	investigationID := req.GetString("investigationId", "")
	if sourceID != "" && investigationID != "" {
		return mcp.NewToolResultError("sourceId and investigationId are mutually exclusive"), nil
	}
	stateFilter := req.GetString("state", "")
	kindFilter := req.GetString("kind", "")
	limit := req.GetInt("limit", 50)
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	cursorStr := req.GetString("cursor", "")

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Tasks, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read tasks in this repository"), nil
	}

	// Investigation scope: resolve the investigation's source_ids
	// now so we can filter the repo-wide job page to them. A source
	// may belong to multiple investigations, so jobs carry
	// source_id (not investigation_id) in metadata; the
	// investigation→sources→jobs expansion happens here, not in
	// the worker. The investigation must belong to the resolved
	// repo (cross-repo is a 404-equivalent tool error).
	var invSourceIDs map[string]bool
	if investigationID != "" {
		var invID pgtype.UUID
		if err := invID.Scan(investigationID); err != nil {
			return mcp.NewToolResultError("invalid investigationId: " + err.Error()), nil
		}
		queries := store.New(pool)
		inv, gerr := queries.GetInvestigationByID(ctx, invID)
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return mcp.NewToolResultError("investigation not found"), nil
			}
			return mcp.NewToolResultError("failed to get investigation"), nil
		}
		if inv.RepositoryID != repoID {
			return mcp.NewToolResultError("investigation not found"), nil
		}
		invSources, lerr := queries.ListInvestigationSources(ctx, store.ListInvestigationSourcesParams{
			InvestigationID: invID,
			Column2:         "",
			Limit:           1000,
			Offset:          0,
		})
		if lerr != nil {
			return mcp.NewToolResultError("failed to list investigation sources"), nil
		}
		invSourceIDs = make(map[string]bool, len(invSources))
		for _, s := range invSources {
			invSourceIDs[s.ID.String()] = true
		}
	}

	// Validate the state filter up front so BOTH paths fail loudly
	// on an unknown state name. The global summary ignores state
	// and kind filters (it reports every state/kind in one query),
	// but it still rejects a bogus state to keep the contract
	// consistent with the verbose path.
	if stateFilter != "" {
		if _, ok := riverJobState(stateFilter); !ok {
			return mcp.NewToolResultError("unknown state: " + stateFilter), nil
		}
	}

	// Default mode (verbose=false): serve a GLOBAL summary via a
	// single SQL GROUP BY on river_job. The agent gets counts for
	// every state, every kind, every page in one round-trip — no
	// paging through cursors to accumulate a per-page picture, and
	// `complete` is globally trustworthy (no complete_unreliable).
	// Falls back to the legacy per-page aggregation when taskPool
	// is nil (misconfigured deployment; both are wired together in
	// production).
	if !verbose && m.taskPool != nil {
		scope := taskScopeParams{
			repoID:   repoIDString(repoID),
			sourceID: sourceID,
			reportID: "",
		}
		if invSourceIDs != nil {
			scope.invSrcIDs = make([]string, 0, len(invSourceIDs))
			for sid := range invSourceIDs {
				scope.invSrcIDs = append(scope.invSrcIDs, sid)
			}
		}
		return m.sourceTasksGlobalSummary(ctx, scope, byKind)
	}

	// Verbose mode (or legacy fallback when taskPool is nil):
	// paginated River JobList with per-page summary/verbose output.
	// The agent uses the drain protocol (page through every cursor
	// until next_cursor is empty AND pending_count==0) to confirm
	// the pipeline drained.

	// Build the River metadata-containment filter. sourceId narrows
	// the query directly; investigationId is resolved client-side
	// (above) after the repo-wide page returns. repo-wide is the
	// default.
	meta := map[string]string{"repo_id": repoIDString(repoID)}
	if sourceID != "" {
		meta["source_id"] = sourceID
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return mcp.NewToolResultError("encoding metadata filter: " + err.Error()), nil
	}
	params := river.NewJobListParams().Metadata(string(metaJSON))
	params = params.OrderBy(river.JobListOrderByID, river.SortOrderDesc)
	params = params.First(limit)

	// Optional state filter (validated above).
	if stateFilter != "" {
		st, _ := riverJobState(stateFilter)
		params = params.States(st)
	}
	// Optional kind filter.
	if kindFilter != "" {
		params = params.Kinds(kindFilter)
	}
	// Optional pagination cursor (opaque base64-ish blob from a
	// prior response's next_cursor). The cursor is the string form
	// of the marshaled JobListCursor; we demarshal it back so River
	// can resume. A bad cursor is a tool error.
	if cursorStr != "" {
		cur, cerr := decodeJobListCursor(cursorStr)
		if cerr != nil {
			return mcp.NewToolResultError("invalid cursor: " + cerr.Error()), nil
		}
		params = params.After(cur)
	}

	result, err := m.taskClient.JobList(ctx, params)
	if err != nil {
		return mcp.NewToolResultError("failed to list jobs: " + err.Error()), nil
	}

	// Investigation scope: filter the repo-wide page to the
	// investigation's source_ids. A job is kept when its metadata
	// source_id is in the investigation's set, OR when it's a
	// retrieve_source job (which carries only repo_id, no
	// source_id) — but retrieve_source jobs aren't reliably
	// attributable to an investigation without the source_id, so
	// they're dropped here. The downstream chain (source_decomp
	// onward) carries source_id and is kept. This is the known
	// limitation of investigation-scoping without metadata changes;
	// agents can still see retrieve_source via the source-scoped or
	// repo-wide call.
	jobs := result.Jobs
	if invSourceIDs != nil {
		filtered := make([]*rivertype.JobRow, 0, len(jobs))
		for _, j := range jobs {
			sid := jobMetadataSourceID(j.Metadata)
			if sid != "" && invSourceIDs[sid] {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}

	// Next cursor for pagination. River returns LastCursor only when
	// the raw page is full (more rows likely exist beyond it); nil
	// means this is the last page. IMPORTANT: base this on the RAW
	// result.Jobs, NOT the post-filter `jobs` slice. When an
	// investigation filter drops most of the rows on a page (e.g. 50
	// raw → 10 matching the investigation), the filtered length no
	// longer reflects whether more raw pages exist. Using the
	// filtered length here would suppress the cursor and let the
	// summary report complete=true while hundreds of pending jobs
	// still sit on later pages — the root cause of premature
	// "ingested" reports on large investigations. The caller must
	// page through every cursor (regardless of scope) until the raw
	// page is not full AND no pending jobs remain on any page.
	nextCursor := ""
	rawPageFull := len(result.Jobs) == limit
	if result.LastCursor != nil && rawPageFull {
		nextCursor = encodeJobListCursor(result.LastCursor)
	}

	// completeUnreliable: when the caller applied a state filter that
	// excludes ALL non-finalized states (e.g. state=completed), the
	// pending_count on every page is structurally 0 — it can never
	// reflect whether pending jobs exist in other states on later
	// pages. In that case `complete` MUST NOT be trusted as a drain
	// signal; the response carries complete_unreliable=true so the
	// agent knows to re-poll unfiltered before declaring ingested.
	// Similarly a kind filter narrows to one stage of the pipeline,
	// so it too can't confirm the whole pipeline drained.
	completeUnreliable := filterHidesPending(stateFilter, kindFilter)

	if verbose {
		return m.sourceTasksVerboseResult(jobs, nextCursor, rawPageFull)
	}
	return m.sourceTasksSummaryResult(jobs, nextCursor, limit, rawPageFull, completeUnreliable, byKind)
}

// filterHidesPending reports whether the supplied state/kind filters
// are too narrow for the summary's `complete` flag to be trusted as a
// drain signal. A state filter that names only finalized states
// (completed/cancelled/discarded) structurally zeroes pending_count,
// and any kind filter restricts the view to a single pipeline stage.
// In either case the agent must re-poll unfiltered to confirm drain.
func filterHidesPending(stateFilter, kindFilter string) bool {
	if kindFilter != "" {
		return true
	}
	if stateFilter == "" {
		return false
	}
	st, ok := riverJobState(stateFilter)
	if !ok {
		return false
	}
	return !isRiverJobPending(string(st))
}

// sourceTasksVerboseResult builds the per-job row list response.
// nextCursor is only non-empty when rawPageFull is true (the raw
// River page filled to limit, so more rows likely exist beyond it);
// when the page is partial, River still sets LastCursor for the last
// row, but emitting it would invite a useless trailing fetch that
// returns an empty list. This mirrors the summary path's guard so
// verbose and summary modes paginate consistently.
func (m *MCP) sourceTasksVerboseResult(jobs []*rivertype.JobRow, nextCursor string, rawPageFull bool) (*mcp.CallToolResult, error) {
	type taskOut struct {
		ID          int64  `json:"id"`
		Kind        string `json:"kind"`
		State       string `json:"state"`
		Attempt     int    `json:"attempt"`
		CreatedAt   string `json:"created_at"`
		FinalizedAt string `json:"finalized_at,omitempty"`
	}
	out := make([]taskOut, 0, len(jobs))
	for _, j := range jobs {
		finalized := ""
		if j.FinalizedAt != nil {
			finalized = pgTimeToRFC3339Ptr(j.FinalizedAt)
		}
		out = append(out, taskOut{
			ID:          j.ID,
			Kind:        j.Kind,
			State:       string(j.State),
			Attempt:     j.Attempt,
			CreatedAt:   pgTimeToRFC3339(j.CreatedAt),
			FinalizedAt: finalized,
		})
	}
	// Only surface a cursor when the raw page was full; otherwise
	// the caller would chase an empty next page.
	cursor := nextCursor
	if !rawPageFull {
		cursor = ""
	}
	return structuredResult(map[string]any{
		"tasks":       out,
		"count":       len(out),
		"next_cursor": cursor,
	})
}

// sourceTasksSummaryResult builds the compact progress summary. It
// aggregates the (already-scope-filtered) job page into counts by
// state (and, when byKind is true, by kind), then derives
// pending_count (non-finalized), running_count, and `complete`.
// `complete` is true only when the page contains zero non-finalized
// jobs AND the raw page wasn't full (a full raw page means more jobs
// exist beyond it that could be pending, so we can't claim drained).
// rawPageFull is distinct from len(jobs)==limit when a scope filter
// dropped rows from a full raw page — see handleGetSourceTasks for
// why this matters. The agent must page through every next_cursor
// until rawPageFull is false AND pending_count is zero on the last
// page.
//
// completeUnreliable, when true, signals that the caller applied a
// state/kind filter too narrow for `complete` to be a trustworthy
// drain signal (e.g. state=completed zeroes pending_count by
// construction; any kind filter restricts to one pipeline stage).
// In that case complete is forced false and the response carries
// complete_unreliable=true so the agent re-polls unfiltered before
// declaring the pipeline drained. This prevents the failure mode
// where a state-filtered poll reports pending_count=0 and the agent
// falsely concludes ingestion is done while pending jobs sit on
// later pages or in unfiltered states.
//
// byKind gates counts_by_kind (the per-page path never produced
// counts_by_kind_and_state — that's a global-summary-only field);
// byKind=false omits counts_by_kind from the response, matching the
// global summary's compact default.
func (m *MCP) sourceTasksSummaryResult(jobs []*rivertype.JobRow, nextCursor string, limit int, rawPageFull bool, completeUnreliable bool, byKind bool) (*mcp.CallToolResult, error) {
	countsByState := map[string]int{}
	var countsByKind map[string]int
	if byKind {
		countsByKind = map[string]int{}
	}
	pendingCount := 0
	runningCount := 0
	for _, j := range jobs {
		st := string(j.State)
		countsByState[st]++
		if byKind {
			countsByKind[j.Kind]++
		}
		if isRiverJobPending(st) {
			pendingCount++
			if st == string(rivertype.JobStateRunning) {
				runningCount++
			}
		}
	}
	// complete = no pending jobs on this page AND the raw page
	// wasn't full (a full raw page means more jobs exist beyond it
	// that could be pending). Use rawPageFull, NOT len(jobs)==limit,
	// because a scope filter may have shrunk a full raw page to a
	// smaller filtered slice — that doesn't mean we've seen all
	// jobs. When rawPageFull is true we return next_cursor so the
	// agent can page to confirm; only when the final page isn't
	// full AND pending_count==0 is complete true.
	//
	// When completeUnreliable is true (narrow state/kind filter),
	// force complete=false regardless: pending_count==0 under such
	// a filter does NOT mean the pipeline drained, because the
	// filter hid the pending jobs. The agent must re-poll
	// unfiltered to get a trustworthy complete signal.
	complete := pendingCount == 0 && !rawPageFull && !completeUnreliable
	result := map[string]any{
		"verbose":         false,
		"counts_by_state": countsByState,
		"pending_count":   pendingCount,
		"running_count":   runningCount,
		"complete":        complete,
		"next_cursor":     nextCursor,
		"page_size":       len(jobs),
	}
	if byKind {
		result["counts_by_kind"] = countsByKind
	}
	if completeUnreliable {
		result["complete_unreliable"] = true
	}
	return structuredResult(result)
}

// taskScopeParams is the scoping contract shared by the global
// summary and the verbose paginated path. It captures the four
// independent filters the two task tools (getSourceTasks and
// getReportTasks) support: repo_id (always), a single source_id
// OR a slice of investigation source_ids OR a report_id (at most
// one of those three). The global summary builds a SQL metadata
// containment fragment + an optional source_id ANY-array from
// these; the verbose path builds River JobListParams from them.
type taskScopeParams struct {
	repoID   string   // always set; metadata @> {"repo_id": ...}
	sourceID string   // single-source scope; "" = none
	invSrcIDs []string // investigation scope (resolved source_ids); nil = none
	reportID string   // report scope; "" = none
}

// sourceTasksGlobalSummary runs a single SQL GROUP BY against
// river_job on the task pool and returns GLOBAL counts (every
// state, every kind, every page) in one round-trip. This replaces
// the legacy per-page client-side aggregation for the default
// (verbose=false) mode: the agent no longer pages through cursors
// to accumulate a picture of the backlog — one call shows the
// whole scope.
//
// The query uses the same metadata-containment filter River's
// JobList uses (metadata @> fragment::jsonb), plus an optional
// source_id ANY-array for investigation scope (a source may belong
// to multiple investigations; jobs carry source_id, not
// investigation_id, in metadata). retrieve_source jobs (no
// source_id) are dropped under investigation scope, matching the
// verbose path's known limitation.
//
// state/kind filters are intentionally IGNORED in summary mode:
// they are inspection-only filters (per the tool description) and
// the global query already reports every state and, under byKind,
// every kind, so applying them would only hide information. The
// agent that wants a single state or kind reads the relevant key
// from counts_by_state (always) or counts_by_kind /
// counts_by_kind_and_state (byKind=true).
//
// `complete` is GLOBALLY trustworthy here (pending_count == 0 means
// the whole scope is drained), so there is no complete_unreliable
// flag and no drain protocol. The legacy per-page summary couldn't
// trust complete because a single page can't see the whole backlog;
// the SQL query can.
//
// byKind controls whether counts_by_kind and counts_by_kind_and_state
// are populated and emitted. The default (byKind=false) returns only
// counts_by_state + pending_count + running_count + total + complete
// — the compact drain-poll payload. byKind=true adds the per-kind
// breakdown for callers that need to see which pipeline stage is
// stuck. The SQL always groups by (kind, state); byKind only gates
// the map writes and the response keys, not the query.
func (m *MCP) sourceTasksGlobalSummary(ctx context.Context, scope taskScopeParams, byKind bool) (*mcp.CallToolResult, error) {
	// Build the metadata containment fragment. Always carries
	// repo_id; add source_id (single-source scope) or report_id
	// (report scope) when set. Investigation scope is handled via
	// the ANY-array, not the fragment.
	meta := map[string]string{"repo_id": scope.repoID}
	if scope.sourceID != "" {
		meta["source_id"] = scope.sourceID
	}
	if scope.reportID != "" {
		meta["report_id"] = scope.reportID
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return mcp.NewToolResultError("encoding metadata filter: " + err.Error()), nil
	}

	// Investigation scope: pass the resolved source_ids as a
	// text[] so the query can match metadata->>'source_id' =
	// ANY($2). nil means "no investigation filter" (the ANY
	// clause is skipped via $2::text[] IS NULL). An empty slice
	// means the investigation has no sources yet — the query
	// must return zero rows, so we pass a non-null array
	// containing a sentinel that matches nothing ('').
	var invArr any
	if scope.invSrcIDs != nil {
		if len(scope.invSrcIDs) == 0 {
			invArr = []string{""}
		} else {
			invArr = scope.invSrcIDs
		}
	}

	rows, err := m.taskPool.Query(ctx, `
		SELECT kind, state, count(*)::bigint AS count
		FROM river_job
		WHERE metadata @> $1::jsonb
		  AND ($2::text[] IS NULL OR metadata->>'source_id' = ANY($2))
		GROUP BY kind, state`,
		metaJSON, invArr)
	if err != nil {
		return mcp.NewToolResultError("failed to aggregate tasks: " + err.Error()), nil
	}
	defer rows.Close()

	countsByState := map[string]int{}
	var countsByKind map[string]int
	var countsByKindAndState map[string]map[string]int
	if byKind {
		countsByKind = map[string]int{}
		countsByKindAndState = map[string]map[string]int{}
	}
	pendingCount := 0
	runningCount := 0
	total := 0
	for rows.Next() {
		var kind, state string
		var count int64
		if err := rows.Scan(&kind, &state, &count); err != nil {
			return mcp.NewToolResultError("scanning task aggregate: " + err.Error()), nil
		}
		c := int(count)
		countsByState[state] += c
		if byKind {
			countsByKind[kind] += c
			if countsByKindAndState[kind] == nil {
				countsByKindAndState[kind] = map[string]int{}
			}
			countsByKindAndState[kind][state] += c
		}
		total += c
		if isRiverJobPending(state) {
			pendingCount += c
			if state == string(rivertype.JobStateRunning) {
				runningCount += c
			}
		}
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterating task aggregate: " + err.Error()), nil
	}

	result := map[string]any{
		"verbose":          false,
		"counts_by_state":  countsByState,
		"pending_count":    pendingCount,
		"running_count":    runningCount,
		"complete":         pendingCount == 0,
		"total":            total,
	}
	if byKind {
		result["counts_by_kind"]           = countsByKind
		result["counts_by_kind_and_state"] = countsByKindAndState
	}
	return structuredResult(result)
}
// metadata blob. Returns "" when the metadata is absent or carries
// no source_id (e.g. retrieve_source, repo-wide jobs). Used by the
// investigation-scoped getSourceTasks filter.
func jobMetadataSourceID(metadata []byte) string {
	if len(metadata) == 0 {
		return ""
	}
	var m struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal(metadata, &m); err != nil {
		return ""
	}
	return m.SourceID
}

// isRiverJobPending reports whether a River job state is non-finalized
// (still running or waiting to run). Finalized states are completed,
// cancelled, discarded.
func isRiverJobPending(state string) bool {
	switch rivertype.JobState(state) {
	case rivertype.JobStateAvailable,
		rivertype.JobStateRunning,
		rivertype.JobStateRetryable,
		rivertype.JobStatePending,
		rivertype.JobStateScheduled:
		return true
	}
	return false
}

// riverJobState maps a state name string to its rivertype constant.
// Returns ok=false for an unrecognized name so the caller can surface
// a tool error instead of silently ignoring a bad filter.
func riverJobState(name string) (rivertype.JobState, bool) {
	switch name {
	case string(rivertype.JobStateAvailable):
		return rivertype.JobStateAvailable, true
	case string(rivertype.JobStateRunning):
		return rivertype.JobStateRunning, true
	case string(rivertype.JobStateRetryable):
		return rivertype.JobStateRetryable, true
	case string(rivertype.JobStatePending):
		return rivertype.JobStatePending, true
	case string(rivertype.JobStateScheduled):
		return rivertype.JobStateScheduled, true
	case string(rivertype.JobStateCompleted):
		return rivertype.JobStateCompleted, true
	case string(rivertype.JobStateCancelled):
		return rivertype.JobStateCancelled, true
	case string(rivertype.JobStateDiscarded):
		return rivertype.JobStateDiscarded, true
	}
	return "", false
}

// encodeJobListCursor serializes a River JobListCursor into an opaque
// string the caller can round-trip back via the `cursor` arg. Uses
// base64-encoded JSON so the cursor stays URL-safe and opaque (the
// caller never inspects it).
func encodeJobListCursor(c *river.JobListCursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// decodeJobListCursor reverses encodeJobListCursor.
func decodeJobListCursor(s string) (*river.JobListCursor, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	var c river.JobListCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return &c, nil
}

// handleCreateReport is the createReport tool handler. It resolves
// the repo, checks report:create, inserts the report row in `pending`
// status (optionally under a parent), reparents any children_ids onto
// the new report, enqueues an annotate_report job, and returns the
// report id + job id so the caller can poll getReport/getReportTasks.
func (m *MCP) handleCreateReport(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	if m.taskEnqueuer == nil {
		return mcp.NewToolResultError("task manager not configured"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	topic := req.GetString("topic", "")
	parentIDStr := strings.TrimSpace(req.GetString("parentId", ""))
	childrenIDs := req.GetStringSlice("childrenIds", nil)

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Reports, rbac.Actions.Write); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to create reports in this repository"), nil
	}

	var id pgtype.UUID
	if err := id.Scan(uuid.NewString()); err != nil {
		return mcp.NewToolResultError("generating report id: " + err.Error()), nil
	}
	var topicPtr *string
	if strings.TrimSpace(topic) != "" {
		t := strings.TrimSpace(topic)
		topicPtr = &t
	}

	queries := store.New(pool)

	var parentID pgtype.UUID
	if parentIDStr != "" {
		if err := parentID.Scan(parentIDStr); err != nil {
			return mcp.NewToolResultError("invalid parentId: " + err.Error()), nil
		}
		// childID is the new report (not yet inserted), so the cycle
		// check is skipped inside validateReportParent (zero-value
		// childID); only existence + same-repo is verified.
		if err := validateReportParent(ctx, queries, repoID, pgtype.UUID{}, parentID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	report, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           id,
		RepositoryID: repoID,
		Title:        strings.TrimSpace(title),
		Topic:        topicPtr,
		BodyMd:       text,
		Status:       "pending",
		ParentID:     parentID,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to create report"), nil
	}

	if len(childrenIDs) > 0 {
		if err := reparentChildren(ctx, queries, repoID, report.ID, childrenIDs); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	jobID, err := m.taskEnqueuer.EnqueueAnnotateReportFromHTTP(ctx, AnnotateReportArgs{
		ReportID:     report.ID.String(),
		RepositoryID: repoID.String(),
	})
	if err != nil {
		return mcp.NewToolResultError("failed to enqueue annotation: " + err.Error()), nil
	}
	return structuredResult(map[string]any{
		"report_id": report.ID.String(),
		"job_id":    jobID,
		"status":    "queued",
	})
}

// handleUpdateReport is the updateReport tool handler. It resolves
// the report, checks report:update, optionally reparents it / sets
// its children, updates title/topic/body_md, and re-enqueues
// annotation when the body changed. Returns the updated report row.
func (m *MCP) handleUpdateReport(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reportIDStr, err := req.RequireString("reportId")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	topic := req.GetString("topic", "")
	parentIDRaw, hasParent := req.GetArguments()["parentId"]
	childrenIDs := req.GetStringSlice("childrenIds", nil)

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Reports, rbac.Actions.Update); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to update reports in this repository"), nil
	}

	var reportID pgtype.UUID
	if err := reportID.Scan(reportIDStr); err != nil {
		return mcp.NewToolResultError("invalid reportId: " + err.Error()), nil
	}

	queries := store.New(pool)
	existing, err := queries.GetReportByID(ctx, reportID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mcp.NewToolResultError("report not found"), nil
		}
		return mcp.NewToolResultError("failed to get report"), nil
	}
	if existing.RepositoryID != repoID {
		return mcp.NewToolResultError("report not found"), nil
	}

	// Reparent this report when parentId was supplied. An explicit
	// empty string clears the parent; a non-empty value sets it
	// (with cycle detection).
	if hasParent {
		parentIDStr, _ := parentIDRaw.(string)
		parentIDStr = strings.TrimSpace(parentIDStr)
		if parentIDStr == "" {
			if err := queries.SetReportsParent(ctx, store.SetReportsParentParams{
				ParentID:     pgtype.UUID{},
				Column2:      []pgtype.UUID{reportID},
				RepositoryID: repoID,
			}); err != nil {
				return mcp.NewToolResultError("failed to clear parent: " + err.Error()), nil
			}
		} else {
			var newParentID pgtype.UUID
			if err := newParentID.Scan(parentIDStr); err != nil {
				return mcp.NewToolResultError("invalid parentId: " + err.Error()), nil
			}
			if err := validateReportParent(ctx, queries, repoID, reportID, newParentID); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := queries.SetReportsParent(ctx, store.SetReportsParentParams{
				ParentID:     newParentID,
				Column2:      []pgtype.UUID{reportID},
				RepositoryID: repoID,
			}); err != nil {
				return mcp.NewToolResultError("failed to reparent report: " + err.Error()), nil
			}
		}
	}

	if len(childrenIDs) > 0 {
		if err := reparentChildren(ctx, queries, repoID, reportID, childrenIDs); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	var topicPtr *string
	if strings.TrimSpace(topic) != "" {
		t := strings.TrimSpace(topic)
		topicPtr = &t
	}
	updated, err := queries.UpdateReport(ctx, store.UpdateReportParams{
		ID:     reportID,
		Title:  strings.TrimSpace(title),
		Topic:  topicPtr,
		BodyMd: text,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to update report"), nil
	}

	bodyChanged := updated.BodyMd != existing.BodyMd
	if bodyChanged && m.taskEnqueuer != nil {
		jobID, jerr := m.taskEnqueuer.EnqueueAnnotateReportFromHTTP(ctx, AnnotateReportArgs{
			ReportID:     updated.ID.String(),
			RepositoryID: repoID.String(),
		})
		if jerr != nil {
			return mcp.NewToolResultError("failed to enqueue re-annotation: " + jerr.Error()), nil
		}
		jobIDPtr := jobID
		_ = queries.MarkReportStatus(ctx, store.MarkReportStatusParams{
			ID:              reportID,
			Status:          "pending",
			AnnotationJobID: &jobIDPtr,
		})
	}

	parentID := ""
	if updated.ParentID.Valid {
		parentID = updated.ParentID.String()
	}
	topicOut := ""
	if updated.Topic != nil {
		topicOut = *updated.Topic
	}
	return structuredResult(map[string]any{
		"report": map[string]any{
			"id":         updated.ID.String(),
			"title":      updated.Title,
			"topic":      topicOut,
			"body_md":    updated.BodyMd,
			"status":     updated.Status,
			"parent_id":  parentID,
			"created_at": pgTimeToString(updated.CreatedAt),
			"updated_at": pgTimeToString(updated.UpdatedAt),
		},
	})
}

// handleGetReport is the getReport tool handler. It resolves the
// report by UUID, verifies it belongs to the resolved repo, and
// returns the report metadata + annotations (each with the matched
// fact's text, status, source_count, and score).
func (m *MCP) handleGetReport(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reportIDStr, err := req.RequireString("reportId")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Reports, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read reports in this repository"), nil
	}

	var reportID pgtype.UUID
	if err := reportID.Scan(reportIDStr); err != nil {
		return mcp.NewToolResultError("invalid reportId: " + err.Error()), nil
	}

	queries := store.New(pool)
	report, err := queries.GetReportByID(ctx, reportID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mcp.NewToolResultError("report not found"), nil
		}
		return mcp.NewToolResultError("failed to get report"), nil
	}
	if report.RepositoryID != repoID {
		return mcp.NewToolResultError("report not found"), nil
	}

	annotations, err := queries.ListReportAnnotationsByReport(ctx, reportID)
	if err != nil {
		return mcp.NewToolResultError("failed to list report annotations"), nil
	}

	type annotationOut struct {
		SentenceIndex int     `json:"sentence_index"`
		SentenceText  string  `json:"sentence_text"`
		FactID        string  `json:"fact_id"`
		Score         float64 `json:"score"`
		Posture       string  `json:"posture,omitempty"`
		FactText      string  `json:"fact_text"`
		FactStatus    string `json:"fact_status"`
		FactKind      string `json:"fact_kind"`
		SourceCount   int64   `json:"source_count"`
	}
	type reportOut struct {
		ID                  string  `json:"id"`
		Title               string  `json:"title"`
		Topic               string  `json:"topic,omitempty"`
		BodyMd              string  `json:"body_md"`
		Status              string  `json:"status"`
		Error               string  `json:"error,omitempty"`
		ParentID            string  `json:"parent_id,omitempty"`
		SentenceCount       int     `json:"sentence_count,omitempty"`
		SimilarityThreshold float64 `json:"similarity_threshold,omitempty"`
		EmbeddedModel       string  `json:"embedded_model,omitempty"`
		AnnotationJobID     string  `json:"annotation_job_id,omitempty"`
		CreatedAt           string  `json:"created_at"`
		UpdatedAt           string  `json:"updated_at"`
	}
	annOut := make([]annotationOut, 0, len(annotations))
	for _, a := range annotations {
		posture := ""
		if a.Posture != nil {
			posture = *a.Posture
		}
		annOut = append(annOut, annotationOut{
			SentenceIndex: int(a.SentenceIndex),
			SentenceText:  a.SentenceText,
			FactID:        a.FactID.String(),
			Score:         a.Score,
			Posture:       posture,
			FactText:      a.Text,
			FactStatus:    a.Status,
			FactKind:      a.FactKind,
			SourceCount:   a.SourceCount,
		})
	}
	topic := ""
	if report.Topic != nil {
		topic = *report.Topic
	}
	errStr := ""
	if report.Error != nil {
		errStr = *report.Error
	}
	sentenceCount := 0
	if report.SentenceCount != nil {
		sentenceCount = int(*report.SentenceCount)
	}
	threshold := 0.0
	if report.SimilarityThreshold != nil {
		threshold = *report.SimilarityThreshold
	}
	embModel := ""
	if report.EmbeddedModel != nil {
		embModel = *report.EmbeddedModel
	}
	jobID := ""
	if report.AnnotationJobID != nil {
		jobID = *report.AnnotationJobID
	}
	parentID := ""
	if report.ParentID.Valid {
		parentID = report.ParentID.String()
	}
	return structuredResult(map[string]any{
		"report": reportOut{
			ID:                  report.ID.String(),
			Title:               report.Title,
			Topic:               topic,
			BodyMd:              report.BodyMd,
			Status:              report.Status,
			Error:               errStr,
			ParentID:            parentID,
			SentenceCount:       sentenceCount,
			SimilarityThreshold: threshold,
			EmbeddedModel:       embModel,
			AnnotationJobID:     jobID,
			CreatedAt:           pgTimeToString(report.CreatedAt),
			UpdatedAt:           pgTimeToString(report.UpdatedAt),
		},
		"annotations": annOut,
		"count":        len(annOut),
	})
}

// handleGetReportTasks is the getReportTasks tool handler. It
// resolves the repo, checks report:read, then lists River jobs
// filtered by the repo's metadata (and optionally by report_id when
// reportId is given). Mirrors handleGetSourceTasks: same pagination
// (limit/cursor, 1-200 page size), same verbose/summary modes, same
// state/kind filters, and the same complete/complete_unreliable
// drain signals. See handleGetSourceTasks for the rationale behind
// rawPageFull (based on the RAW result, not a post-filter slice) and
// completeUnreliable (state/kind filters narrow the view so
// pending_count can't confirm drain).
func (m *MCP) handleGetReportTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	verbose := req.GetBool("verbose", false)
	byKind := req.GetBool("byKind", false)
	// The summary path needs taskPool (SQL GROUP BY); the verbose
	// path needs taskClient (River JobList). Require whichever
	// matches the requested mode, falling back to taskClient for
	// the legacy nil-pool summary path.
	if !verbose && m.taskPool != nil {
		// global summary path — taskPool is enough
	} else if m.taskClient == nil {
		return mcp.NewToolResultError("task manager not configured"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reportID := req.GetString("reportId", "")
	stateFilter := req.GetString("state", "")
	kindFilter := req.GetString("kind", "")
	limit := req.GetInt("limit", 50)
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	cursorStr := req.GetString("cursor", "")

	repoID, _, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Reports, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read reports in this repository"), nil
	}

	// Validate state filter up front (both paths reject a bogus
	// state name with the same error, matching handleGetSourceTasks).
	if stateFilter != "" {
		if _, ok := riverJobState(stateFilter); !ok {
			return mcp.NewToolResultError("unknown state: " + stateFilter), nil
		}
	}

	// Default mode (verbose=false): serve a GLOBAL summary via a
	// single SQL GROUP BY on river_job, scoped by repo_id (and
	// optionally report_id). `complete` is globally trustworthy.
	// Falls back to the legacy per-page aggregation when taskPool
	// is nil.
	if !verbose && m.taskPool != nil {
		return m.sourceTasksGlobalSummary(ctx, taskScopeParams{
			repoID:   repoIDString(repoID),
			reportID: reportID,
		}, byKind)
	}

	meta := map[string]string{"repo_id": repoIDString(repoID)}
	if reportID != "" {
		meta["report_id"] = reportID
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return mcp.NewToolResultError("encoding metadata filter: " + err.Error()), nil
	}
	params := river.NewJobListParams().Metadata(string(metaJSON))
	params = params.OrderBy(river.JobListOrderByID, river.SortOrderDesc)
	params = params.First(limit)

	// Optional state filter (validated above).
	if stateFilter != "" {
		st, _ := riverJobState(stateFilter)
		params = params.States(st)
	}
	if kindFilter != "" {
		params = params.Kinds(kindFilter)
	}
	if cursorStr != "" {
		cur, cerr := decodeJobListCursor(cursorStr)
		if cerr != nil {
			return mcp.NewToolResultError("invalid cursor: " + cerr.Error()), nil
		}
		params = params.After(cur)
	}

	result, err := m.taskClient.JobList(ctx, params)
	if err != nil {
		return mcp.NewToolResultError("failed to list jobs: " + err.Error()), nil
	}

	jobs := result.Jobs
	nextCursor := ""
	rawPageFull := len(result.Jobs) == limit
	if result.LastCursor != nil && rawPageFull {
		nextCursor = encodeJobListCursor(result.LastCursor)
	}

	completeUnreliable := filterHidesPending(stateFilter, kindFilter)

	if verbose {
		return m.sourceTasksVerboseResult(jobs, nextCursor, rawPageFull)
	}
	return m.sourceTasksSummaryResult(jobs, nextCursor, limit, rawPageFull, completeUnreliable, byKind)
}

// handleListSearchProviders is the listSearchProviders tool handler.
// It returns the deployment's live search providers, tagged with
// whether each is enabled for the given repository (via the
// repository_provider_settings table) and whether it is the
// configured default. An agent calls this before searchSources to
// discover which providers it can use — repos can disable
// individual providers (e.g. a strict scientific repo may disable
// Serper and keep only OpenAlex).
func (m *MCP) handleListSearchProviders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	if len(m.searchProviders) == 0 {
		return structuredResult(map[string]any{
			"providers":         []any{},
			"default_provider":  m.defaultSearchProvider,
			"total":             0,
		})
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoID, _, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}

	// Read per-repo enabled provider settings from the system DB.
	enabledRows, err := m.deps.Store.ListEnabledRepositoryProviderIDs(ctx, repoID)
	if err != nil {
		return mcp.NewToolResultError("failed to list repository provider settings"), nil
	}
	enabledSearch := make(map[string]bool, len(enabledRows))
	for _, r := range enabledRows {
		if r.ProviderKind == "search" {
			enabledSearch[r.ProviderID] = true
		}
	}

	// Human-readable names for the known providers.
	names := map[string]string{
		"serper":   "Serper (Google Search)",
		"openalex": "OpenAlex (Academic Works)",
		"registry": "OKT Knowledge Registry",
	}

	type providerOut struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Enabled     bool   `json:"enabled"`
		IsDefault   bool   `json:"is_default"`
	}
	// Build the list from live providers, sorted by id for stable output.
	ids := make([]string, 0, len(m.searchProviders))
	for k, p := range m.searchProviders {
		if p != nil {
			ids = append(ids, k)
		}
	}
	sort.Strings(ids)
	out := make([]providerOut, 0, len(ids))
	for _, id := range ids {
		name := id
		if n, ok := names[id]; ok && n != "" {
			name = n
		}
		out = append(out, providerOut{
			ID:        id,
			Name:      name,
			Enabled:   enabledSearch[id],
			IsDefault: id == m.defaultSearchProvider,
		})
	}
	return structuredResult(map[string]any{
		"providers":        out,
		"default_provider": m.defaultSearchProvider,
		"total":            len(out),
	})
}

// handleListReports is the listReports tool handler. It resolves the
// repo, checks report:read, then runs the paginated ListReportsByRepo
// query with optional search and status filters. Returns report
// metadata only (not the body); the caller uses getReport with a
// returned id to read the full annotated report.
func (m *MCP) handleListReports(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	search := req.GetString("search", "")
	status := req.GetString("status", "")
	limit := req.GetInt("limit", 50)
	if limit < 1 {
		limit = 1
	}
	if limit > MCPMCPMaxFactsCap {
		limit = MCPMCPMaxFactsCap
	}
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Reports, rbac.Actions.Read); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to read reports in this repository"), nil
	}

	queries := store.New(pool)
	reports, err := queries.ListReportsByRepo(ctx, store.ListReportsByRepoParams{
		RepositoryID: repoID,
		Column2:      search,
		Column3:      status,
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		return mcp.NewToolResultError("failed to list reports"), nil
	}
	total, err := queries.CountReportsByRepo(ctx, store.CountReportsByRepoParams{
		RepositoryID: repoID,
		Column2:      search,
		Column3:      status,
	})
	if err != nil {
		return mcp.NewToolResultError("failed to count reports"), nil
	}

	type reportOut struct {
		ID            string `json:"id"`
		Title         string `json:"title"`
		Topic         string `json:"topic,omitempty"`
		Status        string `json:"status"`
		ParentID      string `json:"parent_id,omitempty"`
		SentenceCount int    `json:"sentence_count,omitempty"`
		CreatedAt     string `json:"created_at"`
		UpdatedAt     string `json:"updated_at"`
	}
	out := make([]reportOut, 0, len(reports))
	for _, r := range reports {
		topic := ""
		if r.Topic != nil {
			topic = *r.Topic
		}
		sentenceCount := 0
		if r.SentenceCount != nil {
			sentenceCount = int(*r.SentenceCount)
		}
		parentID := ""
		if r.ParentID.Valid {
			parentID = r.ParentID.String()
		}
		out = append(out, reportOut{
			ID:            r.ID.String(),
			Title:         r.Title,
			Topic:         topic,
			Status:        r.Status,
			ParentID:      parentID,
			SentenceCount: sentenceCount,
			CreatedAt:     pgTimeToString(r.CreatedAt),
			UpdatedAt:     pgTimeToString(r.UpdatedAt),
		})
	}
	return structuredResult(map[string]any{
		"reports": out,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// handleSearchSources is the searchSources tool handler. It
// resolves the repo, checks source_provider:execute, picks the
// requested search provider (or the first registered when none is
// named), runs the search, and tags each hit with
// already_exists/existing_status via the shared TagExistingSources
// helper (best-effort). The returned url/doi fields are exactly what
// fetchAndProcessSource consumes, so the agent can chain
// searchSources -> fetchAndProcessSource.
func (m *MCP) handleSearchSources(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid := httputil.RequestUserID(ctx)
	if !uid.Valid {
		return mcp.NewToolResultError("no authenticated user on context"), nil
	}
	if len(m.searchProviders) == 0 {
		return mcp.NewToolResultError("search providers not configured"), nil
	}
	repoArg, err := req.RequireString("repository")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	providerName := req.GetString("provider", "")
	perPage := req.GetInt("per_page", 0)
	if perPage < 0 {
		perPage = 0
	}
	cursor := req.GetString("cursor", "")

	// Pick provider: explicit name, else the configured default
	// (cfg.Providers.Search.Provider, typically "serper"), else
	// the first registered provider sorted alphabetically for
	// determinism.
	var provider search.SearchProvider
	var chosenName string
	if providerName != "" {
		p, ok := m.searchProviders[providerName]
		if !ok || p == nil {
			return mcp.NewToolResultError("unknown search provider: " + providerName), nil
		}
		provider, chosenName = p, providerName
	} else if m.defaultSearchProvider != "" {
		p, ok := m.searchProviders[m.defaultSearchProvider]
		if ok && p != nil {
			provider, chosenName = p, m.defaultSearchProvider
		}
	}
	if provider == nil {
		names := make([]string, 0, len(m.searchProviders))
		for k, p := range m.searchProviders {
			if p != nil {
				names = append(names, k)
			}
		}
		if len(names) == 0 {
			return mcp.NewToolResultError("search providers not configured"), nil
		}
		sort.Strings(names)
		chosenName = names[0]
		provider = m.searchProviders[chosenName]
	}

	repoID, pool, err := m.resolveRepoPool(ctx, repoArg)
	if err != nil {
		return mcp.NewToolResultError("repository not found: " + err.Error()), nil
	}
	if ok, err := m.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.SourceProvider, rbac.Actions.Execute); err != nil {
		return mcp.NewToolResultError("rbac check failed: " + err.Error()), nil
	} else if !ok {
		return mcp.NewToolResultError("you do not have permission to use search providers in this repository"), nil
	}

	// Match the REST TestSearch timeout so a slow upstream doesn't
	// pin an MCP request open.
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := provider.Search(sctx, query, search.SearchOptions{
		PerPage: perPage,
		Cursor:  cursor,
	})
	if err != nil {
		return mcp.NewToolResultError("search failed: " + err.Error()), nil
	}

	// Best-effort already-exists tagging. Failures are logged and
	// skipped so search still returns results — mirrors the REST
	// handler's posture.
	if pool != nil && len(resp.Results) > 0 {
		if err := TagExistingSources(ctx, pool, repoID, resp.Results); err != nil {
			mcpLog("searchSources: already-exists tagging failed for repo %s: %v", repoID.String(), err)
		}
	}

	return structuredResult(map[string]any{
		"provider":     chosenName,
		"results":      resp.Results,
		"total":        resp.Total,
		"next_cursor":  resp.NextCursor,
	})
}

// mcpLog is the shared logger for MCP-handler best-effort warnings.
// It keeps mcp.go free of a direct log import while giving the
// searchSources handler a place to surface non-fatal tagging
// failures. Centralized so a future observability swap is one site.
func mcpLog(format string, args ...any) {
	log.Printf("mcp: "+format, args...)
}

// synthesisOut is the wire shape for the optional synthesis field
// returned by getConcept. It surfaces only the content + model; the
// covered_* id arrays and the row id are internal and omitted.
type synthesisOut struct {
	Content string `json:"content"`
	Model   string `json:"model,omitempty"`
}

// summaryOut is the wire shape for one summary slice returned by
// getConceptSummaries.
type summaryOut struct {
	ConceptID   string `json:"concept_id"`
	Context     string `json:"context"`
	SequenceNum int32  `json:"sequence_num"`
	IsComplete  bool   `json:"is_complete"`
	FactCount   int32  `json:"fact_count"`
	Content     string `json:"content"`
	Model       string `json:"model,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}

// relationOut is the wire shape for one related concept returned by
// getRelatedConcepts.
type relationOut struct {
	ConceptID       string `json:"concept_id"`
	CanonicalName   string `json:"canonical_name"`
	SharedFactCount int64  `json:"shared_fact_count"`
}

// ptrStr dereferences a *string, returning "" for nil.
func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// structuredResult builds a CallToolResult carrying both a JSON text
// blob (for backward-compat clients that only read `content`) and
// the same object as `structuredContent` (for clients that read the
// typed field). The mark3labs library marshals StructuredContent into
// the response's `structuredContent` and the text blob into
// `content[0].text`.
func structuredResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("failed to serialize result"), nil
	}
	return &mcp.CallToolResult{
		Content:            []mcp.Content{mcp.NewTextContent(string(b))},
		StructuredContent:  v,
	}, nil
}

// pgTimeToString formats a pgtype.Timestamptz as RFC3339. Invalid
// timestamps (NULL) return "" so the JSON stays clean.
func pgTimeToString(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format("2006-01-02T15:04:05Z07:00")
}

// pgTimeToRFC3339 formats a time.Time as RFC3339.
func pgTimeToRFC3339(t time.Time) string {
	return t.Format("2006-01-02T15:04:05Z07:00")
}

// pgTimeToRFC3339Ptr formats a *time.Time as RFC3339, returning "" for nil.
func pgTimeToRFC3339Ptr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return pgTimeToRFC3339(*t)
}

// ResolveRepoPool is the exported form of the per-call repository
// resolver the MCP handler uses. It is exported so the wiring layer
// (and tests) can build one without duplicating the cache lookups.
// The implementation lives in wiring.go where it closes over the
// registry + RepoDBCache + SlugCache the Handler already owns.
//
// The signature returns (*pgxpool.Pool, error) in addition to the
// UUID so callers don't have to re-resolve the pool after the UUID
// is known. A nil pool with a non-nil error means the repository
// doesn't exist or its database isn't registered.
type ResolveRepoPool func(ctx context.Context, repoIDOrSlug string) (pgtype.UUID, *pgxpool.Pool, error)

// ResolveRepoPoolFromCaches builds a ResolveRepoPool from the same
// Registry + RepoDBCache + SlugCache the wiring layer uses for the
// per-repo chi routes. It tries UUID first (via RepoDBCache) and
// falls back to slug (via SlugCache), then resolves the pool by
// database name from the registry. This keeps MCP tool resolution
// consistent with the REST API's /{repoID} routing.
func ResolveRepoPoolFromCaches(
	registry *dbpool.Registry,
	repoDBCache *appmw.RepoDBCache,
	slugCache *appmw.SlugCache,
) ResolveRepoPool {
	return func(ctx context.Context, repoIDOrSlug string) (pgtype.UUID, *pgxpool.Pool, error) {
		if repoIDOrSlug == "" {
			return pgtype.UUID{}, nil, errors.New("repository is required")
		}
		var repoID pgtype.UUID
		// Try UUID first.
		if err := repoID.Scan(repoIDOrSlug); err == nil {
			if _, err := repoDBCache.Get(ctx, repoID); err != nil {
				return pgtype.UUID{}, nil, fmt.Errorf("repository not found: %w", err)
			}
		} else {
			// Not a UUID — treat as slug.
			var err error
			repoID, err = slugCache.Get(ctx, repoIDOrSlug)
			if err != nil {
				return pgtype.UUID{}, nil, fmt.Errorf("repository not found: %w", err)
			}
		}
		dbName, err := repoDBCache.Get(ctx, repoID)
		if err != nil {
			return pgtype.UUID{}, nil, fmt.Errorf("resolving repository database: %w", err)
		}
		entry := registry.Get(dbName)
		if entry == nil || entry.Pool == nil {
			return pgtype.UUID{}, nil, fmt.Errorf("no pool registered for database %q", dbName)
		}
		return repoID, entry.Pool, nil
	}
}

// The chi import is used so the file declares its chi dependency for
// the wiring layer's route registration; keeping it here avoids an
// empty-import shim. It's referenced by ResolveRepoPoolFromCaches's
// callers that need URL param extraction.
var _ = chi.NewRouter
var _ = httputil.WriteJSON
var _ rbac.Permission